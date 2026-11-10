package controller

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// This credential is dedicated to lottery credits and is never sent to a browser.
func CommunityLotteryCredit(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	secret := os.Getenv("COMMUNITY_LOTTERY_SECRET")
	origin := communitySSOOrigin()
	if len(secret) < 32 || origin == "" {
		communitySSOError(c, http.StatusServiceUnavailable, "lottery_disabled")
		return
	}
	want := sha256.Sum256([]byte("Bearer " + secret))
	got := sha256.Sum256([]byte(c.GetHeader("Authorization")))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		communitySSOError(c, http.StatusUnauthorized, "invalid_lottery_credential")
		return
	}
	if c.Request.Method == http.MethodGet {
		c.JSON(http.StatusOK, gin.H{"ready": true, "client_id": origin})
		return
	}
	var request struct {
		ClientID  string `json:"client_id"`
		UserID    int    `json:"user_id"`
		Reference string `json:"reference"`
		Cents     int64  `json:"amount_cents"`
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") || common.DecodeJson(c.Request.Body, &request) != nil || request.ClientID != origin {
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	replayed, err := model.CreditCommunityLottery(request.UserID, request.Reference, request.Cents)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, model.ErrLotteryCreditConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, model.ErrInvalidTopUpQuota) || errors.Is(err, model.ErrWalletQuotaLimitExceeded) || errors.Is(err, model.ErrTopUpQuotaLimitExceeded) || errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusBadRequest
		}
		communitySSOError(c, status, "lottery_credit_failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"credited": true, "reference": request.Reference, "user_id": request.UserID, "amount_cents": request.Cents, "replayed": replayed})
}
