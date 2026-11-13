package controller

import (
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

var communitySSOOpaque = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func communitySSOOrigin() string {
	return ssoOrigin("COMMUNITY_SSO_ORIGIN")
}

func ssoOrigin(environment string) string {
	return ssoOriginValue(os.Getenv(environment))
}

func ssoOriginValue(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.ForceQuery || strings.Contains(raw, "#") {
		return ""
	}
	origin, err := common.NormalizeOrigin(raw)
	if err != nil || !strings.HasPrefix(origin, "https://") {
		return ""
	}
	return origin
}

func ssoOrigins(prefix string) []string {
	origins := []string{}
	for raw := range strings.SplitSeq(os.Getenv(prefix+"_ORIGIN")+","+os.Getenv(prefix+"_ALLOWED_ORIGINS"), ",") {
		origin := ssoOriginValue(raw)
		if origin != "" && !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	return origins
}

func communitySSOError(c *gin.Context, status int, reason string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(status, gin.H{"error": reason})
}

func CommunitySSOConfig(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	origin := communitySSOOrigin()
	c.JSON(http.StatusOK, gin.H{"enabled": len(ssoOrigins("COMMUNITY_SSO")) > 0, "community_origin": origin, "studio_origin": ssoOrigin("STUDIO_SSO_ORIGIN"), "community_origins": ssoOrigins("COMMUNITY_SSO"), "studio_origins": ssoOrigins("STUDIO_SSO")})
}

func CommunitySSOAuthorize(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request struct {
		ClientID     string `json:"client_id"`
		RedirectURI  string `json:"redirect_uri"`
		ResponseType string `json:"response_type"`
		Scope        string `json:"scope"`
		State        string `json:"state"`
		Challenge    string `json:"code_challenge"`
		Method       string `json:"code_challenge_method"`
	}
	if c.ShouldBindJSON(&request) != nil {
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	origin := request.ClientID
	if !slices.Contains(ssoOrigins("COMMUNITY_SSO"), origin) ||
		request.RedirectURI != origin+"/api/newapi/callback" || request.ResponseType != "code" ||
		request.Scope != "identity" || request.Method != "S256" || !communitySSOOpaque.MatchString(request.State) || !communitySSOOpaque.MatchString(request.Challenge) {
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok || strings.HasPrefix(identity.SessionID, "pat:") {
		communitySSOError(c, http.StatusUnauthorized, "login_required")
		return
	}
	code, err := model.CreateCommunityGrant(model.AuthSessionIdentity(identity), origin, request.Challenge)
	if err != nil {
		communitySSOError(c, http.StatusUnauthorized, "login_required")
		return
	}
	callback := origin + "/api/newapi/callback?" + url.Values{"code": {code}, "state": {request.State}}.Encode()
	c.JSON(http.StatusOK, gin.H{"redirect_uri": callback})
}

func CommunitySSOToken(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	var request struct {
		GrantType   string `json:"grant_type"`
		ClientID    string `json:"client_id"`
		RedirectURI string `json:"redirect_uri"`
		Code        string `json:"code"`
		Verifier    string `json:"code_verifier"`
	}
	if c.ShouldBindJSON(&request) != nil {
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	origin := request.ClientID
	if !slices.Contains(ssoOrigins("COMMUNITY_SSO"), origin) || request.GrantType != "authorization_code" || request.RedirectURI != origin+"/api/newapi/callback" {
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	token, err := model.ExchangeCommunityGrant(request.Code, origin, request.Verifier)
	if err != nil {
		communitySSOError(c, http.StatusBadRequest, "invalid_grant")
		return
	}
	c.JSON(http.StatusOK, gin.H{"access_token": token, "token_type": "Bearer", "expires_in": 60, "scope": "identity"})
}

func CommunitySSOUserinfo(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		communitySSOError(c, http.StatusUnauthorized, "invalid_token")
		return
	}
	// Failed audience checks roll back consumption; only an explicitly allowed grant can succeed.
	for _, origin := range ssoOrigins("COMMUNITY_SSO") {
		identity, err := model.ConsumeCommunityIdentity(strings.TrimPrefix(auth, "Bearer "), origin)
		if err == nil {
			c.JSON(http.StatusOK, identity)
			return
		}
	}
	communitySSOError(c, http.StatusUnauthorized, "invalid_token")
}
