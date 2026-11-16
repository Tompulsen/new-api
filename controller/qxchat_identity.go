package controller

import (
	"errors"
	"net/http"
	"strings"

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
	if !service.QXChatIdentityEnabled() { common.ApiSuccess(c, gin.H{"enabled": false, "state": "unbound"}); return }
	var mapping model.QXChatIdentity
	err := model.DB.Where("user_id = ?", c.GetInt("id")).First(&mapping).Error
	if errors.Is(err, gorm.ErrRecordNotFound) { common.ApiSuccess(c, gin.H{"enabled": true, "state": "unbound"}); return }
	if err != nil { writeQXChatIdentityError(c, service.ErrQXChatUnavailable); return }
	common.ApiSuccess(c, gin.H{"enabled": true, "state": mapping.State, "username": mapping.SourceUsername})
}

func qxchatLoginPassword(request LoginRequest) (string, error) {
	if !common.PasswordLoginEncryptionEnabled { return request.Password, nil }
	if request.PasswordEncrypted == "" || request.EncryptionKeyID == "" { return "", model.ErrInvalidCredentials }
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
	if !service.QXChatIdentityEnabled() { c.Status(http.StatusNotFound); return }
	if !common.PasswordLoginEnabled { common.ApiErrorT(c, "Password login has been disabled by administrator"); return }
	var request LoginRequest
	if common.DecodeJson(c.Request.Body, &request) != nil { common.ApiErrorT(c, "Invalid parameters"); return }
	password, err := qxchatLoginPassword(request)
	if err != nil { writeQXChatIdentityError(c, err); return }
	user, err := service.LoginQXChatIdentity(c.Request.Context(), request.Username, password)
	if err != nil { writeQXChatIdentityError(c, err); return }
	c.Set("login_method", "qxchat")
	setupLogin(user, nil, c)
}

func QXChatIdentityBind(c *gin.Context) {
	if !service.QXChatIdentityEnabled() { c.Status(http.StatusNotFound); return }
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok || strings.HasPrefix(identity.SessionID, "pat:") { c.Status(http.StatusUnauthorized); return }
	var request LoginRequest
	if common.DecodeJson(c.Request.Body, &request) != nil { common.ApiErrorT(c, "Invalid parameters"); return }
	identifier := strings.ToLower(strings.TrimSpace(request.Username))
	encoded, err := common.Marshal(service.AccountBindingContext{Provider: "qxchat", Code: identifier})
	if err != nil { writeSecurityOperationError(c, err); return }
	authorization := middleware.RequireSecurityProof(c, service.VerificationOperation{Scope: service.VerificationScopeAccountBind, Context: encoded})
	if authorization == nil { return }
	password, err := qxchatLoginPassword(request)
	if err != nil { writeQXChatIdentityError(c, err); return }
	mapping, err := service.BindQXChatIdentity(c.Request.Context(), identity, authorization, identifier, password)
	if err != nil { writeQXChatIdentityError(c, err); return }
	recordUserSecurityAudit(c, identity.UserID, "user.binding_bind", map[string]any{"provider": "qxchat", "success": true})
	common.ApiSuccess(c, gin.H{"source_id": mapping.SourceID, "newapi_user_id": mapping.UserID, "state": mapping.State, "notification_warning": false})
}
