package middleware

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func requestClientIP(router http.Handler, remoteAddr string, forwardedFor string) string {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
	request.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}
	router.ServeHTTP(recorder, request)
	return recorder.Body.String()
}

func newClientIPRouter() *gin.Engine {
	router := gin.New()
	router.GET("/client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})
	return router
}

func TestConfigureTrustedProxiesDefaultsToLoopbackAndPrivateNetworks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("TRUSTED_PROXIES", "")
	router := newClientIPRouter()
	require.NoError(t, ConfigureTrustedProxies(router))

	testCases := []struct {
		name       string
		remoteAddr string
	}{
		{name: "IPv4 loopback", remoteAddr: "127.0.0.1:12345"},
		{name: "IPv6 loopback", remoteAddr: "[::1]:12345"},
		{name: "10 private network", remoteAddr: "10.20.30.40:12345"},
		{name: "172 private network", remoteAddr: "172.20.0.2:12345"},
		{name: "192 private network", remoteAddr: "192.168.10.2:12345"},
		{name: "IPv6 unique local network", remoteAddr: "[fd12:3456::2]:12345"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			clientIP := requestClientIP(router, testCase.remoteAddr, "203.0.113.10")
			assert.Equal(t, "203.0.113.10", clientIP)
		})
	}
}

func TestConfigureTrustedProxiesDefaultRejectsPublicPeerHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("TRUSTED_PROXIES", " \t ")
	router := newClientIPRouter()
	require.NoError(t, ConfigureTrustedProxies(router))

	clientIP := requestClientIP(router, "198.51.100.10:12345", "203.0.113.10")
	assert.Equal(t, "198.51.100.10", clientIP, "a public peer must not make a spoofed X-Forwarded-For authoritative")
}

func TestConfigureTrustedProxiesDefaultStopsAtPublicClientInForwardedChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("TRUSTED_PROXIES", "")
	router := newClientIPRouter()
	require.NoError(t, ConfigureTrustedProxies(router))

	clientIP := requestClientIP(router, "172.20.0.2:12345", "192.0.2.99, 203.0.113.10")
	assert.Equal(t, "203.0.113.10", clientIP, "the first public hop from the trusted proxy must win over a client-supplied prefix")
}

func TestConfigureTrustedProxiesNoneDisablesForwardedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("TRUSTED_PROXIES", " NoNe ")
	router := newClientIPRouter()
	require.NoError(t, ConfigureTrustedProxies(router))

	clientIP := requestClientIP(router, "127.0.0.1:12345", "203.0.113.10")
	assert.Equal(t, "127.0.0.1", clientIP)
}

func TestConfigureTrustedProxiesAcceptsTrimmedIPsAndCIDRs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("TRUSTED_PROXIES", " 192.0.2.0/24, 198.51.100.30 ")
	router := newClientIPRouter()
	require.NoError(t, ConfigureTrustedProxies(router))

	trustedClientIP := requestClientIP(router, "192.0.2.10:12345", "203.0.113.20")
	assert.Equal(t, "203.0.113.20", trustedClientIP)

	trustedExactIP := requestClientIP(router, "198.51.100.30:12345", "203.0.113.21")
	assert.Equal(t, "203.0.113.21", trustedExactIP)

	untrustedClientIP := requestClientIP(router, "198.51.100.20:12345", "203.0.113.22")
	assert.Equal(t, "198.51.100.20", untrustedClientIP)

	defaultProxyIP := requestClientIP(router, "127.0.0.1:12345", "203.0.113.23")
	assert.Equal(t, "127.0.0.1", defaultProxyIP, "an explicit list must replace, not extend, the compatibility defaults")
}

func TestConfigureTrustedProxiesRejectsInvalidConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	testCases := []struct {
		name  string
		value string
	}{
		{name: "no entries", value: ", ,"},
		{name: "invalid entry", value: "not-an-ip"},
		{name: "mixed valid and invalid entries", value: "127.0.0.1, not-an-ip"},
		{name: "none mixed with valid entry", value: "none,127.0.0.1"},
		{name: "valid entry mixed with none", value: "127.0.0.1,NONE"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("TRUSTED_PROXIES", testCase.value)
			router := newClientIPRouter()
			assert.Error(t, ConfigureTrustedProxies(router))
		})
	}
}

const qxClientIPTestSecret = "qx-client-ip-test-secret-at-least-32-characters"
const qxLoginTestBody = `{"username":"test-user","password":"test-password"}`

