package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

var qxchatClientIPHeaders = []string{"X-QXChat-Client-IP", "X-QXChat-Timestamp", "X-QXChat-Nonce", "X-QXChat-Signature"}

// QXChatLoginClientIP restores the browser IP only for a fresh, signed QX login
// request. It must run before IP rate limiting; ordinary requests keep the
// existing trusted-proxy policy.
func QXChatLoginClientIP() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.EscapedPath()
		allowedPath := path == "/api/user/login" || path == "/api/user/login/select" || path == "/api/user/login/verify"
		present := false
		for _, name := range qxchatClientIPHeaders {
			present = present || len(c.Request.Header.Values(name)) > 0
		}
		// Session-exchange requests use the existing timestamp/nonce/signature
		// headers, without Client-IP, and retain their own signature purpose.
		if (!allowedPath && len(c.Request.Header.Values("X-QXChat-Client-IP")) == 0) || !present {
			c.Next()
			return
		}
		reject := func(status int) {
			c.Header("Cache-Control", "no-store")
			c.AbortWithStatusJSON(status, gin.H{"success": false, "message": "login request authentication failed"})
		}
		if !allowedPath || c.Request.Method != http.MethodPost || c.GetHeader("Content-Encoding") != "" || len(c.Request.URL.RawQuery) > 8192 {
			reject(http.StatusForbidden)
			return
		}
		for _, name := range qxchatClientIPHeaders {
			if len(c.Request.Header.Values(name)) != 1 || c.GetHeader(name) == "" {
				reject(http.StatusForbidden)
				return
			}
		}
		secret := os.Getenv("QXCHAT_SESSION_SECRET")
		stamp, nonce, clientIP := c.GetHeader("X-QXChat-Timestamp"), c.GetHeader("X-QXChat-Nonce"), c.GetHeader("X-QXChat-Client-IP")
		seconds, err := strconv.ParseInt(stamp, 10, 64)
		now := time.Now().Unix()
		parsedIP := net.ParseIP(clientIP)
		nonceBytes, nonceErr := hex.DecodeString(nonce)
		signature, signatureErr := hex.DecodeString(c.GetHeader("X-QXChat-Signature"))
		origin := c.GetHeader("Origin")
		normalizedOrigin, originErr := common.NormalizeOrigin(origin)
		if len(secret) < 32 || err != nil || strconv.FormatInt(seconds, 10) != stamp || seconds < now-60 || seconds > now+60 ||
			len(clientIP) > 64 || parsedIP == nil || nonceErr != nil || len(nonceBytes) != 16 || hex.EncodeToString(nonceBytes) != nonce ||
			signatureErr != nil || len(signature) != sha256.Size || len(c.Request.Header.Values("Origin")) != 1 ||
			len(origin) > 512 || originErr != nil || normalizedOrigin != origin {
			reject(http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
		if err != nil {
			reject(http.StatusRequestEntityTooLarge)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		digest := sha256.Sum256(body)
		canonical := strings.Join([]string{"qxchat-login-client-ip-v1", http.MethodPost, path, c.Request.URL.RawQuery, stamp, nonce, clientIP, origin, hex.EncodeToString(digest[:])}, "\n")
		expected := hmac.New(sha256.New, []byte(secret))
		_, _ = expected.Write([]byte(canonical))
		if !hmac.Equal(signature, expected.Sum(nil)) {
			reject(http.StatusForbidden)
			return
		}
		if err := model.ClaimExternalAuthAssertion("qxchat_login_client_ip", nonce, time.Unix(seconds+120, 0)); err != nil {
			if errors.Is(err, model.ErrAuthFlowConsumed) || errors.Is(err, model.ErrAuthFlowInvalid) {
				reject(http.StatusForbidden)
			} else {
				reject(http.StatusServiceUnavailable)
			}
			return
		}
		c.Request.RemoteAddr = net.JoinHostPort(parsedIP.String(), "0")
		for _, name := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "CF-Connecting-IP", "True-Client-IP", "X-Appengine-Remote-Addr"} {
			c.Request.Header.Del(name)
		}
		for _, name := range qxchatClientIPHeaders {
			c.Request.Header.Del(name)
		}
		c.Next()
	}
}
