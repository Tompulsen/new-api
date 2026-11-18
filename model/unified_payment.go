package model

import (
	"errors"
	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"strconv"
	"strings"
	"time"
)

// Only signature-verified provider callbacks construct these payment facts.
type VerifiedTopUpPayment struct {
	Currency     string
	PrincipalFen int64
	Reference    string
}

func VerifiedPaymentMajor(currency, amount, reference string) (VerifiedTopUpPayment, error) {
	fact := VerifiedTopUpPayment{Currency: strings.ToUpper(currency), Reference: reference}
	if fact.Currency != "CNY" {
		return fact, nil
	}
	if strings.TrimSpace(reference) == "" || len(reference) > 191 {
		return fact, ErrInvalidTopUpQuota
	}
	value, err := decimal.NewFromString(amount)
	if err != nil || !value.IsPositive() {
		return fact, ErrInvalidTopUpQuota
	}
	fen := value.Mul(decimal.NewFromInt(100))
	if !fen.Equal(fen.Truncate(0)) || fen.GreaterThan(decimal.NewFromInt(int64(common.MaxWalletQuota)/50)) {
		return fact, ErrInvalidTopUpQuota
	}
	fact.PrincipalFen = fen.IntPart()
	return fact, nil
}

func creditCompletedTopUp(tx *gorm.DB, topup *TopUp, quota int, updates map[string]any, payment ...VerifiedTopUpPayment) error {
	if len(payment) > 1 {
		return ErrWalletConflict
	}
	if len(payment) == 1 {
		fact := payment[0]
		if len(fact.Currency) > 8 || len(fact.Reference) > 191 || (fact.Currency == "CNY" && (fact.PrincipalFen <= 0 || fact.PrincipalFen > int64(common.MaxWalletQuota)/50 || strings.TrimSpace(fact.Reference) == "")) {
			return ErrInvalidTopUpQuota
		}
		// Epay's signed amount must match the frozen order amount, not current prices.
		if fact.Currency == "CNY" && topup.PaymentProvider == PaymentProviderEpay {
			frozen, err := decimal.NewFromString(strconv.FormatFloat(topup.Money, 'f', 2, 64))
			if err != nil || !frozen.Equal(decimal.NewFromInt(fact.PrincipalFen).Div(decimal.NewFromInt(100))) {
				return ErrWalletConflict
			}
		}
		topup.PaymentCurrency = fact.Currency
		topup.PaymentPrincipalFen = 0
		if fact.Currency == "CNY" {
			topup.PaymentPrincipalFen = fact.PrincipalFen
		}
		topup.PaymentReference = fact.Reference
		topup.PaymentVerified = fact.Currency != ""
		if err := tx.Model(topup).Updates(map[string]any{"payment_currency": topup.PaymentCurrency, "payment_principal_fen": topup.PaymentPrincipalFen, "payment_reference": topup.PaymentReference, "payment_verified": topup.PaymentVerified}).Error; err != nil {
			return err
		}
	}
	if UnifiedWalletEnabled() {
		return CreditVerifiedPaymentTx(tx, topup.UserId, quota, topup, updates)
	}
	return creditTopUpQuota(tx, topup.UserId, quota, updates)
}

// A verified provider payment can fund only one order, across users and replicas.
type UnifiedWalletPaymentReceipt struct {
	ID        string `gorm:"type:varchar(64);primaryKey"`
	TopUpID   int    `gorm:"uniqueIndex;not null"`
	CreatedAt int64  `gorm:"type:bigint;not null"`
}

