package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const unifiedLoginOrigin = "https://accounts.example.com"

func setupUnifiedLogin(t *testing.T) (*model.User, *gin.Engine) {
	t.Helper()
	user, _ := setupQXChatIdentityTest(t)
	passwordEnabled, secure, trusted := common.PasswordLoginEnabled, common.SessionCookieSecure, common.SessionCookieTrustedURLs
	common.PasswordLoginEnabled, common.SessionCookieSecure = true, true
	common.SessionCookieTrustedURLs = []string{unifiedLoginOrigin}
	t.Cleanup(func() {
		common.PasswordLoginEnabled, common.SessionCookieSecure, common.SessionCookieTrustedURLs = passwordEnabled, secure, trusted
	})
	t.Setenv("QXCHAT_IDENTITY_ENABLED", "1")
	t.Setenv("QXCHAT_IDENTITY_SECRET", qxchatTestSecret)
	t.Setenv("QXCHAT_IDENTITY_SOURCE_URL", "https://qxchat.example.com")
	require.NoError(t, model.DB.Create(&model.QXChatIdentity{SourceID: qxchatTestSource, UserID: user.Id,
		SourceUsername: "legacy-user", SourceEmail: "legacy@example.com", State: model.QXChatIdentityActive}).Error)
	other := model.User{Username: "legacy-user", Password: "not-a-valid-hash", AffCode: "legacy-conflict", AuthVersion: 1, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&other).Error)
	require.NoError(t, model.InitializeLoginIdentifiers())
	router := gin.New()
	for path, handler := range map[string]gin.HandlerFunc{
		"/api/user/login": Login, "/api/user/login/select": SelectLoginIdentity,
		"/api/user/login/qxchat": QXChatIdentityLogin, "/api/user/login/verify": VerifyLogin,
	} {
		router.POST(path, middleware.SessionCookieOriginGuard(), handler)
	}
	router.GET("/api/user/self/login-identifiers", middleware.UserAuth(), GetSelfLoginIdentifiers)
	return user, router
}

