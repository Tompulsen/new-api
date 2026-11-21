package model

import (
	"errors"
	"math"
	"regexp"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var lotteryReference = regexp.MustCompile(`^community_lottery_[a-zA-Z0-9]{1,32}_[0-4]$`)

var ErrLotteryCreditConflict = errors.New("lottery credit reference already has different parameters")

type CommunityLotteryCreditSnapshot struct {
	QuotaAdded    int    `json:"quota_added"`
	QuotaPerUSD   string `json:"quota_per_usd"`
	RewardKind    string `json:"reward_kind,omitempty"`
	ValidityHours int    `json:"validity_hours,omitempty"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
	CreditedAt    int64  `json:"credited_at,omitempty"`
	Points        string `json:"points,omitempty"`
	LotID         int64  `json:"lot_id,omitempty"`
	QuotaPerPoint int64  `json:"quota_per_point,omitempty"`
}

type CommunityLotteryReward struct {
	Kind          string
	ValidityHours int
}

type CommunityLotteryConsumption struct {
	ConsumptionCNYFen int64 `json:"consumption_cny_fen"`
	HasConsumption    bool  `json:"has_consumption"`
}

func GetCommunityLotteryConsumption(userID int) (*CommunityLotteryConsumption, error) {
	if userID <= 0 || userID > common.MaxWalletQuota {
		return nil, ErrInvalidTopUpQuota
	}
	var user User
	if err := DB.Select("id", "used_quota").Where("id = ? AND status = ?", userID, common.UserStatusEnabled).First(&user).Error; err != nil {
		return nil, err
	}
	rate, err := UnifiedWalletQuotaPerPoint()
	if err != nil {
		return nil, err
	}
	if user.UsedQuota <= 0 {
		return &CommunityLotteryConsumption{}, nil
	}
	// One CNY fen is 50 points; the validated rate bounds this denominator.
	fen := int64(user.UsedQuota) / (rate * 50)
	if fen > common.MaxWalletQuota {
		return nil, ErrWalletQuotaLimitExceeded
	}
	return &CommunityLotteryConsumption{ConsumptionCNYFen: fen, HasConsumption: true}, nil
}

func communityLotteryReceiptMatches(receipt *TopUp, userID int, cents int64, reward CommunityLotteryReward) bool {
	if receipt.UserId != userID || receipt.Amount != cents || receipt.PaymentProvider != "community_lottery" || receipt.Status != common.TopUpStatusSuccess {
		return false
	}
	if receipt.LotteryCredit == nil {
		return reward.Kind == "" && reward.ValidityHours == 0
	}
	return receipt.LotteryCredit.RewardKind == reward.Kind && receipt.LotteryCredit.ValidityHours == reward.ValidityHours
}

// CreditCommunityLottery commits the unique receipt and its reward together.
// Omitting the reward keeps pending deliveries on their original permanent path.
func CreditCommunityLottery(userID int, reference string, cents int64, rewards ...CommunityLotteryReward) (*TopUp, bool, error) {
	if userID <= 0 || !lotteryReference.MatchString(reference) || cents < 1 || cents > 1000000 {
		return nil, false, ErrInvalidTopUpQuota
	}
	var reward CommunityLotteryReward
	if len(rewards) > 1 {
		return nil, false, ErrInvalidTopUpQuota
	}
	if len(rewards) == 1 {
		reward = rewards[0]
		if reward.Kind != WalletCharity || reward.ValidityHours < 1 || reward.ValidityHours > 8760 {
			return nil, false, ErrInvalidTopUpQuota
		}
	}
	var receipt TopUp
	var quota int
	alreadyDone := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockWalletUser(tx, userID, &user); err != nil {
			return err
		}
		err := tx.Where("trade_no = ?", reference).First(&receipt).Error
		if err == nil {
			if !communityLotteryReceiptMatches(&receipt, userID, cents, reward) {
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
		var quotaPerPoint int64
		if reward.Kind == WalletCharity {
			if !UnifiedWalletEnabled() {
				return ErrWalletUnavailable
			}
			quotaPerPoint, err = UnifiedWalletQuotaPerPoint()
			if err != nil {
				return err
			}
		}
		rate := common.QuotaPerUnit
		if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return ErrInvalidTopUpQuota
		}
		ratio := decimal.NewFromFloat(rate)
		rewardQuota := decimal.NewFromInt(cents).Div(decimal.NewFromInt(100)).Mul(ratio)
		if reward.Kind == WalletCharity && !rewardQuota.Equal(rewardQuota.Truncate(0)) {
			return ErrInvalidTopUpQuota
		}
		quota, err = common.WalletQuotaFromDecimalStrict(rewardQuota)
		if err != nil || quota <= 0 {
			return ErrInvalidTopUpQuota
		}
		now := common.GetTimestamp()
		snapshot := &CommunityLotteryCreditSnapshot{QuotaAdded: quota, QuotaPerUSD: ratio.String()}
		if reward.Kind == WalletCharity {
			duration := int64(reward.ValidityHours) * 3600
			if int64(quota)%quotaPerPoint != 0 || now <= 0 || now > int64(common.MaxWalletQuota)-duration {
				return ErrInvalidTopUpQuota
			}
			var charityQuota int64
			if err := tx.Model(&UnifiedWalletLot{}).Select("COALESCE(SUM(remaining_quota), 0)").Where("user_id = ? AND kind = ? AND (expires_at = 0 OR expires_at > ?)", userID, WalletCharity, now).Scan(&charityQuota).Error; err != nil {
				return err
			}
			if charityQuota < 0 || charityQuota > int64(common.MaxWalletQuota)-int64(quota) {
				return ErrWalletQuotaLimitExceeded
			}
			points := int64(quota) / quotaPerPoint
			lot := UnifiedWalletLot{UserID: userID, Kind: WalletCharity, Reference: WalletReference("community_lottery", reference), Source: "community_lottery", InitialQuota: int64(quota), RemainingQuota: int64(quota), InitialPoints: points, CreatedAt: now, ExpiresAt: now + duration}
			if err := tx.Create(&lot).Error; err != nil {
				return err
			}
			if lot.ID <= 0 || lot.ID > int64(common.MaxWalletQuota) {
				return ErrWalletQuotaLimitExceeded
			}
			snapshot.RewardKind, snapshot.ValidityHours = reward.Kind, reward.ValidityHours
			snapshot.CreditedAt, snapshot.ExpiresAt = lot.CreatedAt, lot.ExpiresAt
			snapshot.Points, snapshot.LotID, snapshot.QuotaPerPoint = strconv.FormatInt(points, 10), lot.ID, quotaPerPoint
			if err := walletEntry(tx, userID, lot.Reference, WalletAllocation{Kind: WalletCharity, LotID: lot.ID}, "grant", lot.InitialQuota); err != nil {
				return err
			}
		}
		// Amount records the original USD cents for this provider; Money is zero
		// because a promotional reward is not a payment received.
		receipt = TopUp{UserId: userID, Amount: cents, Money: 0, TradeNo: reference, PaymentMethod: "community_lottery", PaymentProvider: "community_lottery", CreateTime: now, CompleteTime: now, Status: common.TopUpStatusSuccess}
		receipt.LotteryCredit = snapshot
		if err := tx.Create(&receipt).Error; err != nil {
			return err
		}
		if reward.Kind == WalletCharity {
			return nil
		}
		return creditTopUpQuota(tx, userID, quota, nil)
	})
	if err != nil {
		// A different recipient can race on the same globally unique trade number.
		var receipt TopUp
		if DB.Where("trade_no = ?", reference).First(&receipt).Error == nil {
			if communityLotteryReceiptMatches(&receipt, userID, cents, reward) {
				return &receipt, true, nil
			}
			return nil, false, ErrLotteryCreditConflict
		}
		return nil, false, err
	}
	if !alreadyDone {
		if reward.Kind != WalletCharity {
			// shortcut: existing top-up cache synchronization can wait for cache expiry after a crash; replay must never apply a second delta.
			syncCreditUserQuotaCache(userID, quota, "community lottery")
		}
		RecordTopupLog(userID, common.NewMessage("Community lottery reward: {{amount}} USD", map[string]any{"amount": decimal.NewFromInt(cents).Div(decimal.NewFromInt(100)).StringFixed(2)}), "", "community_lottery", "community_lottery")
	}
	return &receipt, alreadyDone, nil
}
