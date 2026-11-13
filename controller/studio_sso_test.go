package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const studioTestOrigin = "https://studio.example"

func TestStudioSSOUsesMatchingUserAndIndependentSession(t *testing.T) {
	user, identity := setupCommunitySSOTest(t)
	challenge := communityTestRequest()["code_challenge"]
	code, err := model.CreateStudioGrant(model.AuthSessionIdentity(identity), studioTestOrigin, challenge, user.Id)
	require.NoError(t, err)
	_, _, err = service.ExchangeStudioLogin(code, studioTestOrigin, communityTestVerifier, user.Id+1, "127.0.0.1", "test")
	require.Error(t, err)
	bundle, actual, err := service.ExchangeStudioLogin(code, studioTestOrigin, communityTestVerifier, user.Id, "127.0.0.1", "test")
	require.NoError(t, err)
	require.Equal(t, user.Id, actual.Id)
	require.NotEqual(t, identity.SessionID, bundle.Session.SID)
	require.NotEmpty(t, bundle.RefreshToken)
	parsed, err := service.ParseAccessToken(bundle.AccessToken)
	require.NoError(t, err)
	require.Equal(t, user.Id, parsed.UserID)
	require.NoError(t, model.ValidateAuthSessionWithTx(model.DB, model.AuthSessionIdentity(identity)))
	_, _, err = service.ExchangeStudioLogin(code, studioTestOrigin, communityTestVerifier, user.Id, "127.0.0.1", "test")
	require.Error(t, err)
	var stored model.User
	require.NoError(t, model.DB.First(&stored, user.Id).Error)
	require.Equal(t, user.Quota, stored.Quota)
	require.Equal(t, user.UsedQuota, stored.UsedQuota)
}

func TestStudioSSORejectsIdentityGrantAndWrongAccount(t *testing.T) {
	user, identity := setupCommunitySSOTest(t)
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	t.Setenv("STUDIO_SSO_ORIGIN", studioTestOrigin)
	identityCode := communityTestGrant(t, identity)
	_, _, err := service.ExchangeStudioLogin(identityCode, studioTestOrigin, communityTestVerifier, user.Id, "", "test")
	require.Error(t, err)
	body := communityTestRequest()
	body["client_id"] = studioTestOrigin
	body["redirect_uri"] = studioTestOrigin + "/api/new-api/community/callback"
	body["scope"] = "chat_session"
	body["expected_subject"] = strconv.Itoa(user.Id + 1)
	require.Equal(t, 403, communityTestCall(t, body, identity, StudioSSOAuthorize).Code)
	body["expected_subject"] = strconv.Itoa(user.Id)
	require.Equal(t, 200, communityTestCall(t, body, identity, StudioSSOAuthorize).Code)
	body["scope"] = "identity"
	require.Equal(t, 400, communityTestCall(t, body, identity, StudioSSOAuthorize).Code)
}

func TestStudioSSOLimitsAndRevocationRollback(t *testing.T) {
	user, identity := setupCommunitySSOTest(t)
	challenge := communityTestRequest()["code_challenge"]
	code, err := model.CreateStudioGrant(model.AuthSessionIdentity(identity), studioTestOrigin, challenge, user.Id)
	require.NoError(t, err)
	previous := common.UserSessionActiveLimit
	common.UserSessionActiveLimit = 1
	t.Cleanup(func() { common.UserSessionActiveLimit = previous })
	_, _, err = service.ExchangeStudioLogin(code, studioTestOrigin, communityTestVerifier, user.Id, "", "test")
	require.ErrorIs(t, err, model.ErrUserSessionLimit)
	common.UserSessionActiveLimit = previous
	_, _, err = service.ExchangeStudioLogin(code, studioTestOrigin, communityTestVerifier, user.Id, "", "test")
	require.NoError(t, err)
	code, err = model.CreateStudioGrant(model.AuthSessionIdentity(identity), studioTestOrigin, challenge, user.Id)
	require.NoError(t, err)
	_, err = model.RevokeUserSession(user.Id, identity.SessionID, "test")
	require.NoError(t, err)
	_, _, err = service.ExchangeStudioLogin(code, studioTestOrigin, communityTestVerifier, user.Id, "", "test")
	require.Error(t, err)
}

