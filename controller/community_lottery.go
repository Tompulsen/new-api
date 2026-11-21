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
		rewardKinds := []string{"balance"}
		quotaPerPoint, rateErr := model.UnifiedWalletQuotaPerPoint()
		if model.UnifiedWalletEnabled() && rateErr == nil {
			rewardKinds = append(rewardKinds, model.WalletCharity)
		}
		capabilities := gin.H{"ready": true, "client_id": origin, "reward_kinds": rewardKinds}
		if rateErr == nil {
			capabilities["quota_per_point"] = quotaPerPoint
		}
		if c.Request.URL.RawQuery == "" {
			c.JSON(http.StatusOK, capabilities)
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
		consumption, err := model.GetCommunityLotteryConsumption(int(userID))
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, model.ErrInvalidTopUpQuota) {
				status = http.StatusBadRequest
			}
			communitySSOError(c, status, "lottery_consumption_failed")
			return
		}
		capabilities["user_id"], capabilities["basis"] = userID, "consumption"
		capabilities["consumption_cny_fen"], capabilities["has_consumption"] = consumption.ConsumptionCNYFen, consumption.HasConsumption
		c.JSON(http.StatusOK, capabilities)
		return
	}
	var request struct {
		ClientID      string            `json:"client_id"`
		UserID        int               `json:"user_id"`
		Reference     string            `json:"reference"`
		Cents         int64             `json:"amount_cents"`
		RewardKind    common.RawMessage `json:"reward_kind"`
		ValidityHours common.RawMessage `json:"validity_hours"`
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") || common.DecodeJsonBody(c.Request.Body, &request) != nil || request.ClientID != origin {
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	var rewards []model.CommunityLotteryReward
	if strings.HasSuffix(c.Request.URL.Path, "/lottery/charity") || len(request.RewardKind) != 0 || len(request.ValidityHours) != 0 {
		var reward model.CommunityLotteryReward
		if len(request.RewardKind) == 0 || len(request.ValidityHours) == 0 ||
			common.Unmarshal(request.RewardKind, &reward.Kind) != nil ||
			common.Unmarshal(request.ValidityHours, &reward.ValidityHours) != nil ||
			reward.Kind != model.WalletCharity || reward.ValidityHours < 1 || reward.ValidityHours > 8760 {
			communitySSOError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		rewards = append(rewards, reward)
	}
	receipt, replayed, err := model.CreditCommunityLottery(request.UserID, request.Reference, request.Cents, rewards...)
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
		fields := gin.H{"id": receipt.Id, "quota_added": receipt.LotteryCredit.QuotaAdded, "quota_per_usd": receipt.LotteryCredit.QuotaPerUSD}
		if receipt.LotteryCredit.RewardKind == model.WalletCharity {
			fields["reward_kind"], fields["validity_hours"] = receipt.LotteryCredit.RewardKind, receipt.LotteryCredit.ValidityHours
			fields["expires_at"], fields["credited_at"] = receipt.LotteryCredit.ExpiresAt, receipt.LotteryCredit.CreditedAt
			fields["points"], fields["lot_id"], fields["quota_per_point"] = receipt.LotteryCredit.Points, receipt.LotteryCredit.LotID, receipt.LotteryCredit.QuotaPerPoint
		}
		snapshot = fields
	}
	c.JSON(http.StatusOK, gin.H{"credited": true, "reference": receipt.TradeNo, "user_id": receipt.UserId, "amount_cents": receipt.Amount, "replayed": replayed, "receipt": snapshot})
}
