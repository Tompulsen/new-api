package controller

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
		if c.Request.URL.RawQuery == "" {
			c.JSON(http.StatusOK, gin.H{"ready": true, "client_id": origin})
			return
		}
		query, err := url.ParseQuery(c.Request.URL.RawQuery)
		if err != nil || len(query) != 2 || len(query["client_id"]) != 1 || len(query["user_id"]) != 1 || query.Get("client_id") != origin {
			communitySSOError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		userID, err := strconv.ParseInt(query.Get("user_id"), 10, 64)
		if err != nil || userID <= 0 || userID > common.MaxWalletQuota || strconv.FormatInt(userID, 10) != query.Get("user_id") {
			communitySSOError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		support, err := model.GetCommunityLotterySupport(int(userID))
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, model.ErrInvalidTopUpQuota) {
				status = http.StatusBadRequest
			}
			communitySSOError(c, status, "lottery_support_failed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"ready": true, "client_id": origin, "user_id": userID, "support_cny_fen": support.SupportCNYFen, "has_support": support.HasSupport})
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
	receipt, replayed, err := model.CreditCommunityLottery(request.UserID, request.Reference, request.Cents)
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
	var snapshot any
	if receipt.LotteryCredit != nil {
		snapshot = gin.H{"id": receipt.Id, "quota_added": receipt.LotteryCredit.QuotaAdded, "quota_per_usd": receipt.LotteryCredit.QuotaPerUSD}
	}
	c.JSON(http.StatusOK, gin.H{"credited": true, "reference": receipt.TradeNo, "user_id": receipt.UserId, "amount_cents": receipt.Amount, "replayed": replayed, "receipt": snapshot})
}
