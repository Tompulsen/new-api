package controller

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func StudioSSOAuthorize(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request struct {
		ClientID     string `json:"client_id"`
		RedirectURI  string `json:"redirect_uri"`
		ResponseType string `json:"response_type"`
		Scope        string `json:"scope"`
		State        string `json:"state"`
		Challenge    string `json:"code_challenge"`
		Method       string `json:"code_challenge_method"`
		Subject      string `json:"expected_subject"`
	}
	origin := ssoOrigin("STUDIO_SSO_ORIGIN")
	if origin == "" || c.ShouldBindJSON(&request) != nil || request.ClientID != origin || request.RedirectURI != origin+"/api/new-api/community/callback" || request.ResponseType != "code" || request.Scope != "chat_session" || request.Method != "S256" || !communitySSOOpaque.MatchString(request.State) || !communitySSOOpaque.MatchString(request.Challenge) {
		communitySSOError(c, 400, "invalid_request")
		return
	}
	expected, err := strconv.Atoi(request.Subject)
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if err != nil || !ok || expected != identity.UserID || expected <= 0 || strings.HasPrefix(identity.SessionID, "pat:") {
		communitySSOError(c, 403, "account_mismatch")
		return
	}
	code, err := model.CreateStudioGrant(model.AuthSessionIdentity(identity), origin, request.Challenge, expected)
	if err != nil {
		communitySSOError(c, 401, "login_required")
		return
	}
	c.JSON(200, gin.H{"redirect_uri": origin + "/api/new-api/community/callback?" + url.Values{"code": {code}, "state": {request.State}}.Encode()})
}

func StudioSSOToken(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request struct {
		GrantType   string `json:"grant_type"`
		ClientID    string `json:"client_id"`
		RedirectURI string `json:"redirect_uri"`
		Code        string `json:"code"`
		Verifier    string `json:"code_verifier"`
		Subject     string `json:"expected_subject"`
	}
	origin := ssoOrigin("STUDIO_SSO_ORIGIN")
	if origin == "" || c.ShouldBindJSON(&request) != nil || request.GrantType != "authorization_code" || request.ClientID != origin || request.RedirectURI != origin+"/api/new-api/community/callback" {
		communitySSOError(c, 400, "invalid_request")
		return
	}
	expected, err := strconv.Atoi(request.Subject)
	if err != nil || expected <= 0 {
		communitySSOError(c, 400, "invalid_request")
		return
	}
	bundle, user, err := service.ExchangeStudioLogin(request.Code, origin, request.Verifier, expected, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		communitySSOError(c, 400, "invalid_grant")
		return
	}
	service.WriteRefreshCookie(c, bundle.RefreshToken)
	data := authRotationData(bundle)
	data["user"] = gin.H{"id": user.Id, "username": user.Username, "display_name": user.DisplayName, "quota": user.Quota, "used_quota": user.UsedQuota, "request_count": user.RequestCount, "group": user.Group}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}
