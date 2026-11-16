package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const qxchatPreparePath = "/api/account-unification/prepare"
const qxchatCommitPath = "/api/account-unification/commit"

var ErrQXChatUnavailable = errors.New("QXChat account connection is temporarily unavailable. Please try again.")

func QXChatIdentityEnabled() bool {
	return os.Getenv("QXCHAT_IDENTITY_ENABLED") == "1" && len(os.Getenv("QXCHAT_IDENTITY_SECRET")) >= 32 && os.Getenv("QXCHAT_IDENTITY_SOURCE_URL") != ""
}

// The configured source is operator-owned; user input never chooses a URL.
func callQXChatSource(ctx context.Context, path string, input, output any) error {
	if !QXChatIdentityEnabled() || (path != qxchatPreparePath && path != qxchatCommitPath) {
		return ErrQXChatUnavailable
	}
	base, err := url.Parse(os.Getenv("QXCHAT_IDENTITY_SOURCE_URL"))
	if err != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") || base.Host == "" {
		return ErrQXChatUnavailable
	}
	loopback := net.ParseIP(base.Hostname())
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback != nil && loopback.IsLoopback()) {
		return ErrQXChatUnavailable
	}
	body, err := common.Marshal(input)
	if err != nil {
		return err
	}
	base.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return ErrQXChatUnavailable
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nonceBytes)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{"qxchat-identity-v1", http.MethodPost, path, stamp, nonce, hex.EncodeToString(digest[:])}, "\n")
	secret := []byte(os.Getenv("QXCHAT_IDENTITY_SECRET"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-QXChat-Timestamp", stamp)
	req.Header.Set("X-QXChat-Nonce", nonce)
	req.Header.Set("X-QXChat-Signature", common.GenerateHMACWithKey(secret, canonical))
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return ErrQXChatUnavailable
	}
	defer res.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(res.Body, 32769))
	if err != nil || len(responseBody) > 32768 {
		return ErrQXChatUnavailable
	}
	responseDigest := sha256.Sum256(responseBody)
	responseCanonical := strings.Join([]string{"qxchat-identity-response-v1", nonce, strconv.Itoa(res.StatusCode), hex.EncodeToString(responseDigest[:])}, "\n")
	expected := common.GenerateHMACWithKey(secret, responseCanonical)
	if !hmac.Equal([]byte(res.Header.Get("X-QXChat-Signature")), []byte(expected)) {
		return ErrQXChatUnavailable
	}
	if res.StatusCode == http.StatusConflict && path == "/api/account-unification/provision" {
		var failure struct {
			Error string `json:"error"`
		}
		if common.Unmarshal(responseBody, &failure) == nil && failure.Error == "binding_required" {
			return model.ErrQXChatIdentityConflict
		}
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusConflict {
		return model.ErrInvalidCredentials
	}
	if res.StatusCode != http.StatusOK || common.Unmarshal(responseBody, output) != nil {
		return ErrQXChatUnavailable
	}
	return nil
}

func ExchangeQXChatIdentitySession(ctx context.Context, token, origin string) (*model.QXChatSessionIdentity, error) {
	mapping, profile, err := model.PrepareQXChatRegistration(token, origin)
	if err != nil {
		return nil, err
	}
	if profile != nil {
		var receipt struct {
			RequestID        string `json:"request_id"`
			SourceID         string `json:"source_id"`
			NewAPIUserID     int    `json:"newapi_user_id"`
			Authority        string `json:"authority"`
			AuthorityVersion int64  `json:"authority_version"`
		}
		if err := callQXChatSource(ctx, "/api/account-unification/provision", profile, &receipt); err != nil {
			if errors.Is(err, model.ErrQXChatIdentityConflict) {
				if releaseErr := model.ReleaseQXChatRegistration(*mapping); releaseErr != nil {
					return nil, releaseErr
				}
			}
			return nil, err
		}
		if receipt.RequestID != mapping.RequestID || receipt.SourceID != mapping.SourceID || receipt.NewAPIUserID != mapping.UserID ||
			receipt.Authority != "newapi" || receipt.AuthorityVersion != mapping.AuthorityVersion {
			return nil, ErrQXChatUnavailable
		}
		if err := model.FinishQXChatRegistration(*mapping, token, origin); err != nil {
			return nil, err
		}
	}
	return model.ExchangeQXChatSession(token, origin)
}

type QXChatCommitRequest struct {
	RequestID         string `json:"request_id"`
	SourceID          string `json:"source_id"`
	NewAPIUserID      int    `json:"newapi_user_id"`
	CredentialVersion string `json:"credential_version"`
	AuthorityVersion  int64  `json:"authority_version"`
}

