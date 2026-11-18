package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func QXChatIdentityConfig(c *gin.Context) {
	common.ApiSuccess(c, gin.H{"enabled": service.QXChatIdentityEnabled()})
}

func QXChatIdentityStatus(c *gin.Context) {
	if !service.QXChatIdentityEnabled() {
		common.ApiSuccess(c, gin.H{"enabled": false, "state": "unbound"})
		return
	}
	var mapping model.QXChatIdentity
	err := model.DB.Where("user_id = ?", c.GetInt("id")).First(&mapping).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiSuccess(c, gin.H{"enabled": true, "state": "unbound", "profile_kind": "independent"})
		return
	}
	if err != nil {
		writeQXChatIdentityError(c, service.ErrQXChatUnavailable)
		return
	}
	state := mapping.State
	if state == model.QXChatIdentityProvisioning {
		state = model.QXChatIdentityPending
	}
	profileKind := "original"
	// Native reservations use a generated digest; original profiles retain their credential fingerprint, even without email.
	registrationVersion := sha256.Sum256([]byte(mapping.SourceID + "\n" + mapping.RequestID))
	if !mapping.Imported && mapping.CredentialVersion == hex.EncodeToString(registrationVersion[:]) {
		profileKind = "independent"
	}
	common.ApiSuccess(c, gin.H{"enabled": true, "state": state, "username": mapping.SourceUsername, "profile_kind": profileKind})
}

func qxchatLoginPassword(request LoginRequest) (string, error) {
	if !common.PasswordLoginEncryptionEnabled {
		return request.Password, nil
	}
	if request.PasswordEncrypted == "" || request.EncryptionKeyID == "" {
		return "", model.ErrInvalidCredentials
	}
	return common.DecryptPassword(request.PasswordEncrypted, request.EncryptionKeyID)
}

func writeQXChatIdentityError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrQXChatUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "QXCHAT_UNAVAILABLE", "message": service.ErrQXChatUnavailable.Error()})
	case errors.Is(err, model.ErrQXChatIdentityConflict):
		c.JSON(http.StatusConflict, gin.H{"success": false, "code": "QXCHAT_BIND_REQUIRED", "message": "Sign in to your existing New API account and connect QXChat in account security."})
	default:
		common.ApiErrorT(c, "Username or password is incorrect, or user has been banned")
	}
}

func QXChatIdentityLogin(c *gin.Context) {
	if !service.QXChatIdentityEnabled() {
		c.Status(http.StatusNotFound)
		return
	}
	if !common.PasswordLoginEnabled {
		common.ApiErrorT(c, "Password login has been disabled by administrator")
		return
	}
	var request LoginRequest
	if common.DecodeJson(c.Request.Body, &request) != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	password, err := qxchatLoginPassword(request)
	if err != nil {
		writeQXChatIdentityError(c, err)
		return
	}
	mapping, lookupErr := model.FindQXChatIdentity(request.Username)
	if lookupErr != nil || mapping.State != model.QXChatIdentityActive {
		// The explicit legacy endpoint may finish an existing source migration.
		// Once active, it uses exactly the same account confirmation as /login.
		if _, err := service.LoginQXChatIdentity(c.Request.Context(), request.Username, password); err != nil {
			writeQXChatIdentityError(c, err)
			return
		}
	}
	c.Set("login_method", "qxchat")
	completePasswordLogin(c, request.Username, password)
}

func QXChatIdentityBind(c *gin.Context) {
	if !service.QXChatIdentityEnabled() {
		c.Status(http.StatusNotFound)
		return
	}
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok || strings.HasPrefix(identity.SessionID, "pat:") {
		c.Status(http.StatusUnauthorized)
		return
	}
	var request LoginRequest
	if common.DecodeJson(c.Request.Body, &request) != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	identifier := strings.ToLower(strings.TrimSpace(request.Username))
	encoded, err := common.Marshal(service.AccountBindingContext{Provider: "qxchat", Code: identifier})
	if err != nil {
		writeSecurityOperationError(c, err)
		return
	}
	authorization := middleware.RequireSecurityProof(c, service.VerificationOperation{Scope: service.VerificationScopeAccountBind, Context: encoded})
	if authorization == nil {
		return
	}
	password, err := qxchatLoginPassword(request)
	if err != nil {
		writeQXChatIdentityError(c, err)
		return
	}
	mapping, err := service.BindQXChatIdentity(c.Request.Context(), identity, authorization, identifier, password)
	if err != nil {
		writeQXChatIdentityError(c, err)
		return
	}
	recordUserSecurityAudit(c, identity.UserID, "user.binding_bind", map[string]any{"provider": "qxchat", "success": true})
	common.ApiSuccess(c, gin.H{"source_id": mapping.SourceID, "newapi_user_id": mapping.UserID, "state": mapping.State, "notification_warning": false})
}

var qxchatSessionNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)

func readQXChatSessionRequest(c *gin.Context) ([]byte, error) {
	secret := os.Getenv("QXCHAT_SESSION_SECRET")
	if !service.QXChatIdentityEnabled() || len(secret) < 32 || len(ssoOrigins("QXCHAT_SSO")) == 0 || c.Request.URL.RawQuery != "" || c.GetHeader("Content-Encoding") != "" {
		return nil, model.ErrAuthFlowInvalid
	}
	stamp, nonce := c.GetHeader("X-QXChat-Timestamp"), c.GetHeader("X-QXChat-Nonce")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != stamp || seconds < time.Now().Unix()-60 || seconds > time.Now().Unix()+60 || !qxchatSessionNonce.MatchString(nonce) {
		return nil, model.ErrAuthFlowInvalid
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16384))
	if err != nil {
		return nil, model.ErrAuthFlowInvalid
	}
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{"qxchat-session-v1", c.Request.Method, c.Request.URL.Path, stamp, nonce, hex.EncodeToString(digest[:])}, "\n")
	if !hmac.Equal([]byte(c.GetHeader("X-QXChat-Signature")), []byte(common.GenerateHMACWithKey([]byte(secret), canonical))) {
		return nil, model.ErrAuthFlowInvalid
	}
	if err := model.ClaimExternalAuthAssertion("qxchat_session_nonce", nonce, time.Unix(seconds+120, 0)); err != nil {
		return nil, err
	}
	return body, nil
}

func writeQXChatSessionResponse(c *gin.Context, status int, result any) {
	body, err := common.Marshal(result)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{"qxchat-session-response-v1", c.GetHeader("X-QXChat-Nonce"), strconv.Itoa(status), hex.EncodeToString(digest[:])}, "\n")
	c.Header("X-QXChat-Signature", common.GenerateHMACWithKey([]byte(os.Getenv("QXCHAT_SESSION_SECRET")), canonical))
	c.Header("Cache-Control", "no-store")
	c.Data(status, "application/json", body)
}

func QXChatSessionExchange(c *gin.Context) {
	body, err := readQXChatSessionRequest(c)
	if err != nil {
		writeQXChatSessionResponse(c, http.StatusUnauthorized, gin.H{"active": false})
		return
	}
	var request struct {
		AccessToken string `json:"access_token"`
	}
	if common.Unmarshal(body, &request) != nil {
		writeQXChatSessionResponse(c, http.StatusBadRequest, gin.H{"active": false})
		return
	}
	// Failed audience checks roll back consumption, as in CommunitySSOUserinfo.
	for _, origin := range ssoOrigins("QXCHAT_SSO") {
		identity, err := service.ExchangeQXChatIdentitySession(c.Request.Context(), request.AccessToken, origin)
		if err == nil {
			writeQXChatSessionResponse(c, http.StatusOK, identity)
			return
		}
	}
	writeQXChatSessionResponse(c, http.StatusUnauthorized, gin.H{"active": false})
}

func QXChatSessionValidate(c *gin.Context) {
	body, err := readQXChatSessionRequest(c)
	if err != nil {
		writeQXChatSessionResponse(c, http.StatusUnauthorized, gin.H{"active": false})
		return
	}
	var request struct {
		SessionProof   string `json:"session_proof"`
		SourceID       string `json:"source_id"`
		UserID         int    `json:"newapi_user_id"`
		IncludeAccount bool   `json:"include_account"`
	}
	if common.Unmarshal(body, &request) != nil {
		writeQXChatSessionResponse(c, http.StatusBadRequest, gin.H{"active": false})
		return
	}
	for _, origin := range ssoOrigins("QXCHAT_SSO") {
		identity, err := model.ValidateQXChatSession(request.SessionProof, origin, request.SourceID, request.UserID)
		if err == nil {
			if request.IncludeAccount {
				account, err := model.GetUserLoginIdentifiers(identity.UserID)
				if err != nil {
					writeQXChatSessionResponse(c, http.StatusServiceUnavailable, gin.H{"active": false})
					return
				}
				identity.AccountUsername, identity.AccountEmail = &account.Username, &account.Email
				identity.LoginIdentifiers = &account.LoginIdentifiers
			}
			writeQXChatSessionResponse(c, http.StatusOK, identity)
			return
		}
	}
	writeQXChatSessionResponse(c, http.StatusUnauthorized, gin.H{"active": false})
}