func recordVerifiedPaymentTx(tx *gorm.DB, topup *TopUp) error {
	if topup.Id <= 0 || topup.PaymentProvider == "" || strings.TrimSpace(topup.PaymentReference) == "" || len(topup.PaymentReference) > 191 {
		return ErrWalletConflict
	}
	id := WalletReference("payment-receipt", topup.PaymentProvider, topup.PaymentReference)
	var receipt UnifiedWalletPaymentReceipt
	err := tx.First(&receipt, "id = ?", id).Error
	if err == nil {
		if receipt.TopUpID == topup.Id {
			return nil
		}
		return ErrWalletConflict
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.Create(&UnifiedWalletPaymentReceipt{ID: id, TopUpID: topup.Id, CreatedAt: time.Now().Unix()}).Error
}

type UnifiedWalletPaymentReversal struct {
	ID           string `gorm:"type:varchar(64);primaryKey"`
	TopUpID      int    `gorm:"index;not null"`
	PrincipalFen int64  `gorm:"type:bigint;not null"`
	CreatedAt    int64  `gorm:"type:bigint;not null"`
}

// Reversals revoke only their original paid lot; already spent value becomes
// legacy debt, which offsets further paid spending. Charity never pays it.
func ReverseVerifiedPayment(provider, reference, eventID string, fen int64, cumulative bool) error {
	if !UnifiedWalletEnabled() {
		return nil
	}
	if reference == "" || eventID == "" || fen <= 0 || fen > int64(common.MaxWalletQuota)/50 {
		return ErrWalletConflict
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var topup TopUp
		if err := lockForUpdate(tx).Where("payment_provider = ? AND payment_reference = ? AND payment_verified = ?", provider, reference, true).First(&topup).Error; err != nil {
			return err
		}
		if topup.PaymentCurrency != "CNY" {
			return nil
		}
		if topup.Status != common.TopUpStatusSuccess {
			return ErrWalletConflict
		}
		user, account, err := lockUnifiedWallet(tx, topup.UserId)
		if err != nil {
			return err
		}
		id := WalletReference("payment-reversal", provider, eventID)
		var old UnifiedWalletPaymentReversal
		err = tx.First(&old, "id = ?", id).Error
		if err == nil {
			if old.TopUpID == topup.Id && old.PrincipalFen == fen {
				return nil
			}
			return ErrWalletConflict
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		total := fen
		if !cumulative {
			total += topup.PaymentRefundedFen
		}
		if total > topup.PaymentPrincipalFen {
			return ErrWalletConflict
		}
		if cumulative && total < topup.PaymentRefundedFen {
			return nil
		}
		var lot UnifiedWalletLot
		if err := lockForUpdate(tx).Where("reference = ? AND user_id = ? AND kind = ?", WalletReference("topup", topup.TradeNo), topup.UserId, WalletPaid).First(&lot).Error; err != nil {
			return err
		}
		target := decimal.NewFromInt(lot.InitialQuota).Mul(decimal.NewFromInt(total)).Div(decimal.NewFromInt(topup.PaymentPrincipalFen)).Ceil().IntPart()
		delta := target - lot.ReversedQuota
		if delta < 0 {
			return ErrWalletConflict
		}
		remaining := min(delta, lot.RemainingQuota)
		account.PaidQuota -= remaining
		account.LegacyQuota -= delta - remaining
		lot.RemainingQuota -= remaining
		lot.ReversedQuota = target
		if err := tx.Save(&lot).Error; err != nil {
			return err
		}
		if err := walletEntry(tx, topup.UserId, id, WalletAllocation{Kind: WalletPaid, LotID: lot.ID}, "payment_reversal", -remaining); err != nil {
			return err
		}
		if delta > remaining {
			if err := walletEntry(tx, topup.UserId, id, WalletAllocation{Kind: WalletLegacy}, "payment_reversal_debt", -(delta - remaining)); err != nil {
				return err
			}
		}
		if err := saveUnifiedWallet(tx, user, account); err != nil {
			return err
		}
		if err := tx.Model(&topup).Update("payment_refunded_fen", total).Error; err != nil {
			return err
		}
		return tx.Create(&UnifiedWalletPaymentReversal{ID: id, TopUpID: topup.Id, PrincipalFen: fen, CreatedAt: time.Now().Unix()}).Error
	})
}