func commitQXChatIdentity(ctx context.Context, identity *model.QXChatIdentity, target *AuthIdentity) (*model.User, error) {
	request := QXChatCommitRequest{identity.RequestID, identity.SourceID, identity.UserID, identity.CredentialVersion, identity.AuthorityVersion}
	var receipt struct {
		RequestID        string `json:"request_id"`
		SourceID         string `json:"source_id"`
		NewAPIUserID     int    `json:"newapi_user_id"`
		Authority        string `json:"authority"`
		AuthorityVersion int64  `json:"authority_version"`
	}
	if err := callQXChatSource(ctx, qxchatCommitPath, request, &receipt); err != nil {
		return nil, err
	}
	if receipt.RequestID != request.RequestID || receipt.SourceID != request.SourceID || receipt.NewAPIUserID != request.NewAPIUserID ||
		receipt.Authority != "newapi" || receipt.AuthorityVersion != request.AuthorityVersion {
		return nil, ErrQXChatUnavailable
	}
	user, err := model.ActivateQXChatIdentity(*identity, target)
	if err != nil {
		return nil, err
	}
	if err := model.PublishUserAuthCache(user.Id); err != nil {
		return nil, err
	}
	return user, nil
}

func LoginQXChatIdentity(ctx context.Context, identifier, password string) (*model.User, error) {
	if !QXChatIdentityEnabled() {
		return nil, ErrQXChatUnavailable
	}
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if identifier == "" || len(identifier) > 255 || password == "" || len(password) > 512 {
		return nil, model.ErrInvalidCredentials
	}
	mapping, err := model.FindQXChatIdentity(identifier)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err == nil {
		var user model.User
		if err := model.DB.First(&user, mapping.UserID).Error; err != nil {
			return nil, model.ErrInvalidCredentials
		}
		if mapping.State == model.QXChatIdentityActive {
			if user.Status != common.UserStatusEnabled || !common.ValidatePasswordAndHash(password, user.Password) {
				return nil, model.ErrInvalidCredentials
			}
			return &user, nil
		}
		if !mapping.Imported {
			return nil, model.ErrQXChatIdentityConflict
		}
		// Resume after a lost source receipt without re-enabling the old credential authority.
		if user.Status == model.QXChatUserPendingStatus && common.ValidatePasswordAndHash(password, user.Password) {
			user, err := commitQXChatIdentity(ctx, mapping, nil)
			if err == nil {
				return user, nil
			}
			if !errors.Is(err, model.ErrInvalidCredentials) {
				return nil, err
			}
		}
	}
	var profile model.QXChatSourceProfile
	requestID := uuid.NewString()
	if mapping != nil {
		requestID = mapping.RequestID
	}
	if err := callQXChatSource(ctx, qxchatPreparePath, struct {
		RequestID  string `json:"request_id"`
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}{requestID, identifier, password}, &profile); err != nil {
		return nil, err
	}
	if profile.RequestID != requestID {
		return nil, ErrQXChatUnavailable
	}
	mapping, _, err = model.PrepareQXChatIdentity(profile, nil)
	if err != nil {
		return nil, err
	}
	return commitQXChatIdentity(ctx, mapping, nil)
}

func BindQXChatIdentity(ctx context.Context, identity AuthIdentity, authorization *model.AuthFlowAuthorization, identifier, password string) (*model.QXChatIdentity, error) {
	if !QXChatIdentityEnabled() {
		return nil, ErrQXChatUnavailable
	}
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	encoded, err := common.Marshal(AccountBindingContext{Provider: "qxchat", Code: identifier})
	if err != nil {
		return nil, err
	}
	operation := VerificationOperation{Scope: VerificationScopeAccountBind, Context: encoded}
	if err := ValidateFlowAuthorization(identity, operation, authorization); err != nil {
		return nil, err
	}
	if strings.HasPrefix(identity.SessionID, "pat:") || identifier == "" || len(identifier) > 255 || password == "" || len(password) > 512 {
		return nil, model.ErrInvalidCredentials
	}
	mapping, err := model.FindQXChatIdentity(identifier)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err == nil && mapping.UserID == identity.UserID {
		if mapping.State == model.QXChatIdentityActive {
			return model.ResolveQXChatIdentity(mapping.SourceID)
		}
		if _, err := commitQXChatIdentity(ctx, mapping, &identity); err == nil {
			return model.ResolveQXChatIdentity(mapping.SourceID)
		} else if !errors.Is(err, model.ErrInvalidCredentials) {
			return nil, err
		}
	}
	var profile model.QXChatSourceProfile
	requestID := uuid.NewString()
	if mapping != nil && mapping.UserID == identity.UserID {
		requestID = mapping.RequestID
	}
	if err := callQXChatSource(ctx, qxchatPreparePath, struct {
		RequestID  string `json:"request_id"`
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}{requestID, identifier, password}, &profile); err != nil {
		return nil, err
	}
	if profile.RequestID != requestID {
		return nil, ErrQXChatUnavailable
	}
	if err := ValidateFlowAuthorization(identity, operation, authorization); err != nil {
		return nil, err
	}
	mapping, _, err = model.PrepareQXChatIdentity(profile, &identity)
	if err != nil {
		return nil, err
	}
	if _, err := commitQXChatIdentity(ctx, mapping, &identity); err != nil {
		return nil, err
	}
	return model.ResolveQXChatIdentity(mapping.SourceID)
}
