package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type turnstileTestTransport func(*http.Request) (*http.Response, error)

func (transport turnstileTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func setupTurnstileTest(t *testing.T) (*gin.Engine, *miniredis.Miniredis) {
	t.Helper()
	require.NoError(t, i18n.Init())
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	enabled, secret := common.TurnstileCheckEnabled, common.TurnstileSecretKey
	redisEnabled, previousClient := common.RedisEnabled, common.RDB
	address, transport := system_setting.ServerAddress, http.DefaultTransport
	common.TurnstileCheckEnabled, common.TurnstileSecretKey = true, "test-secret"
	common.RedisEnabled, common.RDB = true, client
	system_setting.ServerAddress = "https://zhangyuapi.com"
	t.Cleanup(func() {
		common.TurnstileCheckEnabled, common.TurnstileSecretKey = enabled, secret
		common.RedisEnabled, common.RDB = redisEnabled, previousClient
		system_setting.ServerAddress, http.DefaultTransport = address, transport
		require.NoError(t, client.Close())
	})
	http.DefaultTransport = turnstileTestTransport(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, "https://challenges.cloudflare.com/turnstile/v0/siteverify", request.URL.String())
		require.NoError(t, request.ParseForm())
		assert.Equal(t, "test-secret", request.Form.Get("secret"))
		_, hasDeadline := request.Context().Deadline()
		assert.True(t, hasDeadline, "provider validation must have a bounded timeout")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"success":true,"hostname":"zhangyuapi.com","action":"registration"}`)),
		}, nil
	})
	router := gin.New()
	router.GET("/verification", TurnstileCheck("registration"), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/register", TurnstileCheck("registration"), func(c *gin.Context) { c.Status(http.StatusConflict) })
	router.POST("/login", TurnstileCheck(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	return router, server
}

func TestTurnstileRegistrationRejectsInvalidProviderEvidence(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"wrong hostname", `{"success":true,"hostname":"evil.example","action":"registration"}`, 200},
		{"hostname suffix", `{"success":true,"hostname":"zhangyuapi.com.evil.example","action":"registration"}`, 200},
		{"www hostname suffix", `{"success":true,"hostname":"www.zhangyuapi.com.evil.example","action":"registration"}`, 200},
		{"nested www hostname", `{"success":true,"hostname":"evil.www.zhangyuapi.com","action":"registration"}`, 200},
		{"API hostname", `{"success":true,"hostname":"api.zhangyuapi.com","action":"registration"}`, 200},
		{"wrong action", `{"success":true,"hostname":"zhangyuapi.com","action":"login"}`, 200},
		{"www wrong action", `{"success":true,"hostname":"www.zhangyuapi.com","action":"login"}`, 200},
		{"missing metadata", `{"success":true}`, 200},
		{"expired or failed", `{"success":false,"error-codes":["timeout-or-duplicate"]}`, 200},
		{"invalid JSON", `{`, 200},
		{"oversized response", `{"success":true,"hostname":"zhangyuapi.com","action":"registration","padding":"` + strings.Repeat("a", 20000) + `"}`, 200},
		{"upstream error", `{"success":true,"hostname":"zhangyuapi.com","action":"registration"}`, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, server := setupTurnstileTest(t)
			http.DefaultTransport = turnstileTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "https://zhangyuapi.com/verification?turnstile=test-token", nil)
			request.Host = "evil.example"
			router.ServeHTTP(response, request)
			assert.Contains(t, response.Body.String(), `"success":false`)
			assert.NotEqual(t, http.StatusNoContent, response.Code)
			assert.Empty(t, server.Keys(), "failed validation must not consume replay storage")
		})
	}
}

func TestTurnstileRegistrationAcceptsConfiguredHostnameAndWWW(t *testing.T) {
	for _, hostname := range []string{"zhangyuapi.com", "www.zhangyuapi.com", "WWW.ZHANGYUAPI.COM"} {
		t.Run(hostname, func(t *testing.T) {
			router, _ := setupTurnstileTest(t)
			body, err := common.Marshal(turnstileCheckResponse{
				Success: true, Hostname: hostname, Action: "registration",
			})
			require.NoError(t, err)
			http.DefaultTransport = turnstileTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/verification?turnstile=www-token", nil)
			request.Host = "untrusted.example"
			router.ServeHTTP(response, request)
			assert.Equal(t, http.StatusNoContent, response.Code)
		})
	}
}

func TestTurnstileRegistrationConsumesAcrossRequestsAndBusinessFailures(t *testing.T) {
	router, server := setupTurnstileTest(t)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/register?turnstile=one-token", nil))
	assert.Equal(t, http.StatusConflict, first.Code, "business handler must run once")
	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/verification?turnstile=one-token", nil))
	assert.Contains(t, second.Body.String(), `"success":false`)
	require.Len(t, server.Keys(), 1)
	assert.NotContains(t, server.Keys()[0], "one-token")
	assert.Equal(t, 5*time.Minute, server.TTL(server.Keys()[0]))
}

func TestTurnstileRegistrationAdmitsOneConcurrentReplay(t *testing.T) {
	router, _ := setupTurnstileTest(t)
	var group sync.WaitGroup
	var accepted atomic.Int32
	for range 8 {
		group.Go(func() {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/verification?turnstile=concurrent-token", nil))
			if response.Code == http.StatusNoContent {
				accepted.Add(1)
			}
		})
	}
	group.Wait()
	assert.EqualValues(t, 1, accepted.Load())
}

func TestTurnstileRegistrationRejectsInvalidConfigurationOrUnavailableRedis(t *testing.T) {
	for _, failure := range []string{"invalid URL", "missing Redis", "unavailable Redis"} {
		t.Run(failure, func(t *testing.T) {
			router, server := setupTurnstileTest(t)
			switch failure {
			case "invalid URL":
				system_setting.ServerAddress = "zhangyuapi.com"
			case "missing Redis":
				common.RDB = nil
			case "unavailable Redis":
				server.Close()
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/verification?turnstile=valid-token", nil))
			assert.Contains(t, response.Body.String(), `"success":false`)
			assert.NotEqual(t, http.StatusNoContent, response.Code)
		})
	}
}

func TestTurnstileRegistrationRejectsMissingOversizedAndCancelledTokens(t *testing.T) {
	router, _ := setupTurnstileTest(t)
	for _, token := range []string{"", strings.Repeat("a", 2049)} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/verification?turnstile="+url.QueryEscape(token), nil))
		assert.Contains(t, response.Body.String(), `"success":false`)
	}
	http.DefaultTransport = turnstileTestTransport(func(request *http.Request) (*http.Response, error) {
		assert.Error(t, request.Context().Err())
		return nil, errors.New("sensitive-provider-detail")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/verification?turnstile=cancelled-token", nil).WithContext(ctx))
	assert.Contains(t, response.Body.String(), `"success":false`)
	assert.NotContains(t, response.Body.String(), "sensitive-provider-detail")
}

func TestTurnstileRegistrationMemoryReplayAndLegacyCompatibility(t *testing.T) {
	router, _ := setupTurnstileTest(t)
	common.RedisEnabled = false
	for attempt := range 2 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/verification?turnstile=memory-"+t.Name(), nil))
		if attempt == 0 {
			assert.Equal(t, http.StatusNoContent, response.Code)
		} else {
			assert.Contains(t, response.Body.String(), `"success":false`)
		}
	}
	http.DefaultTransport = turnstileTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true}`))}, nil
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/login?turnstile=legacy-token", nil))
	assert.Equal(t, http.StatusNoContent, response.Code)
	common.TurnstileCheckEnabled = false
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/verification", nil))
	assert.Equal(t, http.StatusNoContent, response.Code)
}
