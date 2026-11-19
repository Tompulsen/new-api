package model

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var communityReferralCodePattern = regexp.MustCompile(`^wf[A-Za-z0-9]{24}$`)

// Configuration revisions are immutable, including the actor who changed them.
type CommunityReferralConfig struct {
	Origin        string `json:"-" gorm:"type:varchar(191);primaryKey"`
	Version       int64  `json:"version" gorm:"primaryKey;autoIncrement:false"`
	Enabled       bool   `json:"enabled"`
	InviterPoints int64  `json:"inviterPoints" gorm:"type:bigint;not null"`
	InviteePoints int64  `json:"inviteePoints" gorm:"type:bigint;not null"`
	UpdatedBy     int    `json:"-"`
	UpdatedAt     int64  `json:"updatedAt" gorm:"type:bigint;not null"`
}

type CommunityReferralCode struct {
	Code   string `gorm:"type:varchar(32);primaryKey"`
	Origin string `gorm:"type:varchar(191);uniqueIndex:idx_community_referral_owner,priority:1;not null"`
	UserID int    `gorm:"uniqueIndex:idx_community_referral_owner,priority:2;not null"`
}

// The invitee primary key is also the reward receipt's idempotency boundary.
type CommunityReferral struct {
	InviteeID     int    `json:"inviteeId" gorm:"primaryKey;autoIncrement:false"`
	InviterID     int    `json:"inviterId" gorm:"index;not null"`
	Origin        string `json:"-" gorm:"type:varchar(191);not null"`
	ConfigVersion int64  `json:"configVersion" gorm:"type:bigint;not null"`
	InviterPoints int64  `json:"inviterPoints" gorm:"type:bigint;not null"`
	InviteePoints int64  `json:"inviteePoints" gorm:"type:bigint;not null"`
	InviterQuota  int64  `json:"-" gorm:"type:bigint;not null"`
	InviteeQuota  int64  `json:"-" gorm:"type:bigint;not null"`
	BoundAt       int64  `json:"boundAt" gorm:"type:bigint;not null"`
	State         string `json:"state" gorm:"type:varchar(24);not null"`
	TopUpID       int    `json:"-"`
	CreditedAt    int64  `json:"creditedAt" gorm:"type:bigint;not null"`
	NextCheckAt   int64  `json:"-" gorm:"type:bigint;index;not null"`
	Attempts      int    `json:"-"`
}

func GetCommunityReferralConfig(origin string) (*CommunityReferralConfig, error) {
	if origin == "" || len(origin) > 191 {
		return nil, ErrWalletConflict
	}
	initial := CommunityReferralConfig{Origin: origin, Version: 1, Enabled: true, InviterPoints: 5000, InviteePoints: 5000, UpdatedAt: time.Now().Unix()}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
		return nil, err
	}
	var config CommunityReferralConfig
	err := DB.Where("origin = ?", origin).Order("version DESC").First(&config).Error
	return &config, err
}

func UpdateCommunityReferralConfig(origin string, actor int, expected int64, enabled bool, inviterPoints, inviteePoints int64) (*CommunityReferralConfig, error) {
	if actor <= 0 || expected < 1 || inviterPoints < 1 || inviteePoints < 1 || inviterPoints > 100000000 || inviteePoints > 100000000 {
		return nil, ErrInvalidTopUpQuota
	}
	rate, err := UnifiedWalletQuotaPerPoint()
	if err != nil {
		return nil, err
	}
	for _, points := range []int64{inviterPoints, inviteePoints} {
		if _, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(points).Mul(decimal.NewFromInt(rate))); err != nil {
			return nil, err
		}
	}
	current, err := GetCommunityReferralConfig(origin)
	if err != nil {
		return nil, err
	}
	if current.Version != expected {
		return nil, ErrWalletConflict
	}
	config := CommunityReferralConfig{Origin: origin, Version: expected + 1, Enabled: enabled, InviterPoints: inviterPoints, InviteePoints: inviteePoints, UpdatedBy: actor, UpdatedAt: time.Now().Unix()}
	result := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&config)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrWalletConflict
	}
	return &config, nil
}

