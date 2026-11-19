package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var qxChatInvitePattern = regexp.MustCompile(`^[A-Z0-9]{8,32}$`)
var ErrQXChatInvitationInvalid = errors.New("QXChat invitation code is invalid or no longer available")

func ValidateQXChatInvitation(ctx context.Context, code string) error {
	if !qxChatInvitePattern.MatchString(code) {
		return ErrQXChatInvitationInvalid
	}
	var receipt struct {
		Valid bool `json:"valid"`
	}
	err := common.CallQXChatSource(ctx, common.QXChatReferralCheckPath, map[string]string{"invite_code": code}, &receipt)
	var failure *common.QXChatSourceError
	if errors.As(err, &failure) && failure.Code == "invalid_invitation" {
		return ErrQXChatInvitationInvalid
	}
	if err != nil {
		return err
	}
	if !receipt.Valid {
		return ErrQXChatInvitationInvalid
	}
	return nil
}

// Called only inside the new-user registration transaction, after a local email-code claim.
func RecordQXChatRegistrationInvitationWithTx(tx *gorm.DB, user *User, code string) error {
	if code == "" {
		return nil
	}
	if !qxChatInvitePattern.MatchString(code) {
		return ErrQXChatInvitationInvalid
	}
	if err := validateQXChatVerifiedEmailWithTx(tx, *user); err != nil {
		return err
	}
	mapping := QXChatIdentity{SourceID: uuid.NewString(), UserID: user.Id, SourceUsername: strings.ToLower(user.Username),
		RequestID: uuid.NewString(), AuthorityVersion: 1, State: QXChatIdentityProvisioning,
		RegistrationInviteCode: code, ReferralStatus: "pending"}
	digest := sha256.Sum256([]byte(mapping.SourceID + "\n" + mapping.RequestID))
	mapping.CredentialVersion = hex.EncodeToString(digest[:])
	return tx.Create(&mapping).Error
}

func deliverQXChatReferral(expected QXChatIdentity) error {
	var profile QXChatRegistrationProfile
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).First(&user, expected.UserID).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrQXChatIdentityInactive
		}
		var mapping QXChatIdentity
		if err := tx.Where("source_id = ? AND user_id = ? AND request_id = ? AND credential_version = ? AND imported = ?", expected.SourceID, expected.UserID, expected.RequestID, expected.CredentialVersion, false).First(&mapping).Error; err != nil {
			return err
		}
		if mapping.ReferralStatus != "pending" || (mapping.State != QXChatIdentityProvisioning && mapping.State != QXChatIdentityActive) {
			return ErrQXChatIdentityInactive
		}
		if err := validateQXChatVerifiedEmailWithTx(tx, user); err != nil {
			return err
		}
		profile = QXChatRegistrationProfile{RequestID: mapping.RequestID, SourceID: mapping.SourceID, NewAPIUserID: mapping.UserID,
			AuthorityVersion: mapping.AuthorityVersion, CredentialVersion: mapping.CredentialVersion, Username: mapping.SourceUsername,
			Email: mapping.SourceEmail, DisplayName: user.DisplayName, IdentityOnly: mapping.SourceEmail == "",
			RegistrationInviteCode: mapping.RegistrationInviteCode, RegistrationEmailVerified: true}
		return nil
	})
	if err != nil {
		return err
	}
	var receipt struct {
		RequestID         string `json:"request_id"`
		SourceID          string `json:"source_id"`
		UserID            int    `json:"newapi_user_id"`
		Authority         string `json:"authority"`
		AuthorityVersion  int64  `json:"authority_version"`
		ReferralProcessed bool   `json:"referral_processed"`
		InviteCode        string `json:"invite_code"`
		ReferralStatus    string `json:"referral_status"`
	}
	if err := common.CallQXChatSource(context.Background(), "/api/account-unification/provision", profile, &receipt); err != nil {
		return err
	}
	if receipt.RequestID != expected.RequestID || receipt.SourceID != expected.SourceID || receipt.UserID != expected.UserID || receipt.Authority != "newapi" || receipt.AuthorityVersion != expected.AuthorityVersion ||
		!receipt.ReferralProcessed || receipt.InviteCode != expected.RegistrationInviteCode || (receipt.ReferralStatus != "awarded" && receipt.ReferralStatus != "ineligible") {
		return common.ErrQXChatUnavailable
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).First(&user, expected.UserID).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrQXChatIdentityInactive
		}
		result := tx.Model(&QXChatIdentity{}).Where("source_id = ? AND user_id = ? AND request_id = ? AND credential_version = ? AND authority_version = ? AND imported = ? AND registration_invite_code = ? AND referral_status = ? AND state IN ?",
			expected.SourceID, expected.UserID, expected.RequestID, expected.CredentialVersion, expected.AuthorityVersion, false, expected.RegistrationInviteCode, "pending", []string{QXChatIdentityProvisioning, QXChatIdentityActive}).Updates(map[string]any{"state": QXChatIdentityActive, "referral_status": receipt.ReferralStatus})
		return result.Error
	})
}

func ProcessQXChatReferralRewards() error {
	if !common.QXChatSourceEnabled() {
		return nil
	}
	var pending []QXChatIdentity
	if err := DB.Where("referral_status = ? AND referral_next_attempt <= ?", "pending", time.Now().Unix()).Order("created_at").Limit(50).Find(&pending).Error; err != nil {
		return err
	}
	var first error
	for _, mapping := range pending {
		// Claim a bounded retry window before crossing the service boundary; a crashed worker is recoverable.
		result := DB.Model(&QXChatIdentity{}).Where("source_id = ? AND referral_status = ? AND referral_next_attempt <= ?", mapping.SourceID, "pending", time.Now().Unix()).Update("referral_next_attempt", time.Now().Add(2*time.Minute).Unix())
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			continue
		}
		if err := deliverQXChatReferral(mapping); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func RunQXChatReferralRewards() {
	for {
		if err := ProcessQXChatReferralRewards(); err != nil {
			common.SysLog("QXChat referral delivery pending; will retry")
		}
		time.Sleep(time.Minute)
	}
}
