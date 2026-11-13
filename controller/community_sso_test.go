package controller

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const communityTestOrigin = "https://community.example"
const communityTestVerifier = "valid_verifier_for_community_01234567890123456789"

func setupCommunitySSOTest(t *testing.T) (*model.User, service.AuthIdentity) {
	t.Helper()
	user, identity := setupSecurityEnrollmentTest(t)
	logs, err := model.LOG_DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = logs.Close() })
	return user, identity
}

func communityTestRequest() map[string]string {
	digest := sha256.Sum256([]byte(communityTestVerifier))
	return map[string]string{"client_id": communityTestOrigin, "redirect_uri": communityTestOrigin + "/api/newapi/callback", "response_type": "code", "scope": "identity", "state": strings.Repeat("s", 43), "code_challenge": base64.RawURLEncoding.EncodeToString(digest[:]), "code_challenge_method": "S256"}
}

func communityTestCall(t *testing.T, body any, identity service.AuthIdentity, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	return securityEnrollmentRequest("POST", "/", string(b), "", identity, handler)
}

func communityTestGrant(t *testing.T, identity service.AuthIdentity) string {
	t.Helper()
	r := communityTestCall(t, communityTestRequest(), identity, CommunitySSOAuthorize)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	var result map[string]string
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &result))
	u, err := url.Parse(result["redirect_uri"])
	require.NoError(t, err)
	require.Equal(t, communityTestOrigin+"/api/newapi/callback", u.Scheme+"://"+u.Host+u.Path)
	require.Equal(t, strings.Repeat("s", 43), u.Query().Get("state"))
	return u.Query().Get("code")
}

func TestCommunitySSOIdentityOnlyAndReplay(t *testing.T) {
	user, identity := setupCommunitySSOTest(t)
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	require.NoError(t, model.DB.Model(user).Updates(map[string]any{"quota": 123456, "used_quota": 789, "access_token": strings.Repeat("k", 32)}).Error)
	code := communityTestGrant(t, identity)
	request := map[string]string{"grant_type": "authorization_code", "client_id": communityTestOrigin, "redirect_uri": communityTestOrigin + "/api/newapi/callback", "code": code, "code_verifier": communityTestVerifier}
	r := communityTestCall(t, request, identity, CommunitySSOToken)
	require.Equal(t, 200, r.Code, r.Body.String())
	var result map[string]any
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &result))
	require.Equal(t, "identity", result["scope"])
	token := result["access_token"].(string)
	require.Equal(t, 400, communityTestCall(t, request, identity, CommunitySSOToken).Code)
	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Request.Header.Set("Authorization", "Bearer "+token)
		CommunitySSOUserinfo(c)
		if i == 0 {
			require.Equal(t, 200, response.Code)
			var claims map[string]string
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &claims))
			require.Len(t, claims, 3)
			require.Equal(t, strconv.Itoa(user.Id), claims["sub"])
			require.Equal(t, user.Username, claims["preferred_username"])
		} else {
			require.Equal(t, 401, response.Code)
		}
	}
	var stored model.User
	require.NoError(t, model.DB.First(&stored, user.Id).Error)
	require.Equal(t, 123456, stored.Quota)
	require.Equal(t, 789, stored.UsedQuota)
	require.NotNil(t, stored.AccessToken)
	require.Equal(t, strings.Repeat("k", 32), *stored.AccessToken)
}

func TestCommunitySSORejectsInvalidAuthorization(t *testing.T) {
	_, identity := setupCommunitySSOTest(t)
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	for key, value := range map[string]string{"client_id": "https://evil.example", "redirect_uri": communityTestOrigin + "/elsewhere", "response_type": "token", "scope": "models", "state": "short", "code_challenge": "short", "code_challenge_method": "plain"} {
		t.Run(key, func(t *testing.T) {
			body := communityTestRequest()
			body[key] = value
			require.Equal(t, 400, communityTestCall(t, body, identity, CommunitySSOAuthorize).Code)
		})
	}
	pat := identity
	pat.SessionID = "pat:123"
	require.Equal(t, 401, communityTestCall(t, communityTestRequest(), pat, CommunitySSOAuthorize).Code)
	t.Setenv("COMMUNITY_SSO_ORIGIN", "http://unsafe.example")
	require.Equal(t, 400, communityTestCall(t, communityTestRequest(), identity, CommunitySSOAuthorize).Code)
}

