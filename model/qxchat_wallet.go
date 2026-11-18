package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var walletReservationID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var sharedPointsPattern = regexp.MustCompile(`^\d{1,10}(?:\.\d{1,6})?$`)

func QXChatSharedWalletEnabled() bool { return os.Getenv("QXCHAT_SHARED_WALLET_ENABLED") == "1" }

// SQLite needs a write lock before any read, including reservation-status reads.
func lockWalletUser(tx *gorm.DB, userID int, user *User) error {
	if tx.Dialector.Name() == "sqlite" {
		if err := tx.Model(&User{}).Where("id = ?", userID).UpdateColumn("quota", gorm.Expr("quota")).Error; err != nil {
			return err
		}
	}
	return lockForUpdate(tx).First(user, userID).Error
}

func sharedWalletIdentity(tx *gorm.DB, userID int) (*QXChatIdentity, error) {
	if !QXChatSharedWalletEnabled() {
		return nil, nil
	}
	if !UnifiedWalletEnabled() || !common.QXChatSourceEnabled() {
		return nil, ErrWalletUnavailable
	}
	var identity QXChatIdentity
	err := tx.Where("user_id = ?", userID).First(&identity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if identity.State != QXChatIdentityActive {
		return nil, ErrWalletUnavailable
	}
	return &identity, nil
}

type sharedWalletSnapshot struct {
	Points    decimal.Decimal
	Quota     int64
	ExpiresAt *int64
}

func qxchatWalletBalance(tx *gorm.DB, userID int) (sharedWalletSnapshot, error) {
	result := sharedWalletSnapshot{Points: decimal.Zero}
	identity, err := sharedWalletIdentity(tx, userID)
	if err != nil || identity == nil {
		return result, err
	}
	var response struct {
		SourceID  string `json:"source_id"`
		UserID    int    `json:"newapi_user_id"`
		Points    string `json:"points"`
		ExpiresAt *int64 `json:"expires_at"`
	}
	request := map[string]any{"action": "balance", "source_id": identity.SourceID, "newapi_user_id": userID, "authority_version": identity.AuthorityVersion}
	if err := common.CallQXChatSource(tx.Statement.Context, common.QXChatSharedWalletPath, request, &response); err != nil {
		return result, ErrWalletUnavailable
	}
	if response.SourceID != identity.SourceID || response.UserID != userID || !sharedPointsPattern.MatchString(response.Points) || (response.ExpiresAt != nil && *response.ExpiresAt <= 0) {
		return result, ErrWalletInvariant
	}
	points, err := decimal.NewFromString(response.Points)
	if err != nil {
		return result, ErrWalletInvariant
	}
	rate, err := UnifiedWalletQuotaPerPoint()
	if err != nil || 1_000_000%rate != 0 {
		return result, ErrWalletUnavailable
	}
	quota, err := common.WalletQuotaFromDecimalStrict(points.Mul(decimal.NewFromInt(rate)).Truncate(0))
	if err != nil {
		return result, err
	}
	return sharedWalletSnapshot{Points: points, Quota: int64(quota), ExpiresAt: response.ExpiresAt}, nil
}

func reserveQXChatWallet(tx *gorm.DB, userID int, operation string, quota int64) (WalletAllocation, error) {
	allocation := WalletAllocation{Kind: WalletQXChat}
	identity, err := sharedWalletIdentity(tx, userID)
	if err != nil || identity == nil {
		return allocation, err
	}
	rate, err := UnifiedWalletQuotaPerPoint()
	if err != nil || 1_000_000%rate != 0 {
		return allocation, ErrWalletUnavailable
	}
	// Every SQL attempt gets a new reservation; rolled-back attempts keep their tombstones.
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return allocation, err
	}
	allocation.Reference = hex.EncodeToString(random[:])
	request := map[string]any{
		"action": "reserve", "source_id": identity.SourceID, "newapi_user_id": userID,
		"authority_version": identity.AuthorityVersion, "reservation_id": allocation.Reference,
		"operation_id": operation, "quota": quota, "quota_per_point": rate,
	}
	var response struct {
		ReservationID string `json:"reservation_id"`
		Quota         int64  `json:"quota"`
	}
	if err := common.CallQXChatSource(tx.Statement.Context, common.QXChatSharedWalletPath, request, &response); err != nil {
		return allocation, ErrWalletUnavailable
	}
	if response.ReservationID != allocation.Reference || response.Quota < 0 || response.Quota > quota {
		return allocation, ErrWalletInvariant
	}
	allocation.Quota = response.Quota
	return allocation, nil
}

type QXChatWalletReservationStatus struct {
	ReservationID string `json:"reservation_id"`
	OperationID   string `json:"operation_id"`
	UserID        int    `json:"newapi_user_id"`
	Quota         int64  `json:"quota"`
	Terminal      bool   `json:"terminal"`
}

// The user lock waits for in-flight reserves to commit or roll back. Never infer
// a release from an unlocked read, a deleted owner, or an unavailable database.
func GetQXChatWalletReservationStatus(request QXChatWalletReservationStatus) (*QXChatWalletReservationStatus, error) {
	if !UnifiedWalletEnabled() || !QXChatSharedWalletEnabled() {
		return nil, ErrWalletUnavailable
	}
	if request.UserID <= 0 || !walletReservationID.MatchString(request.ReservationID) || !walletReservationID.MatchString(request.OperationID) {
		return nil, ErrWalletConflict
	}
	result := QXChatWalletReservationStatus{ReservationID: request.ReservationID, OperationID: request.OperationID, UserID: request.UserID, Terminal: true}
	err := DB.Transaction(func(tx *gorm.DB) error {
		var owner User
		// Historical refunds remain valid if the account is banned or soft-deleted.
		if err := lockWalletUser(tx.Unscoped(), request.UserID, &owner); err != nil {
			return err
		}
		var operation UnifiedWalletOperation
		err := tx.First(&operation, "id = ?", request.OperationID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if operation.UserID != request.UserID || operation.Mode != WalletCommunity {
			return ErrWalletConflict
		}
		if operation.State != "reserved" && operation.State != "settled" && operation.State != "refunded" {
			return ErrWalletInvariant
		}
		for _, allocation := range operation.Allocations {
			if allocation.Kind == WalletQXChat && allocation.Reference == request.ReservationID {
				if allocation.Quota < 0 || allocation.Quota > int64(common.MaxWalletQuota) {
					return ErrWalletInvariant
				}
				result.Quota = allocation.Quota
				result.Terminal = operation.State != "reserved"
				return nil
			}
		}
		return nil
	})
	return &result, err
}
