package model

import (
	"errors"
	"math"
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var lotteryReference = regexp.MustCompile(`^community_lottery_[a-zA-Z0-9]{1,32}_[0-4]$`)

var ErrLotteryCreditConflict = errors.New("lottery credit reference already has different parameters")

type CommunityLotteryCreditSnapshot struct {
	QuotaAdded  int    `json:"quota_added"`
	QuotaPerUSD string `json:"quota_per_usd"`
}

// CreditCommunityLottery reuses the top-up receipt's unique trade number. The
// receipt and balance are committed together; transport retries cannot recredit.
func CreditCommunityLottery(userID int, reference string, cents int64) (*TopUp, bool, error) {
	if userID <= 0 || !lotteryReference.MatchString(reference) || cents < 1 || cents > 1000000 {
		return nil, false, ErrInvalidTopUpQuota
	}
	var receipt TopUp
	var quota int
	alreadyDone := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
			return err
		}
		err := tx.Where("trade_no = ?", reference).First(&receipt).Error
		if err == nil {
			if receipt.UserId != userID || receipt.Amount != cents || receipt.PaymentProvider != "community_lottery" || receipt.Status != common.TopUpStatusSuccess {
				return ErrLotteryCreditConflict
			}
			alreadyDone = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrInvalidTopUpQuota
		}
		rate := common.QuotaPerUnit
		if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return ErrInvalidTopUpQuota
		}
		ratio := decimal.NewFromFloat(rate)
		quota, err = common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(cents).Div(decimal.NewFromInt(100)).Mul(ratio))
		if err != nil || quota <= 0 {
			return ErrInvalidTopUpQuota
		}
		now := common.GetTimestamp()
		// Amount records the original USD cents for this provider; Money is zero
		// because a promotional reward is not a payment received.
		receipt = TopUp{UserId: userID, Amount: cents, Money: 0, TradeNo: reference, PaymentMethod: "community_lottery", PaymentProvider: "community_lottery", CreateTime: now, CompleteTime: now, Status: common.TopUpStatusSuccess}
		receipt.LotteryCredit = &CommunityLotteryCreditSnapshot{QuotaAdded: quota, QuotaPerUSD: ratio.String()}
		if err := tx.Create(&receipt).Error; err != nil {
			return err
		}
		return creditTopUpQuota(tx, userID, quota, nil)
	})
	if err != nil {
		// A different recipient can race on the same globally unique trade number.
		var receipt TopUp
		if DB.Where("trade_no = ?", reference).First(&receipt).Error == nil {
			if receipt.UserId == userID && receipt.Amount == cents && receipt.PaymentProvider == "community_lottery" && receipt.Status == common.TopUpStatusSuccess {
				return &receipt, true, nil
			}
			return nil, false, ErrLotteryCreditConflict
		}
		return nil, false, err
	}
	if !alreadyDone {
		// shortcut: existing top-up cache synchronization can wait for cache expiry after a crash; replay must never apply a second delta.
		syncCreditUserQuotaCache(userID, quota, "community lottery")
		RecordTopupLog(userID, common.NewMessage("Community lottery reward: {{amount}} USD", map[string]any{"amount": decimal.NewFromInt(cents).Div(decimal.NewFromInt(100)).StringFixed(2)}), "", "community_lottery", "community_lottery")
	}
	return &receipt, alreadyDone, nil
}