func TestStudioSSOConcurrentExchangeCreatesOneSession(t *testing.T) {
	user, identity := setupCommunitySSOTest(t)
	code, err := model.CreateStudioGrant(model.AuthSessionIdentity(identity), studioTestOrigin, communityTestRequest()["code_challenge"], user.Id)
	require.NoError(t, err)
	var group sync.WaitGroup
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, err := service.ExchangeStudioLogin(code, studioTestOrigin, communityTestVerifier, user.Id, "", "test")
			results <- err == nil
		}()
	}
	group.Wait()
	close(results)
	success := 0
	for ok := range results {
		if ok {
			success++
		}
	}
	require.Equal(t, 1, success)
	var count int64
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("user_id = ?", user.Id).Count(&count).Error)
	require.EqualValues(t, 2, count)
}

func studioTokenRequest(t *testing.T, target, contentType string, origins []string, referer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", contentType)
	for _, origin := range origins {
		request.Header.Add("Origin", origin)
	}
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	router := gin.New()
	router.POST("/api/studio-sso/token", StudioSSOToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestStudioSSOTokenRejectsUnboundBrowserRequestsWithoutConsumingGrant(t *testing.T) {
	const issuer = "https://accounts.example.test"
	for _, secure := range []bool{false, true} {
		t.Run(strconv.FormatBool(secure), func(t *testing.T) {
			user, identity := setupCommunitySSOTest(t)
			t.Setenv("STUDIO_SSO_ORIGIN", studioTestOrigin)
			previousSecure, previousTrusted := common.SessionCookieSecure, common.SessionCookieTrustedURLs
			common.SessionCookieSecure, common.SessionCookieTrustedURLs = secure, nil
			t.Cleanup(func() {
				common.SessionCookieSecure, common.SessionCookieTrustedURLs = previousSecure, previousTrusted
			})
			code, err := model.CreateStudioGrant(model.AuthSessionIdentity(identity), studioTestOrigin, communityTestRequest()["code_challenge"], user.Id)
			require.NoError(t, err)
			body := map[string]string{
				"grant_type": "authorization_code", "client_id": studioTestOrigin,
				"redirect_uri": studioTestOrigin + "/api/new-api/community/callback",
				"code":         code, "code_verifier": communityTestVerifier, "expected_subject": strconv.Itoa(user.Id),
			}
			cases := []struct {
				name        string
				contentType string
				origins     []string
				referer     string
				status      int
			}{
				{name: "text form", contentType: "text/plain", origins: []string{issuer}, status: http.StatusUnsupportedMediaType},
				{name: "urlencoded form", contentType: "application/x-www-form-urlencoded", origins: []string{issuer}, status: http.StatusUnsupportedMediaType},
				{name: "multipart form", contentType: "multipart/form-data; boundary=test", origins: []string{issuer}, status: http.StatusUnsupportedMediaType},
				{name: "missing content type", origins: []string{issuer}, status: http.StatusUnsupportedMediaType},
				{name: "malformed content type", contentType: "application/json; charset", origins: []string{issuer}, status: http.StatusUnsupportedMediaType},
				{name: "missing origin", contentType: "application/json", status: http.StatusForbidden},
				{name: "null origin", contentType: "application/json", origins: []string{"null"}, status: http.StatusForbidden},
				{name: "foreign origin", contentType: "application/json", origins: []string{"https://unrelated.example.test"}, status: http.StatusForbidden},
				{name: "origin suffix", contentType: "application/json", origins: []string{issuer + ".unrelated.test"}, status: http.StatusForbidden},
				{name: "duplicate origin", contentType: "application/json", origins: []string{issuer, issuer}, status: http.StatusForbidden},
				{name: "bad origin overrides referer", contentType: "application/json", origins: []string{"https://unrelated.example.test"}, referer: issuer + "/dashboard", status: http.StatusForbidden},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					response := studioTokenRequest(t, issuer+"/api/studio-sso/token", tc.contentType, tc.origins, tc.referer, body)
					assert.Equal(t, tc.status, response.Code)
					assert.Empty(t, response.Header().Values("Set-Cookie"))
				})
			}
			var count int64
			require.NoError(t, model.DB.Model(&model.UserSession{}).Where("user_id = ?", user.Id).Count(&count).Error)
			assert.EqualValues(t, 1, count)
			accepted := studioTokenRequest(t, issuer+"/api/studio-sso/token", "Application/JSON; charset=utf-8", []string{issuer}, "", body)
			require.Equal(t, http.StatusOK, accepted.Code, accepted.Body.String())
			assert.NotEmpty(t, accepted.Result().Cookies())
			require.NoError(t, model.DB.Model(&model.UserSession{}).Where("user_id = ?", user.Id).Count(&count).Error)
			assert.EqualValues(t, 2, count)
			replayed := studioTokenRequest(t, issuer+"/api/studio-sso/token", "application/json", []string{issuer}, "", body)
			assert.Equal(t, http.StatusBadRequest, replayed.Code)
			assert.Empty(t, replayed.Header().Values("Set-Cookie"))
		})
	}
}

