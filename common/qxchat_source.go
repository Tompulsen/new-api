package common

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const qxchatPreparePath = "/api/account-unification/prepare"
const qxchatCommitPath = "/api/account-unification/commit"
const qxchatProvisionPath = "/api/account-unification/provision"
const QXChatSharedWalletPath = "/api/account-unification/shared-wallet"

var ErrQXChatUnavailable = errors.New("QXChat account connection is temporarily unavailable. Please try again.")

type QXChatSourceError struct {
	Status int
	Code   string
}

func (e *QXChatSourceError) Error() string { return ErrQXChatUnavailable.Error() }

func QXChatSourceEnabled() bool {
	return os.Getenv("QXCHAT_IDENTITY_ENABLED") == "1" && len(os.Getenv("QXCHAT_IDENTITY_SECRET")) >= 32 && os.Getenv("QXCHAT_IDENTITY_SOURCE_URL") != ""
}

// The configured source is operator-owned; user input never chooses a URL.
func CallQXChatSource(ctx context.Context, path string, input, output any) error {
	if !QXChatSourceEnabled() || (path != qxchatPreparePath && path != qxchatCommitPath && path != qxchatProvisionPath && path != QXChatSharedWalletPath) {
		return ErrQXChatUnavailable
	}
	base, err := url.Parse(os.Getenv("QXCHAT_IDENTITY_SOURCE_URL"))
	if err != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") || base.Host == "" {
		return ErrQXChatUnavailable
	}
	loopback := net.ParseIP(base.Hostname())
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback != nil && loopback.IsLoopback()) {
		return ErrQXChatUnavailable
	}
	body, err := Marshal(input)
	if err != nil {
		return err
	}
	base.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return ErrQXChatUnavailable
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nonceBytes)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{"qxchat-identity-v1", http.MethodPost, path, stamp, nonce, hex.EncodeToString(digest[:])}, "\n")
	secret := []byte(os.Getenv("QXCHAT_IDENTITY_SECRET"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-QXChat-Timestamp", stamp)
	req.Header.Set("X-QXChat-Nonce", nonce)
	req.Header.Set("X-QXChat-Signature", GenerateHMACWithKey(secret, canonical))
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return ErrQXChatUnavailable
	}
	defer res.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(res.Body, 32769))
	if err != nil || len(responseBody) > 32768 {
		return ErrQXChatUnavailable
	}
	responseDigest := sha256.Sum256(responseBody)
	responseCanonical := strings.Join([]string{"qxchat-identity-response-v1", nonce, strconv.Itoa(res.StatusCode), hex.EncodeToString(responseDigest[:])}, "\n")
	expected := GenerateHMACWithKey(secret, responseCanonical)
	if !hmac.Equal([]byte(res.Header.Get("X-QXChat-Signature")), []byte(expected)) {
		return ErrQXChatUnavailable
	}
	if res.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = Unmarshal(responseBody, &failure)
		return &QXChatSourceError{Status: res.StatusCode, Code: failure.Error}
	}
	if Unmarshal(responseBody, output) != nil {
		return ErrQXChatUnavailable
	}
	return nil
}