// Called exclusively inside the new-user insert transaction, never on login or profile provisioning.
func bindCommunityReferralTx(tx *gorm.DB, user *User) error {
	code := user.CommunityInviteCode
	if !communityReferralCodePattern.MatchString(code) || !UnifiedWalletEnabled() {
		return nil
	}
	var invitation CommunityReferralCode
	err := tx.First(&invitation, "code = ?", code).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if invitation.UserID == user.Id {
		return ErrWalletConflict
	}
	var inviter User
	if err := tx.Where("id = ? AND status = ?", invitation.UserID, common.UserStatusEnabled).First(&inviter).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var config CommunityReferralConfig
	if err := tx.Where("origin = ?", invitation.Origin).Order("version DESC").First(&config).Error; err != nil {
		return err
	}
	if !config.Enabled {
		return nil
	}
	rate, err := UnifiedWalletQuotaPerPoint()
	if err != nil {
		return err
	}
	inviterQuota, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(config.InviterPoints).Mul(decimal.NewFromInt(rate)))
	if err != nil {
		return err
	}
	inviteeQuota, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(config.InviteePoints).Mul(decimal.NewFromInt(rate)))
	if err != nil {
		return err
	}
	return tx.Create(&CommunityReferral{InviteeID: user.Id, InviterID: invitation.UserID, Origin: invitation.Origin,
		ConfigVersion: config.Version, InviterPoints: config.InviterPoints, InviteePoints: config.InviteePoints,
		InviterQuota: int64(inviterQuota), InviteeQuota: int64(inviteeQuota), BoundAt: time.Now().Unix(), State: "waiting_topup"}).Error
}

type CommunityReferralOverview struct {
	Config     *CommunityReferralConfig `json:"config"`
	Code       string                   `json:"code"`
	Invited    int64                    `json:"invited"`
	Credited   int64                    `json:"credited"`
	Pending    int64                    `json:"pending"`
	Earned     int64                    `json:"earnedPoints"`
	AsInvitee  *CommunityReferral       `json:"asInvitee"`
	Items      []CommunityReferral      `json:"items"`
	NextCursor int                      `json:"nextCursor"`
}

func GetCommunityReferralOverview(origin string, userID, cursor int) (*CommunityReferralOverview, error) {
	config, err := GetCommunityReferralConfig(origin)
	if err != nil {
		return nil, err
	}
	view := &CommunityReferralOverview{Config: config, Items: []CommunityReferral{}}
	if userID == 0 {
		return view, nil
	}
	var user User
	if err := DB.Where("id = ? AND status = ?", userID, common.UserStatusEnabled).First(&user).Error; err != nil {
		return nil, err
	}
	code := CommunityReferralCode{Code: "wf" + common.GetRandomString(24), Origin: origin, UserID: userID}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&code).Error; err != nil {
		return nil, err
	}
	code = CommunityReferralCode{}
	if err := DB.Where("origin = ? AND user_id = ?", origin, userID).First(&code).Error; err != nil {
		return nil, err
	}
	view.Code = code.Code
	var totals struct{ Invited, Credited, Pending, Earned int64 }
	err = DB.Model(&CommunityReferral{}).Where("origin = ? AND inviter_id = ?", origin, userID).
		Select("COUNT(*) AS invited, COALESCE(SUM(CASE WHEN credited_at > 0 THEN 1 ELSE 0 END),0) AS credited, COALESCE(SUM(CASE WHEN state = 'pending' THEN 1 ELSE 0 END),0) AS pending, COALESCE(SUM(CASE WHEN credited_at > 0 THEN inviter_points ELSE 0 END),0) AS earned").Scan(&totals).Error
	if err != nil {
		return nil, err
	}
	view.Invited, view.Credited, view.Pending, view.Earned = totals.Invited, totals.Credited, totals.Pending, totals.Earned
	var own CommunityReferral
	err = DB.Where("origin = ? AND invitee_id = ?", origin, userID).First(&own).Error
	if err == nil {
		view.AsInvitee = &own
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	query := DB.Where("origin = ? AND inviter_id = ?", origin, userID)
	if cursor > 0 {
		query = query.Where("invitee_id < ?", cursor)
	}
	if err := query.Order("invitee_id DESC").Limit(21).Find(&view.Items).Error; err != nil {
		return nil, err
	}
	if len(view.Items) > 20 {
		view.NextCursor = view.Items[19].InviteeID
		view.Items = view.Items[:20]
	}
	return view, nil
}

