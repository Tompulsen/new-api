package model

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const QXChatIdentityPending = "pending"
const QXChatIdentityActive = "active"
const QXChatUserPendingStatus = 3

var ErrQXChatIdentityConflict = errors.New("QXChat identity requires explicit account binding")
var ErrQXChatIdentityInactive = errors.New("QXChat identity is not active")
var qxchatSourceID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var qxchatCredentialVersion = regexp.MustCompile(`^[0-9a-f]{64}$`)

// A retained row also prevents a deleted target account from being re-imported.
type QXChatIdentity struct {
	SourceID          string `json:"source_id" gorm:"type:varchar(36);primaryKey"`
	UserID            int    `json:"newapi_user_id" gorm:"not null;uniqueIndex"`
	SourceUsername    string `json:"-" gorm:"type:varchar(32);not null;index"`
	SourceEmail       string `json:"-" gorm:"type:varchar(191);not null;index"`
	RequestID         string `json:"-" gorm:"type:varchar(36);not null"`
	CredentialVersion string `json:"-" gorm:"type:varchar(64);not null"`
	AuthorityVersion  int64  `json:"authority_version" gorm:"not null"`
	State             string `json:"state" gorm:"type:varchar(16);not null"`
	Imported          bool   `json:"-" gorm:"not null"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type QXChatSourceProfile struct {
	RequestID         string `json:"request_id"`
	SourceID          string `json:"source_id"`
	Username          string `json:"username"`
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	PasswordHash      string `json:"password_hash"`
	CredentialVersion string `json:"credential_version"`
	Authority         string `json:"authority"`
	NewAPIUserID      int    `json:"newapi_user_id"`
	Eligible          bool   `json:"eligible"`
	EmailVerified     bool   `json:"email_verified"`
}

func MigrateQXChatIdentity(db *gorm.DB) error {
	return db.AutoMigrate(&QXChatIdentity{})
}

func ResolveQXChatIdentity(sourceID string) (*QXChatIdentity, error) {
	if !qxchatSourceID.MatchString(sourceID) {
		return nil, ErrQXChatIdentityInactive
	}
	var identity QXChatIdentity
	if err := DB.Where("source_id = ? AND state = ?", sourceID, QXChatIdentityActive).First(&identity).Error; err != nil {
		return nil, ErrQXChatIdentityInactive
	}
	var user User
	if err := DB.Select("id", "status").First(&user, identity.UserID).Error; err != nil || user.Status != common.UserStatusEnabled {
		return nil, ErrQXChatIdentityInactive
	}
	return &identity, nil
}

func FindQXChatIdentity(identifier string) (*QXChatIdentity, error) {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	var rows []QXChatIdentity
	if err := DB.Where("source_username = ? OR source_email = ?", identifier, identifier).Limit(2).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, gorm.ErrRecordNotFound
	}
	return &rows[0], nil
}

func PrepareQXChatIdentity(profile QXChatSourceProfile, target *AuthSessionIdentity) (*QXChatIdentity, *User, error) {
	profile.Username = strings.ToLower(strings.TrimSpace(profile.Username))
	profile.Email = NormalizeEmail(profile.Email)
	if !qxchatSourceID.MatchString(profile.SourceID) || !qxchatSourceID.MatchString(profile.RequestID) ||
		!qxchatCredentialVersion.MatchString(profile.CredentialVersion) || profile.Authority != "qxchat" ||
		profile.NewAPIUserID != 0 || !profile.Eligible || !profile.EmailVerified || profile.Username == "" || len(profile.Username) > 32 ||
		len(profile.Email) > 191 || common.Validate.Var(profile.Email, "required,email") != nil {
		return nil, nil, ErrQXChatIdentityInactive
	}
	hash, err := common.ImportQXChatPasswordHash(profile.PasswordHash)
	if err != nil {
		return nil, nil, err
	}
	var identity QXChatIdentity
	var user User
	err = DB.Transaction(func(tx *gorm.DB) error {
		if target != nil {
			if strings.HasPrefix(target.SessionID, "pat:") {
				return ErrUserSessionInactive
			}
			if err := ValidateAuthSessionWithTx(tx, *target); err != nil {
				return err
			}
		}
		find := lockForUpdate(tx).Where("source_id = ?", profile.SourceID).First(&identity).Error
		if find == nil {
			if target != nil && identity.UserID != target.UserID || target == nil && !identity.Imported {
				return ErrQXChatIdentityConflict
			}
			if err := lockForUpdate(tx).First(&user, identity.UserID).Error; err != nil || (identity.Imported && user.Status != QXChatUserPendingStatus) || (!identity.Imported && user.Status != common.UserStatusEnabled) {
				return ErrQXChatIdentityInactive
			}
			if identity.State == QXChatIdentityActive {
				return ErrQXChatIdentityInactive
			}
			if identity.Imported {
				if !strings.HasPrefix(user.Password, common.QXChatPasswordPrefix) {
					return ErrQXChatIdentityInactive
				}
				if err := tx.Model(&user).Update("password", hash).Error; err != nil {
					return err
				}
				user.Password = hash
			}
			// Keep the original idempotency key when concurrent logins prepared the same source.
			identity.CredentialVersion = profile.CredentialVersion
			return tx.Model(&identity).Update("credential_version", profile.CredentialVersion).Error
		}
		if !errors.Is(find, gorm.ErrRecordNotFound) {
			return find
		}
		if target != nil {
			if err := tx.First(&user, target.UserID).Error; err != nil {
				return err
			}
		} else {
			username := profile.Username
			if utf8.RuneCountInString(username) > UserNameMaxLength {
				username = "qx_" + strings.ReplaceAll(profile.SourceID, "-", "")[:16]
			}
			if err := withNormalizedEmailLock(tx, profile.Email, func(tx *gorm.DB) error {
				var collisions int64
				if err := tx.Unscoped().Model(&User{}).Where("LOWER(username) IN ? OR LOWER(email) IN ?", []string{username, profile.Email}, []string{username, profile.Email}).Count(&collisions).Error; err != nil {
					return err
				}
				if collisions > 0 {
					return ErrQXChatIdentityConflict
				}
				name := []rune(profile.DisplayName)
				if len(name) > UserNameMaxLength { name = name[:UserNameMaxLength] }
				user = User{Username: username, Email: profile.Email, Password: hash, DisplayName: string(name), Role: common.RoleCommonUser,
					Status: QXChatUserPendingStatus, Group: "default", AffCode: common.GetRandomString(16), AuthVersion: 1}
				// Deliberately bypass Insert/FinishInsert: migration is not registration.
				return tx.Create(&user).Error
			}); err != nil {
				return err
			}
		}
		identity = QXChatIdentity{SourceID: profile.SourceID, UserID: user.Id, SourceUsername: profile.Username, SourceEmail: profile.Email,
			RequestID: profile.RequestID, CredentialVersion: profile.CredentialVersion, AuthorityVersion: 1, State: QXChatIdentityPending, Imported: target == nil}
		return tx.Create(&identity).Error
	})
	return &identity, &user, err
}

func ActivateQXChatIdentity(expected QXChatIdentity) (*User, error) {
	var user User
	err := DB.Transaction(func(tx *gorm.DB) error {
		var identity QXChatIdentity
		if err := lockForUpdate(tx).Where("source_id = ?", expected.SourceID).First(&identity).Error; err != nil {
			return err
		}
		if identity.UserID != expected.UserID || identity.RequestID != expected.RequestID || identity.CredentialVersion != expected.CredentialVersion {
			return ErrQXChatIdentityConflict
		}
		if err := lockForUpdate(tx).First(&user, identity.UserID).Error; err != nil {
			return ErrQXChatIdentityInactive
		}
		if identity.State == QXChatIdentityActive {
			if user.Status != common.UserStatusEnabled { return ErrQXChatIdentityInactive }
			return nil
		}
		if identity.Imported {
			if user.Status != QXChatUserPendingStatus || !strings.HasPrefix(user.Password, common.QXChatPasswordPrefix) || user.AuthVersion != 1 { return ErrQXChatIdentityInactive }
			if err := tx.Model(&user).Update("status", common.UserStatusEnabled).Error; err != nil { return err }
			user.Status = common.UserStatusEnabled
		} else if user.Status != common.UserStatusEnabled {
			return ErrQXChatIdentityInactive
		}
		return tx.Model(&identity).Update("state", QXChatIdentityActive).Error
	})
	return &user, err
}
