package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestQXChatIdentityLoginRequiresTurnstileWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("QXCHAT_IDENTITY_ENABLED", "")
	t.Setenv("QXCHAT_IDENTITY_SECRET", "")
	t.Setenv("QXCHAT_IDENTITY_SOURCE_URL", "")

	previousTurnstileEnabled := common.TurnstileCheckEnabled
	previousTurnstileSecret := common.TurnstileSecretKey
	previousCriticalRateLimitEnabled := common.CriticalRateLimitEnable
	common.TurnstileCheckEnabled = true
	common.TurnstileSecretKey = "test-secret"
	common.CriticalRateLimitEnable = false
	t.Cleanup(func() {
		common.TurnstileCheckEnabled = previousTurnstileEnabled
		common.TurnstileSecretKey = previousTurnstileSecret
		common.CriticalRateLimitEnable = previousCriticalRateLimitEnabled
	})

	engine := gin.New()
	RegisterQXChatIdentityRoutes(engine.Group("/api"))
	request := httptest.NewRequest(http.MethodPost, "/api/user/login/qxchat", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	// The compatibility path uses the same credential verifier as /login.
	// It must not bypass the challenge before reaching the disabled provider.
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"success":false`)
	require.Contains(t, recorder.Body.String(), "Turnstile token is empty")
}

func TestQXChatIdentityLoginRetainsRequestBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("QXCHAT_IDENTITY_ENABLED", "")
	t.Setenv("QXCHAT_IDENTITY_SECRET", "")
	t.Setenv("QXCHAT_IDENTITY_SOURCE_URL", "")

	previousTurnstileEnabled := common.TurnstileCheckEnabled
	previousCriticalRateLimitEnabled := common.CriticalRateLimitEnable
	previousBodyLimit := constant.AnonymousRequestBodyLimitKB
	common.TurnstileCheckEnabled = true
	common.CriticalRateLimitEnable = false
	constant.AnonymousRequestBodyLimitKB = 1
	t.Cleanup(func() {
		common.TurnstileCheckEnabled = previousTurnstileEnabled
		common.CriticalRateLimitEnable = previousCriticalRateLimitEnabled
		constant.AnonymousRequestBodyLimitKB = previousBodyLimit
	})

	engine := gin.New()
	RegisterQXChatIdentityRoutes(engine.Group("/api"))
	request := httptest.NewRequest(http.MethodPost, "/api/user/login/qxchat", strings.NewReader(strings.Repeat("x", 1025)))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
}

func TestQXChatIdentityLoginRetainsCriticalRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("QXCHAT_IDENTITY_ENABLED", "")
	t.Setenv("QXCHAT_IDENTITY_SECRET", "")
	t.Setenv("QXCHAT_IDENTITY_SOURCE_URL", "")

	previousTurnstileEnabled := common.TurnstileCheckEnabled
	previousCriticalRateLimitEnabled := common.CriticalRateLimitEnable
	previousCriticalRateLimitNum := common.CriticalRateLimitNum
	previousCriticalRateLimitDuration := common.CriticalRateLimitDuration
	previousRedisEnabled := common.RedisEnabled
	common.TurnstileCheckEnabled = true
	common.CriticalRateLimitEnable = true
	common.CriticalRateLimitNum = 1
	common.CriticalRateLimitDuration = 1200
	common.RedisEnabled = false
	t.Cleanup(func() {
		common.TurnstileCheckEnabled = previousTurnstileEnabled
		common.CriticalRateLimitEnable = previousCriticalRateLimitEnabled
		common.CriticalRateLimitNum = previousCriticalRateLimitNum
		common.CriticalRateLimitDuration = previousCriticalRateLimitDuration
		common.RedisEnabled = previousRedisEnabled
	})

	engine := gin.New()
	RegisterQXChatIdentityRoutes(engine.Group("/api"))
	for attempt, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodPost, "/api/user/login/qxchat", nil)
		request.RemoteAddr = "198.51.100.47:12345"
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		require.Equal(t, want, recorder.Code, "attempt %d", attempt+1)
	}
}