func creditCommunityReferral(inviteeID int) error {
	if !UnifiedWalletEnabled() {
		return ErrWalletUnavailable
	}
	qualified := false
	var credited *CommunityReferral
	err := DB.Transaction(func(tx *gorm.DB) error {
		var referral CommunityReferral
		if err := lockForUpdate(tx).First(&referral, "invitee_id = ?", inviteeID).Error; err != nil {
			return err
		}
		if referral.CreditedAt > 0 {
			return nil
		}
		var topup TopUp
		err := lockForUpdate(tx).Where("user_id = ? AND status = ? AND create_time >= ? AND complete_time >= ? AND payment_verified = ? AND payment_positive = ? AND payment_provider IN ? AND payment_reference <> '' AND (payment_currency <> 'CNY' OR payment_principal_fen > payment_refunded_fen)",
			inviteeID, common.TopUpStatusSuccess, referral.BoundAt, referral.BoundAt, true, true,
			[]string{PaymentProviderEpay, PaymentProviderStripe, PaymentProviderCreem, PaymentProviderWaffo, PaymentProviderWaffoPancake}).Order("complete_time ASC, id ASC").First(&topup).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Model(&referral).Updates(map[string]any{"state": "waiting_topup", "next_check_at": time.Now().Unix() + 60}).Error
		}
		if err != nil {
			return err
		}
		qualified = true
		if err := recordVerifiedPaymentTx(tx, &topup); err != nil {
			return err
		}
		// Stable lock order prevents two referral chains deadlocking on shared wallets.
		for _, id := range []int{min(referral.InviterID, referral.InviteeID), max(referral.InviterID, referral.InviteeID)} {
			var user User
			if err := lockForUpdate(tx).Where("id = ? AND status = ?", id, common.UserStatusEnabled).First(&user).Error; err != nil {
				return err
			}
		}
		reference := WalletReference("community-referral", strconv.Itoa(inviteeID))
		if err := UnifiedWalletDeltaTx(tx, referral.InviterID, referral.InviterQuota, false, reference); err != nil {
			return err
		}
		if err := UnifiedWalletDeltaTx(tx, referral.InviteeID, referral.InviteeQuota, false, reference); err != nil {
			return err
		}
		referral.CreditedAt, referral.TopUpID, referral.State = time.Now().Unix(), topup.Id, "credited"
		if err := tx.Save(&referral).Error; err != nil {
			return err
		}
		credited = &referral
		return nil
	})
	if err != nil {
		updates := map[string]any{"next_check_at": time.Now().Unix() + 60, "attempts": gorm.Expr("attempts + 1")}
		if qualified {
			updates["state"] = "pending"
		}
		DB.Model(&CommunityReferral{}).Where("invitee_id = ? AND credited_at = 0", inviteeID).Updates(updates)
		return err
	}
	if credited != nil {
		syncCreditUserQuotaCache(credited.InviterID, int(credited.InviterQuota), "community referral")
		syncCreditUserQuotaCache(credited.InviteeID, int(credited.InviteeQuota), "community referral")
	}
	return nil
}

func ProcessCommunityReferralRewards() error {
	if !UnifiedWalletEnabled() {
		return nil
	}
	var pending []CommunityReferral
	if err := DB.Where("credited_at = 0 AND next_check_at <= ?", time.Now().Unix()).Order("next_check_at ASC, invitee_id ASC").Limit(100).Find(&pending).Error; err != nil {
		return err
	}
	var firstError error
	for _, referral := range pending {
		if err := creditCommunityReferral(referral.InviteeID); err != nil && firstError == nil {
			firstError = fmt.Errorf("referral %d: %w", referral.InviteeID, err)
		}
	}
	return firstError
}

func RunCommunityReferralRewards() {
	for {
		if err := ProcessCommunityReferralRewards(); err != nil {
			common.SysError("community referral reward processing failed: " + err.Error())
		}
		time.Sleep(5 * time.Second)
	}
}