func TestCommunitySSOWrongPKCEAndClientDoNotConsume(t *testing.T) {
	_, identity := setupCommunitySSOTest(t)
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	code := communityTestGrant(t, identity)
	_, err := model.ExchangeCommunityGrant(code, communityTestOrigin, strings.Repeat("x", 43))
	require.Error(t, err)
	_, err = model.ExchangeCommunityGrant(code, "https://other.example", communityTestVerifier)
	require.Error(t, err)
	token, err := model.ExchangeCommunityGrant(code, communityTestOrigin, communityTestVerifier)
	require.NoError(t, err)
	_, err = model.ConsumeCommunityIdentity(token, "https://other.example")
	require.Error(t, err)
	_, err = model.ConsumeCommunityIdentity(token, communityTestOrigin)
	require.NoError(t, err)
}

func TestCommunitySSORevocationAndExpiry(t *testing.T) {
	for _, at := range []string{"code", "identity"} {
		for _, change := range []string{"disabled", "password", "revoked", "session-version", "session-expired", "flow-expired", "deleted"} {
			t.Run(at+"/"+change, func(t *testing.T) {
				user, identity := setupCommunitySSOTest(t)
				t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
				code := communityTestGrant(t, identity)
				token := ""
				if at == "identity" {
					var err error
					token, err = model.ExchangeCommunityGrant(code, communityTestOrigin, communityTestVerifier)
					require.NoError(t, err)
				}
				var result *gorm.DB
				switch change {
				case "disabled":
					result = model.DB.Model(user).Update("status", common.UserStatusDisabled)
				case "password":
					result = model.DB.Model(user).Update("auth_version", gorm.Expr("auth_version + 1"))
				case "revoked":
					result = model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("status", model.UserSessionStatusRevoked)
				case "session-version":
					result = model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("version", gorm.Expr("version + 1"))
				case "session-expired":
					result = model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("expires_at", time.Now().Unix()-1)
				case "flow-expired":
					result = model.DB.Model(&model.AuthFlow{}).Where("user_id = ?", user.Id).Update("expires_at", time.Now().Add(-time.Minute))
				case "deleted":
					result = model.DB.Delete(user)
				}
				require.NoError(t, result.Error)
				if at == "code" {
					_, err := model.ExchangeCommunityGrant(code, communityTestOrigin, communityTestVerifier)
					require.Error(t, err)
				} else {
					_, err := model.ConsumeCommunityIdentity(token, communityTestOrigin)
					require.Error(t, err)
				}
			})
		}
	}
}

func TestCommunitySSOServerExchangeBudget(t *testing.T) {
	_, identity := setupCommunitySSOTest(t)
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	router := gin.New()
	router.POST("/token", middleware.CommunitySSOExchangeRateLimit(), CommunitySSOToken)
	router.GET("/userinfo", middleware.CommunitySSOExchangeRateLimit(), CommunitySSOUserinfo)
	for i := 0; i < 25; i++ {
		code := communityTestGrant(t, identity)
		body, _ := json.Marshal(map[string]string{"grant_type": "authorization_code", "client_id": communityTestOrigin, "redirect_uri": communityTestOrigin + "/api/newapi/callback", "code": code, "code_verifier": communityTestVerifier})
		req := httptest.NewRequest("POST", "/token", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r := httptest.NewRecorder()
		router.ServeHTTP(r, req)
		require.Equal(t, 200, r.Code)
		var result map[string]any
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &result))
		req = httptest.NewRequest("GET", "/userinfo", nil)
		req.Header.Set("Authorization", "Bearer "+result["access_token"].(string))
		r = httptest.NewRecorder()
		router.ServeHTTP(r, req)
		require.Equal(t, 200, r.Code)
	}
}

