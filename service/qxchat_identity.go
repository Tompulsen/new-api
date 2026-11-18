package service

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const qxchatPreparePath = "/api/account-unification/prepare"
const qxchatCommitPath = "/api/account-unification/commit"
const qxchatProvisionPath = "/api/account-unification/provision"

var ErrQXChatUnavailable = common.ErrQXChatUnavailable

func QXChatIdentityEnabled() bool {
	return os.Getenv("QXCHAT_IDENTITY_ENABLED") == "1" && len(os.Getenv("QXCHAT_IDENTITY_SECRET")) >= 32 && os.Getenv("QXCHAT_IDENTITY_SOURCE_URL") != ""
}

// Preserve the identity API's existing error contract around the shared transport.
func callQXChatSource(ctx context.Context, path string, input, output any) error {
	if path != qxchatPreparePath && path != qxchatCommitPath && path != qxchatProvisionPath {
		return ErrQXChatUnavailable
	}
	err := common.CallQXChatSource(ctx, path, input, output)
	var failure *common.QXChatSourceError
	if errors.As(err, &failure) {
		if failure.Status == http.StatusConflict && path == qxchatProvisionPath && failure.Code == "binding_required" {
			return model.ErrQXChatIdentityConflict
		}
		if failure.Status == http.StatusUnauthorized || failure.Status == http.StatusForbidden || failure.Status == http.StatusConflict {
			return model.ErrInvalidCredentials
		}
		return ErrQXChatUnavailable
	}
	return err
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
		if err := callQXChatSource(ctx, qxchatProvisionPath, profile, &receipt); err != nil {
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

// Private import journals omit password hashes; activation always obtains a fresh signed source receipt.
type QXChatImportState struct {
	QXChatCommitRequest
	State          string `json:"state,omitempty"`
	Imported       bool   `json:"imported"`
	Authority      string `json:"authority,omitempty"`
	PasswordDigest string `json:"password_digest,omitempty"`
	TargetUsername string `json:"target_username"`
	TargetEmail    string `json:"target_email"`
}

func InspectQXChatImport(expected QXChatImportState) (*QXChatImportState, error) {
	var mapping model.QXChatIdentity
	if expected.NewAPIUserID <= 0 {
		return nil, model.ErrQXChatIdentityInactive
	}
	if err := model.DB.Where("source_id = ?", expected.SourceID).First(&mapping).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrQXChatIdentityInactive
		}
		return nil, err
	}
	if mapping.UserID != expected.NewAPIUserID || mapping.RequestID != expected.RequestID ||
		mapping.CredentialVersion != expected.CredentialVersion || mapping.AuthorityVersion != expected.AuthorityVersion {
		return nil, model.ErrQXChatIdentitySnapshotConflict
	}
	var user model.User
	if err := model.DB.First(&user, mapping.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrQXChatIdentityInactive
		}
		return nil, err
	}
	if mapping.State == model.QXChatIdentityActive {
		if user.Status != common.UserStatusEnabled {
			return nil, model.ErrQXChatIdentityInactive
		}
		expected.PasswordDigest = ""
	} else if mapping.State != model.QXChatIdentityPending || !mapping.Imported || user.Status != model.QXChatUserPendingStatus ||
		user.AuthVersion != 1 || !strings.HasPrefix(user.Password, model.QXChatImportPasswordPrefix+common.QXChatPasswordPrefix) {
		return nil, model.ErrQXChatIdentityInactive
	} else if expected.PasswordDigest == "" || expected.PasswordDigest != model.QXChatImportPasswordDigest(user.Password) {
		return nil, model.ErrQXChatIdentitySnapshotConflict
	} else {
		prefix := model.QXChatImportPasswordPrefix + common.QXChatPasswordPrefix
		if _, err := common.ImportQXChatPasswordHash(strings.TrimPrefix(user.Password, prefix)); err != nil {
			return nil, model.ErrQXChatIdentityInactive
		}
	}
	return &QXChatImportState{QXChatCommitRequest: expected.QXChatCommitRequest, State: mapping.State, Imported: mapping.Imported,
		PasswordDigest: expected.PasswordDigest, TargetUsername: user.Username, TargetEmail: user.Email}, nil
}

// The existing signed source commit is idempotent after a lost receipt or an interrupted target activation.
func ActivateQXChatImport(ctx context.Context, expected QXChatImportState, apply bool) (*QXChatImportState, error) {
	state, err := InspectQXChatImport(expected)
	if err != nil {
		return nil, err
	}
	if !state.Imported {
		return nil, model.ErrQXChatIdentityConflict
	}
	if !apply {
		return state, nil
	}
	if state.State == model.QXChatIdentityActive {
		return state, model.PublishUserAuthCache(state.NewAPIUserID)
	}
	identity := model.QXChatIdentity{SourceID: state.SourceID, UserID: state.NewAPIUserID, RequestID: state.RequestID,
		CredentialVersion: state.CredentialVersion, AuthorityVersion: state.AuthorityVersion, State: state.State, Imported: true}
	if err := commitQXChatSource(ctx, &identity); err != nil {
		return nil, err
	}
	user, err := model.ActivateQXChatImportIdentity(identity, state.PasswordDigest)
	if err != nil {
		return nil, err
	}
	if err := model.PublishUserAuthCache(user.Id); err != nil {
		return nil, err
	}
	return InspectQXChatImport(expected)
}

func commitQXChatSource(ctx context.Context, identity *model.QXChatIdentity) error {
	request := QXChatCommitRequest{identity.RequestID, identity.SourceID, identity.UserID, identity.CredentialVersion, identity.AuthorityVersion}
	var receipt struct {
		RequestID        string `json:"request_id"`
		SourceID         string `json:"source_id"`
		NewAPIUserID     int    `json:"newapi_user_id"`
		Authority        string `json:"authority"`
		AuthorityVersion int64  `json:"authority_version"`
	}
	if err := callQXChatSource(ctx, qxchatCommitPath, request, &receipt); err != nil {
		return err
	}
	if receipt.RequestID != request.RequestID || receipt.SourceID != request.SourceID || receipt.NewAPIUserID != request.NewAPIUserID ||
		receipt.Authority != "newapi" || receipt.AuthorityVersion != request.AuthorityVersion {
		return ErrQXChatUnavailable
	}
	return nil
}

func commitQXChatIdentity(ctx context.Context, identity *model.QXChatIdentity, target *AuthIdentity) (*model.User, error) {
	if err := commitQXChatSource(ctx, identity); err != nil {
		return nil, err
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
			if !mapping.Imported || user.Status != common.UserStatusEnabled || !common.ValidatePasswordAndHash(password, user.Password) {
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
