package middleware

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

type turnstileCheckResponse struct {
	Success  bool   `json:"success"`
	Hostname string `json:"hostname"`
	Action   string `json:"action"`
}

var turnstileReplayLimiter common.InMemoryRateLimiter

func TurnstileCheck(actions ...string) gin.HandlerFunc {
	expectedAction := ""
	if len(actions) > 0 {
		expectedAction = actions[0]
		turnstileReplayLimiter.Init(5 * time.Minute)
	}
	return func(c *gin.Context) {
		if common.TurnstileCheckEnabled {
			response := c.Query("turnstile")
			if response == "" {
				common.ApiErrorT(c, "Turnstile token is empty")
				c.Abort()
				return
			}
			if len(response) > 2048 {
				common.ApiErrorT(c, "Turnstile verification failed, please refresh and try again!")
				c.Abort()
				return
			}
			ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
			defer cancel()
			form := url.Values{
				"secret":   {common.TurnstileSecretKey},
				"response": {response},
				"remoteip": {c.ClientIP()},
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost,
				"https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
			if err != nil {
				common.ApiErrorT(c, "Turnstile verification failed, please refresh and try again!")
				c.Abort()
				return
			}
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rawRes, err := http.DefaultClient.Do(request)
			if err != nil {
				common.SysLog("Turnstile validation request failed")
				common.ApiErrorT(c, "Turnstile verification failed, please refresh and try again!")
				c.Abort()
				return
			}
			defer rawRes.Body.Close()
			var res turnstileCheckResponse
			body, err := io.ReadAll(io.LimitReader(rawRes.Body, 16*1024+1))
			if err != nil || len(body) > 16*1024 || rawRes.StatusCode != http.StatusOK {
				c.Abort()
				common.ApiErrorT(c, "Turnstile verification failed, please refresh and try again!")
				return
			}
			if common.Unmarshal(body, &res) != nil || !res.Success {
				common.ApiErrorT(c, "Turnstile verification failed, please refresh and try again!")
				c.Abort()
				return
			}
			if expectedAction != "" {
				trustedURL, err := url.Parse(system_setting.ServerAddress)
				if err != nil || (trustedURL.Scheme != "https" && trustedURL.Scheme != "http") ||
					trustedURL.Hostname() == "" || trustedURL.User != nil ||
					(!strings.EqualFold(res.Hostname, trustedURL.Hostname()) &&
						!strings.EqualFold(res.Hostname, "www."+strings.TrimPrefix(strings.ToLower(trustedURL.Hostname()), "www."))) ||
					res.Action != expectedAction {
					common.ApiErrorT(c, "Turnstile verification failed, please refresh and try again!")
					c.Abort()
					return
				}
				// Keep consumption atomic across nodes even while Cloudflare redemption state converges.
				key := fmt.Sprintf("turnstile:used:%x", sha256.Sum256([]byte(response)))
				accepted := false
				if common.RedisEnabled {
					if common.RDB != nil {
						replayCtx, replayCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
						accepted, err = common.RDB.SetNX(replayCtx, key, "1", 5*time.Minute).Result()
						replayCancel()
						if err != nil {
							common.SysLog("Turnstile replay storage is unavailable")
						}
					}
				} else {
					accepted = turnstileReplayLimiter.Request(key, 1, 300)
				}
				if !accepted {
					common.ApiErrorT(c, "Turnstile verification failed, please refresh and try again!")
					c.Abort()
					return
				}
			}
		}
		c.Next()
	}
}