func setupQXClientIPTest(t *testing.T) {
	t.Helper()
	t.Setenv("QXCHAT_SESSION_SECRET", qxClientIPTestSecret)
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AuthFlow{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; _ = sqlDB.Close() })
}

func signQXClientIPTestRequest(request *http.Request, body string) {
	digest := sha256.Sum256([]byte(body))
	canonical := strings.Join([]string{"qxchat-login-client-ip-v1", request.Method, request.URL.EscapedPath(), request.URL.RawQuery,
		request.Header.Get("X-QXChat-Timestamp"), request.Header.Get("X-QXChat-Nonce"), request.Header.Get("X-QXChat-Client-IP"),
		request.Header.Get("Origin"), hex.EncodeToString(digest[:])}, "\n")
	request.Header.Set("X-QXChat-Signature", common.GenerateHMACWithKey([]byte(qxClientIPTestSecret), canonical))
}

func qxClientIPTestRequest(t *testing.T, path, ip string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(qxLoginTestBody))
	request.RemoteAddr = "172.20.0.2:12345"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://accounts.example")
	request.Header.Set("X-QXChat-Client-IP", ip)
	request.Header.Set("X-QXChat-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	request.Header.Set("X-QXChat-Nonce", hex.EncodeToString(nonce))
	request.Header.Set("X-Forwarded-For", "192.0.2.99, 198.51.100.10")
	request.Header.Set("X-Real-IP", "198.51.100.10")
	request.Header.Set("CF-Connecting-IP", "198.51.100.10")
	signQXClientIPTestRequest(request, qxLoginTestBody)
	return request
}

func qxClientIPTestRouter(t *testing.T, beforeHandler ...gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, ConfigureTrustedProxies(router))
	router.Use(QXChatLoginClientIP())
	router.Use(beforeHandler...)
	router.Any("/*path", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		assert.NoError(t, err)
		assert.Equal(t, qxLoginTestBody, string(body), "signature verification must preserve the exact request body")
		c.String(http.StatusOK, c.ClientIP())
	})
	return router
}

func TestQXChatSignedClientIPSurvivesCloudflareAndPreservesOrdinaryProxyPolicy(t *testing.T) {
	setupQXClientIPTest(t)
	router := qxClientIPTestRouter(t)
	for _, ip := range []string{"203.0.113.12", "2001:db8::42", "10.2.3.4"} {
		request := qxClientIPTestRequest(t, "/api/user/login?turnstile=token%2Bvalue", ip)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, ip, recorder.Body.String())
		for _, name := range []string{"X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP", "X-QXChat-Signature"} {
			assert.Empty(t, request.Header.Get(name))
		}
	}
	request := qxClientIPTestRequest(t, "/api/user/login", "203.0.113.12")
	for _, name := range qxchatClientIPHeaders {
		request.Header.Del(name)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "198.51.100.10", recorder.Body.String(), "unsigned requests still use the first untrusted proxy hop")
	request = qxClientIPTestRequest(t, "/api/qxchat/session/exchange", "203.0.113.12")
	request.Header.Del("X-QXChat-Client-IP")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusOK, recorder.Code, "the existing session signature protocol must remain untouched")
}