func unifiedLoginRequest(t *testing.T, router *gin.Engine, path, origin string, input any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := common.Marshal(input)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func startUnifiedLoginSelection(t *testing.T, router *gin.Engine, path string) model.LoginIdentitySelection {
	t.Helper()
	response := unifiedLoginRequest(t, router, path, unifiedLoginOrigin,
		map[string]string{"username": "legacy-user", "password": "enrollment-password"})
	require.Equal(t, http.StatusOK, response.Code)
	var result struct {
		Success bool                         `json:"success"`
		Data    model.LoginIdentitySelection `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	require.True(t, result.Data.RequiresIdentitySelection)
	require.Len(t, result.Data.Candidates, 1)
	assert.Empty(t, response.Header().Values("Set-Cookie"))
	assert.NotContains(t, response.Body.String(), "access_token")
	assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
	return result.Data
}

func TestUnifiedLoginControllerSelectionPreservesMFA(t *testing.T) {
	for _, path := range []string{"/api/user/login", "/api/user/login/qxchat"} {
		t.Run(path, func(t *testing.T) {
			user, router := setupUnifiedLogin(t)
			factor := model.TwoFA{UserId: user.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}
			require.NoError(t, model.DB.Create(&factor).Error)
			selection := startUnifiedLoginSelection(t, router, path)
			confirm := map[string]string{"flow_token": selection.FlowToken, "selection_id": selection.Candidates[0].SelectionID}
			response := unifiedLoginRequest(t, router, "/api/user/login/select", unifiedLoginOrigin, confirm)
			var result securityEnrollmentResponse
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success, response.Body.String())
			var challenge service.LoginChallenge
			require.NoError(t, common.Unmarshal(result.Data, &challenge))
			require.True(t, challenge.RequireVerification)
			require.Len(t, challenge.FlowToken, 43)
			assert.Empty(t, response.Header().Values("Set-Cookie"))
			assert.NotContains(t, response.Body.String(), "access_token")
			count, err := model.CountActiveUserSessions(user.Id, time.Now().Unix())
			require.NoError(t, err)
			assert.EqualValues(t, 1, count)
			code, err := totp.GenerateCode(factor.Secret, time.Now())
			require.NoError(t, err)
			response = unifiedLoginRequest(t, router, "/api/user/login/verify", unifiedLoginOrigin,
				map[string]string{"flow_token": challenge.FlowToken, "method": "2fa", "code": code})
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success, response.Body.String())
			var bundle service.AuthBundle
			require.NoError(t, common.Unmarshal(result.Data, &bundle))
			identity, err := service.ParseAccessToken(bundle.AccessToken)
			require.NoError(t, err)
			assert.Equal(t, user.Id, identity.UserID)
			assert.NotEmpty(t, response.Header().Values("Set-Cookie"))
			response = unifiedLoginRequest(t, router, "/api/user/login/select", unifiedLoginOrigin, confirm)
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.False(t, result.Success)
			assert.Empty(t, response.Header().Values("Set-Cookie"))
		})
	}
}

func TestUnifiedLoginControllerRejectsUntrustedOrigins(t *testing.T) {
	_, router := setupUnifiedLogin(t)
	selection := startUnifiedLoginSelection(t, router, "/api/user/login")
	for _, origin := range []string{"", "null", "https://attacker.example.com"} {
		for _, path := range []string{"/api/user/login", "/api/user/login/qxchat", "/api/user/login/select", "/api/user/login/verify"} {
			response := unifiedLoginRequest(t, router, path, origin, map[string]string{
				"username": "legacy-user", "password": "enrollment-password",
				"flow_token": selection.FlowToken, "selection_id": selection.Candidates[0].SelectionID})
			assert.Equal(t, http.StatusForbidden, response.Code, path+" "+origin)
			assert.Empty(t, response.Header().Values("Set-Cookie"))
		}
	}
	_, err := model.GetAuthFlow(selection.FlowToken, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeLoginIdentity})
	require.NoError(t, err, "untrusted submissions do not consume a valid flow")
}

func TestUnifiedLoginControllerSelfIdentifiersRequireAuthentication(t *testing.T) {
	user, router := setupUnifiedLogin(t)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/user/self/login-identifiers", nil))
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	bundle, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", "test")
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodGet, "/api/user/self/login-identifiers?user_id=99999", nil)
	request.Header.Set("Authorization", "Bearer "+bundle.AccessToken)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var result struct {
		Success bool                       `json:"success"`
		Data    model.UserLoginIdentifiers `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	assert.Equal(t, user.Username, result.Data.Username)
	assert.ElementsMatch(t, []string{user.Username, "legacy@example.com"}, result.Data.LoginIdentifiers)
	assert.NotContains(t, response.Body.String(), "password")
}

func TestUnifiedLoginReverifyCurrentEmailDoesNotReclaimLegacyCollision(t *testing.T) {
	user, identity := setupQXChatIdentityTest(t)
	t.Setenv("QXCHAT_IDENTITY_ENABLED", "1")
	user.Email = "already-owned@example.com"
	require.NoError(t, user.Update(false))
	other := model.User{Username: "email-alias-owner", AffCode: "email-alias-owner"}
	require.NoError(t, model.DB.Create(&other).Error)
	require.NoError(t, model.DB.Create(&model.QXChatIdentity{SourceID: qxchatTestSource, UserID: other.Id,
		SourceUsername: other.Username, SourceEmail: user.Email, State: model.QXChatIdentityActive}).Error)
	require.NoError(t, model.InitializeLoginIdentifiers())
	hash, err := common.Password2Hash("123456")
	require.NoError(t, err)
	state := model.EmailBindingState{CurrentEmail: user.Email, Email: user.Email, NewCodeHash: hash,
		Authorization: &model.AuthFlowAuthorization{AuthSessionIdentity: identity, ProofID: 1,
			Scope: service.VerificationScopeAccountBind, ContextHash: strings.Repeat("a", 64)}}
	token, _, err := model.CreateEmailBinding(identity, state)
	require.NoError(t, err)
	_, err = model.CompleteEmailBinding(identity, token, user.Email, "123456", "")
	require.NoError(t, err, "verifying an unchanged email must not try to claim it again")
}

func TestUnifiedLoginControllerConcurrentSelectionCreatesOneSession(t *testing.T) {
	user, router := setupUnifiedLogin(t)
	connection, err := model.DB.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(8)
	// Holding two connections establishes this is not a single-connection test.
	first, err := connection.Conn(context.Background())
	require.NoError(t, err)
	second, err := connection.Conn(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, connection.Stats().OpenConnections, 2)
	require.NoError(t, first.Close())
	require.NoError(t, second.Close())
	selection := startUnifiedLoginSelection(t, router, "/api/user/login")
	start, results := make(chan struct{}), make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			results <- unifiedLoginRequest(t, router, "/api/user/login/select", unifiedLoginOrigin,
				map[string]string{"flow_token": selection.FlowToken, "selection_id": selection.Candidates[0].SelectionID})
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		response := <-results
		var result securityEnrollmentResponse
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		if result.Success {
			successes++
			assert.NotEmpty(t, response.Header().Values("Set-Cookie"))
		} else {
			assert.Empty(t, response.Header().Values("Set-Cookie"))
		}
	}
	assert.Equal(t, 1, successes)
	count, err := model.CountActiveUserSessions(user.Id, time.Now().Unix())
	require.NoError(t, err)
	assert.EqualValues(t, 2, count)
}

func TestUnifiedLoginQXAccountDetailsAreOptInAndPreserveEmptyValues(t *testing.T) {
	user, auth := setupQXChatIdentityTest(t)
	startQXChatSourceFixture(t)
	t.Setenv("QXCHAT_SESSION_SECRET", qxchatTestSecret)
	t.Setenv("QXCHAT_SSO_ORIGIN", "https://qxchat.example.com")
	mapping := model.QXChatIdentity{SourceID: qxchatTestSource, UserID: user.Id, SourceUsername: user.Username,
		SourceEmail: "legacy@example.com", State: model.QXChatIdentityActive}
	require.NoError(t, model.DB.Create(&mapping).Error)
	other := model.User{Username: "other-account", Email: user.Username, AffCode: "details-other"}
	require.NoError(t, model.DB.Create(&other).Error)
	require.NoError(t, model.DB.Create(&model.QXChatIdentity{SourceID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", UserID: other.Id,
		SourceUsername: other.Username, SourceEmail: mapping.SourceEmail, State: model.QXChatIdentityActive}).Error)
	verifier := strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	code, err := model.CreateCommunityGrant(auth, "https://qxchat.example.com", base64.RawURLEncoding.EncodeToString(digest[:]))
	require.NoError(t, err)
	token, err := model.ExchangeCommunityGrant(code, "https://qxchat.example.com", verifier)
	require.NoError(t, err)
	proof, err := model.ExchangeQXChatSession(token, "https://qxchat.example.com")
	require.NoError(t, err)
	lookupQueries := 0
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register("count-login-identifiers", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "LOWER(TRIM") {
			lookupQueries++
		}
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Query().Remove("count-login-identifiers") })
	request := map[string]any{"session_proof": proof.SessionProof, "source_id": mapping.SourceID, "newapi_user_id": user.Id}
	response := qxchatSessionRequest(t, "/api/qxchat/session/validate", request, strings.Repeat("3", 32), QXChatSessionValidate)
	require.Equal(t, http.StatusOK, response.Code)
	assert.NotContains(t, response.Body.String(), "account_username")
	assert.NotContains(t, response.Body.String(), "login_identifiers")
	assert.Zero(t, lookupQueries)
	request["include_account"] = true
	response = qxchatSessionRequest(t, "/api/qxchat/session/validate", request, strings.Repeat("4", 32), QXChatSessionValidate)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"account_username":"`+user.Username+`"`)
	assert.Contains(t, response.Body.String(), `"account_email":""`)
	assert.Contains(t, response.Body.String(), `"login_identifiers":[]`)
	assert.Greater(t, lookupQueries, 0)
}