func TestStudioSSOTokenAcceptsTrustedProxyOrigin(t *testing.T) {
	user, identity := setupCommunitySSOTest(t)
	t.Setenv("STUDIO_SSO_ORIGIN", studioTestOrigin)
	previousSecure, previousTrusted := common.SessionCookieSecure, common.SessionCookieTrustedURLs
	common.SessionCookieSecure = true
	common.SessionCookieTrustedURLs = []string{"https://accounts.example.test"}
	t.Cleanup(func() {
		common.SessionCookieSecure, common.SessionCookieTrustedURLs = previousSecure, previousTrusted
	})
	code, err := model.CreateStudioGrant(model.AuthSessionIdentity(identity), studioTestOrigin, communityTestRequest()["code_challenge"], user.Id)
	require.NoError(t, err)
	body := map[string]string{
		"grant_type": "authorization_code", "client_id": studioTestOrigin,
		"redirect_uri": studioTestOrigin + "/api/new-api/community/callback",
		"code":         code, "code_verifier": communityTestVerifier, "expected_subject": strconv.Itoa(user.Id),
	}
	response := studioTokenRequest(t, "http://api.internal/api/studio-sso/token", "application/json", []string{"https://accounts.example.test"}, "", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var refresh *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "new_api_refresh" {
			refresh = cookie
		}
	}
	require.NotNil(t, refresh)
	assert.NotEmpty(t, refresh.Value)
	assert.True(t, refresh.HttpOnly)
	assert.True(t, refresh.Secure)
	assert.Equal(t, http.SameSiteStrictMode, refresh.SameSite)
}

func TestStudioSSOMultipleOriginsStayIsolated(t *testing.T) {
	const added = "https://zaiduyu.top"
	const issuer = "https://accounts.example.test"
	for _, origin := range []string{studioTestOrigin, added} {
		t.Run(origin, func(t *testing.T) {
			user, identity := setupCommunitySSOTest(t)
			t.Setenv("STUDIO_SSO_ORIGIN", studioTestOrigin)
			t.Setenv("STUDIO_SSO_ALLOWED_ORIGINS", added)
			t.Setenv("COMMUNITY_SSO_ALLOWED_ORIGINS", "https://community-only.example")
			body := communityTestRequest()
			body["client_id"], body["redirect_uri"] = origin, origin+"/api/new-api/community/callback"
			body["scope"], body["expected_subject"] = "chat_session", strconv.Itoa(user.Id)
			r := communityTestCall(t, body, identity, StudioSSOAuthorize)
			require.Equal(t, 200, r.Code, r.Body.String())
			var grant map[string]string
			require.NoError(t, common.Unmarshal(r.Body.Bytes(), &grant))
			callback, err := url.Parse(grant["redirect_uri"])
			require.NoError(t, err)
			other := added
			if origin == added {
				other = studioTestOrigin
			}
			body = map[string]string{"grant_type": "authorization_code", "client_id": other, "redirect_uri": other + "/api/new-api/community/callback", "code": callback.Query().Get("code"), "code_verifier": communityTestVerifier, "expected_subject": strconv.Itoa(user.Id)}
			r = studioTokenRequest(t, issuer+"/api/studio-sso/token", "application/json", []string{issuer}, "", body)
			require.Equal(t, 400, r.Code)
			require.Empty(t, r.Header().Values("Set-Cookie"))
			body["client_id"], body["redirect_uri"] = origin, origin+"/api/new-api/community/callback"
			r = studioTokenRequest(t, issuer+"/api/studio-sso/token", "application/json", []string{issuer}, "", body)
			require.Equal(t, 200, r.Code, r.Body.String())
			require.NotEmpty(t, r.Result().Cookies())
			require.Equal(t, 400, studioTokenRequest(t, issuer+"/api/studio-sso/token", "application/json", []string{issuer}, "", body).Code)
			body = communityTestRequest()
			body["client_id"], body["redirect_uri"] = "https://community-only.example", "https://community-only.example/api/new-api/community/callback"
			body["scope"], body["expected_subject"] = "chat_session", strconv.Itoa(user.Id)
			require.Equal(t, 400, communityTestCall(t, body, identity, StudioSSOAuthorize).Code)
		})
	}
}
