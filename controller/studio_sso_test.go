package controller

import (
	"strconv"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
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