func TestQXChatSignedClientIPBindsEveryRequestFieldAndRejectsReplay(t *testing.T) {
	setupQXClientIPTest(t)
	router := qxClientIPTestRouter(t)
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"IP", func(r *http.Request) { r.Header.Set("X-QXChat-Client-IP", "203.0.113.90") }},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://other.example") }},
		{"path", func(r *http.Request) { r.URL.Path = "/api/user/login/select" }},
		{"query", func(r *http.Request) { r.URL.RawQuery = "turnstile=changed" }},
		{"body", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{"username":"changed"}`)) }},
		{"partial headers", func(r *http.Request) { r.Header.Del("X-QXChat-Nonce") }},
		{"duplicate headers", func(r *http.Request) { r.Header.Add("X-QXChat-Client-IP", "203.0.113.12") }},
		{"stale", func(r *http.Request) {
			r.Header.Set("X-QXChat-Timestamp", strconv.FormatInt(time.Now().Unix()-120, 10))
			signQXClientIPTestRequest(r, qxLoginTestBody)
		}},
		{"future", func(r *http.Request) {
			r.Header.Set("X-QXChat-Timestamp", strconv.FormatInt(time.Now().Unix()+120, 10))
			signQXClientIPTestRequest(r, qxLoginTestBody)
		}},
		{"invalid IP", func(r *http.Request) {
			r.Header.Set("X-QXChat-Client-IP", "not-an-ip")
			signQXClientIPTestRequest(r, qxLoginTestBody)
		}},
		{"wrong path", func(r *http.Request) { r.URL.Path = "/api/user/self"; signQXClientIPTestRequest(r, qxLoginTestBody) }},
		{"wrong method", func(r *http.Request) { r.Method = http.MethodGet; signQXClientIPTestRequest(r, qxLoginTestBody) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := qxClientIPTestRequest(t, "/api/user/login?turnstile=token", "203.0.113.12")
			test.mutate(request)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			assert.Equal(t, http.StatusForbidden, recorder.Code)
		})
	}
	request := qxClientIPTestRequest(t, "/api/user/login/verify", "203.0.113.12")
	duplicate := request.Clone(request.Context())
	duplicate.Body = io.NopCloser(strings.NewReader(qxLoginTestBody))
	first, second := httptest.NewRecorder(), httptest.NewRecorder()
	router.ServeHTTP(first, request)
	router.ServeHTTP(second, duplicate)
	assert.Equal(t, http.StatusOK, first.Code)
	assert.Equal(t, http.StatusForbidden, second.Code)
}

func TestQXChatSignedClientIPFailsClosedOnBodyLimitAndUnavailableReplayStore(t *testing.T) {
	setupQXClientIPTest(t)
	router := qxClientIPTestRouter(t)
	request := qxClientIPTestRequest(t, "/api/user/login", "203.0.113.12")
	large := strings.Repeat("x", 8193)
	request.Body = io.NopCloser(strings.NewReader(large))
	signQXClientIPTestRequest(request, large)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
	require.NoError(t, model.DB.Migrator().DropTable(&model.AuthFlow{}))
	request = qxClientIPTestRequest(t, "/api/user/login", "203.0.113.12")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestQXChatSignedClientIPNonceCanOnlyWinOnceUnderConcurrency(t *testing.T) {
	setupQXClientIPTest(t)
	router := qxClientIPTestRouter(t)
	request := qxClientIPTestRequest(t, "/api/user/login/select", "203.0.113.12")
	results := make(chan int, 8)
	var workers sync.WaitGroup
	for range 8 {
		duplicate := request.Clone(request.Context())
		duplicate.Body = io.NopCloser(strings.NewReader(qxLoginTestBody))
		workers.Add(1)
		go func() {
			defer workers.Done()
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, duplicate)
			results <- recorder.Code
		}()
	}
	workers.Wait()
	close(results)
	successes := 0
	for code := range results {
		if code == http.StatusOK {
			successes++
		} else {
			assert.Equal(t, http.StatusForbidden, code)
		}
	}
	assert.Equal(t, 1, successes)
}

func TestQXChatSignedClientIPKeepsBothGlobalAndCriticalRateLimitsPerBrowser(t *testing.T) {
	setupQXClientIPTest(t)
	redis, _ := useRateLimitMiniRedis(t)
	oldGlobal, oldGlobalNum, oldGlobalDuration := common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration
	oldCritical, oldCriticalNum, oldCriticalDuration := common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration
	common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = true, 2, 60
	common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = true, 1, 60
	t.Cleanup(func() {
		common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = oldGlobal, oldGlobalNum, oldGlobalDuration
		common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = oldCritical, oldCriticalNum, oldCriticalDuration
	})
	router := qxClientIPTestRouter(t, GlobalAPIRateLimit(), CriticalRateLimit())
	for _, ip := range []string{"203.0.113.12", "203.0.113.13"} {
		for attempt, expected := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests} {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, qxClientIPTestRequest(t, "/api/user/login", ip))
			assert.Equal(t, expected, recorder.Code, "attempt %d for %s", attempt, ip)
		}
		assert.True(t, redis.Exists(redisIPRateLimitKey("GA", ip)))
		assert.True(t, redis.Exists(redisIPRateLimitKey("CT", ip)))
	}
	assert.False(t, redis.Exists(redisIPRateLimitKey("GA", "198.51.100.10")))
	assert.False(t, redis.Exists(redisIPRateLimitKey("CT", "198.51.100.10")))
}