func TestCommunitySSOMultipleOriginsStayIsolated(t *testing.T) {
	const added = "https://zaiduyu.top"
	for _, origin := range []string{communityTestOrigin, added} {
		t.Run(origin, func(t *testing.T) {
			user, identity := setupCommunitySSOTest(t)
			t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
			t.Setenv("COMMUNITY_SSO_ALLOWED_ORIGINS", added)
			body := communityTestRequest()
			body["client_id"], body["redirect_uri"] = origin, origin+"/api/newapi/callback"
			r := communityTestCall(t, body, identity, CommunitySSOAuthorize)
			require.Equal(t, 200, r.Code, r.Body.String())
			var grant map[string]string
			require.NoError(t, common.Unmarshal(r.Body.Bytes(), &grant))
			callback, err := url.Parse(grant["redirect_uri"])
			require.NoError(t, err)
			other := added
			if origin == added {
				other = communityTestOrigin
			}
			exchange := map[string]string{"grant_type": "authorization_code", "client_id": other, "redirect_uri": other + "/api/newapi/callback", "code": callback.Query().Get("code"), "code_verifier": communityTestVerifier}
			require.Equal(t, 400, communityTestCall(t, exchange, identity, CommunitySSOToken).Code)
			exchange["client_id"], exchange["redirect_uri"] = origin, origin+"/api/newapi/callback"
			r = communityTestCall(t, exchange, identity, CommunitySSOToken)
			require.Equal(t, 200, r.Code, r.Body.String())
			var tokens map[string]any
			require.NoError(t, common.Unmarshal(r.Body.Bytes(), &tokens))
			require.Equal(t, 400, communityTestCall(t, exchange, identity, CommunitySSOToken).Code)
			for attempt := range 2 {
				response := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(response)
				c.Request = httptest.NewRequest("GET", "/userinfo", nil)
				c.Request.Header.Set("Authorization", "Bearer "+tokens["access_token"].(string))
				CommunitySSOUserinfo(c)
				if attempt == 0 {
					require.Equal(t, 200, response.Code, response.Body.String())
					var claims map[string]string
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &claims))
					require.Equal(t, strconv.Itoa(user.Id), claims["sub"])
				} else {
					require.Equal(t, 401, response.Code)
				}
			}
		})
	}
}

func TestCommunitySSOExplicitAllowlistAndRemoval(t *testing.T) {
	_, identity := setupCommunitySSOTest(t)
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	t.Setenv("COMMUNITY_SSO_ALLOWED_ORIGINS", "https://zaiduyu.top,https://*.example,http://unsafe.example,https://path.example/path,https://user@info.example,https://query.example?x=1")
	t.Setenv("STUDIO_SSO_ALLOWED_ORIGINS", "https://studio-only.example")
	for _, origin := range []string{"https://evil.example", "https://zaiduyu.top.evil.example", "https://studio-only.example", "https://*.example", "http://unsafe.example", "https://path.example", "https://info.example", "https://query.example"} {
		body := communityTestRequest()
		body["client_id"], body["redirect_uri"] = origin, origin+"/api/newapi/callback"
		require.Equal(t, 400, communityTestCall(t, body, identity, CommunitySSOAuthorize).Code, origin)
	}
	body := communityTestRequest()
	body["client_id"] = "https://zaiduyu.top"
	require.Equal(t, 400, communityTestCall(t, body, identity, CommunitySSOAuthorize).Code)
	code, err := model.CreateCommunityGrant(model.AuthSessionIdentity(identity), "https://zaiduyu.top", body["code_challenge"])
	require.NoError(t, err)
	token, err := model.ExchangeCommunityGrant(code, "https://zaiduyu.top", communityTestVerifier)
	require.NoError(t, err)
	t.Setenv("COMMUNITY_SSO_ALLOWED_ORIGINS", "")
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest("GET", "/userinfo", nil)
	c.Request.Header.Set("Authorization", "Bearer "+token)
	CommunitySSOUserinfo(c)
	require.Equal(t, 401, r.Code)
}

func TestCommunitySSOConfigPreservesDefaults(t *testing.T) {
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	t.Setenv("STUDIO_SSO_ORIGIN", studioTestOrigin)
	t.Setenv("COMMUNITY_SSO_ALLOWED_ORIGINS", "https://zaiduyu.top,"+communityTestOrigin)
	t.Setenv("STUDIO_SSO_ALLOWED_ORIGINS", "https://zaiduyu.top")
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	CommunitySSOConfig(c)
	var config map[string]any
	require.NoError(t, common.Unmarshal(r.Body.Bytes(), &config))
	require.Equal(t, communityTestOrigin, config["community_origin"])
	require.Equal(t, studioTestOrigin, config["studio_origin"])
	require.Equal(t, []any{communityTestOrigin, "https://zaiduyu.top"}, config["community_origins"])
	require.Equal(t, []any{studioTestOrigin, "https://zaiduyu.top"}, config["studio_origins"])
}
