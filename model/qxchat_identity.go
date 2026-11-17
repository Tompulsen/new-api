package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const QXChatIdentityPending = "pending"
const QXChatIdentityActive = "active"
const QXChatIdentityProvisioning = "provisioning"
const QXChatUserPendingStatus = 3
const QXChatImportPasswordPrefix = "!qxchat-import!"
const qxchatSessionPurpose = "qxchat_session"
const qxchatVerifiedEmailProvider = "qxchat_verified_email"

var ErrQXChatIdentityConflict = errors.New("QXChat identity requires explicit account binding")
var ErrQXChatIdentitySnapshotConflict = fmt.Errorf("%w: frozen migration snapshot differs", ErrQXChatIdentityConflict)
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

type QXChatRegistrationProfile struct {
	RequestID         string `json:"request_id"`
	SourceID          string `json:"source_id"`
	NewAPIUserID      int    `json:"newapi_user_id"`
	AuthorityVersion  int64  `json:"authority_version"`
	CredentialVersion string `json:"credential_version"`
	Username          string `json:"username"`
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	IdentityOnly      bool   `json:"identity_only"`
}

// Only local email-code verification may write this claim; OAuth profile emails are not proof.
func RecordQXChatVerifiedEmailWithTx(tx *gorm.DB, userID int, email string) error {
	email = NormalizeEmail(email)
	if tx == nil || userID <= 0 || common.Validate.Var(email, "required,email") != nil {
		return ErrQXChatIdentityInactive
	}
	digest := sha256.Sum256([]byte(strconv.Itoa(userID) + "\n" + email))
	if err := ReleaseExternalIdentityWithTx(tx, qxchatVerifiedEmailProvider, userID); err != nil {
		return err
	}
	return ClaimExternalIdentityWithTx(tx, qxchatVerifiedEmailProvider, hex.EncodeToString(digest[:]), userID)
}

func validateQXChatVerifiedEmailWithTx(tx *gorm.DB, user User) error {
	email := NormalizeEmail(user.Email)
	if len(email) > 191 || common.Validate.Var(email, "required,email") != nil {
		return ErrQXChatIdentityInactive
	}
	digest := sha256.Sum256([]byte(strconv.Itoa(user.Id) + "\n" + email))
	var claim ExternalIdentityClaim
	if err := tx.Where("provider = ? AND user_id = ? AND subject = ?", qxchatVerifiedEmailProvider, user.Id, hex.EncodeToString(digest[:])).First(&claim).Error; err != nil {
		return ErrQXChatIdentityInactive
	}
	return nil
}

// Reserve the destination before contacting QX, so retries cannot create a second local identity.
func PrepareQXChatRegistration(token, origin string) (*QXChatIdentity, *QXChatRegistrationProfile, error) {
	if !communityOpaque.MatchString(token) || origin == "" {
		return nil, nil, ErrAuthFlowInvalid
	}
	var mapping QXChatIdentity
	var profile *QXChatRegistrationProfile
	err := DB.Transaction(func(tx *gorm.DB) error {
		var flow AuthFlow
		if err := applyAuthFlowMatch(tx, token, AuthFlowMatch{Purpose: communityIdentityPurpose}).
			Where("consumed_at IS NULL AND expires_at > ?", time.Now()).First(&flow).Error; err != nil {
			return ErrAuthFlowInvalid
		}
		if _, err := validateCommunityGrant(tx, &flow, origin); err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx).First(&user, flow.UserId).Error; err != nil || user.Status != common.UserStatusEnabled {
			return ErrQXChatIdentityInactive
		}
		err := tx.Where("user_id = ?", user.Id).First(&mapping).Error
		if err == nil && mapping.State == QXChatIdentityActive {
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && mapping.State != QXChatIdentityProvisioning {
			return ErrQXChatIdentityInactive
		}
		// Finish older reservations with their original verified-email contract.
		if err == nil && mapping.SourceEmail != "" {
			if mapping.SourceEmail != NormalizeEmail(user.Email) {
				return ErrQXChatIdentityInactive
			}
			if err := validateQXChatVerifiedEmailWithTx(tx, user); err != nil {
				return err
			}
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			username := strings.ToLower(strings.TrimSpace(user.Username))
			if username == "" || utf8.RuneCountInString(username) > 32 {
				return ErrQXChatIdentityInactive
			}
			mapping = QXChatIdentity{SourceID: uuid.NewString(), UserID: user.Id, SourceUsername: username,
				RequestID: uuid.NewString(), AuthorityVersion: 1, State: QXChatIdentityProvisioning}
			digest := sha256.Sum256([]byte(mapping.SourceID + "\n" + mapping.RequestID))
			mapping.CredentialVersion = hex.EncodeToString(digest[:])
			if err := tx.Create(&mapping).Error; err != nil {
				return err
			}
		}
		displayName := user.DisplayName
		if displayName == "" {
			displayName = user.Username
		}
		profile = &QXChatRegistrationProfile{RequestID: mapping.RequestID, SourceID: mapping.SourceID, NewAPIUserID: mapping.UserID,
			AuthorityVersion: mapping.AuthorityVersion, CredentialVersion: mapping.CredentialVersion, Username: mapping.SourceUsername,
			Email: mapping.SourceEmail, DisplayName: displayName, IdentityOnly: mapping.SourceEmail == ""}
		return nil
	})
	return &mapping, profile, err
}

func FinishQXChatRegistration(expected QXChatIdentity, token, origin string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var flow AuthFlow
		if err := applyAuthFlowMatch(tx, token, AuthFlowMatch{Purpose: communityIdentityPurpose, UserId: expected.UserID}).
			Where("consumed_at IS NULL AND expires_at > ?", time.Now()).First(&flow).Error; err != nil {
			return ErrAuthFlowInvalid
		}
		if _, err := validateCommunityGrant(tx, &flow, origin); err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx).First(&user, expected.UserID).Error; err != nil || user.Status != common.UserStatusEnabled {
			return ErrQXChatIdentityInactive
		}
		if expected.SourceEmail != "" {
			if NormalizeEmail(user.Email) != expected.SourceEmail {
				return ErrQXChatIdentityInactive
			}
			if err := validateQXChatVerifiedEmailWithTx(tx, user); err != nil {
				return err
			}
		}
		var mapping QXChatIdentity
		if err := lockForUpdate(tx).Where("user_id = ?", expected.UserID).First(&mapping).Error; err != nil {
			return err
		}
		if mapping.SourceID != expected.SourceID || mapping.RequestID != expected.RequestID ||
			mapping.CredentialVersion != expected.CredentialVersion || mapping.SourceUsername != expected.SourceUsername ||
			mapping.SourceEmail != expected.SourceEmail || mapping.AuthorityVersion != expected.AuthorityVersion || mapping.Imported ||
			(mapping.State != QXChatIdentityProvisioning && mapping.State != QXChatIdentityActive) {
			return ErrQXChatIdentityConflict
		}
		return tx.Model(&mapping).Update("state", QXChatIdentityActive).Error
	})
}

// A signed, definitive collision means QX made no write; free the reservation for explicit binding.
func ReleaseQXChatRegistration(expected QXChatIdentity) error {
	return DB.Where("source_id = ? AND user_id = ? AND request_id = ? AND state = ?", expected.SourceID, expected.UserID, expected.RequestID, QXChatIdentityProvisioning).
		Delete(&QXChatIdentity{}).Error
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
	// Native SSO display names must not shadow an unrelated legacy account login.
	if err := DB.Where("source_email <> '' AND (source_username = ? OR source_email = ?)", identifier, identifier).Limit(2).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, gorm.ErrRecordNotFound
	}
	return &rows[0], nil
}

func PrepareQXChatIdentity(profile QXChatSourceProfile, target *AuthSessionIdentity) (*QXChatIdentity, *User, error) {
	return prepareQXChatIdentity(profile, target, false, true)
}

// Offline imports never refresh an existing reservation or turn a dry run into a write.
func PrepareQXChatImport(profile QXChatSourceProfile, apply bool) (*QXChatIdentity, *User, error) {
	return prepareQXChatIdentity(profile, nil, true, apply)
}

func qxchatImportUsername(sourceID string, attempt int) string {
	if attempt == 0 {
		return "qx_" + strings.ReplaceAll(sourceID, "-", "")[:16]
	}
	digest := sha256.Sum256([]byte(sourceID + "\n" + strconv.Itoa(attempt)))
	return "qx_" + hex.EncodeToString(digest[:])[:16]
}

func prepareQXChatIdentity(profile QXChatSourceProfile, target *AuthSessionIdentity, offline, apply bool) (*QXChatIdentity, *User, error) {
	profile.Username = strings.ToLower(strings.TrimSpace(profile.Username))
	profile.Email = NormalizeEmail(profile.Email)
	if !qxchatSourceID.MatchString(profile.SourceID) || !qxchatSourceID.MatchString(profile.RequestID) ||
		!qxchatCredentialVersion.MatchString(profile.CredentialVersion) || profile.Authority != "qxchat" ||
		profile.NewAPIUserID != 0 || !profile.Eligible || !profile.EmailVerified || profile.Username == "" || utf8.RuneCountInString(profile.Username) > 32 ||
		len(profile.Email) > 191 || common.Validate.Var(profile.Email, "required,email") != nil {
		return nil, nil, ErrQXChatIdentityInactive
	}
	hash, err := common.ImportQXChatPasswordHash(profile.PasswordHash)
	if err != nil {
		if offline {
			return nil, nil, ErrQXChatIdentityInactive
		}
		return nil, nil, err
	}
	if offline {
		hash = QXChatImportPasswordPrefix + hash
	}
	var identity QXChatIdentity
	var user User
	err = DB.Transaction(func(tx *gorm.DB) error {
		find := lockForUpdate(tx).Where("source_id = ?", profile.SourceID).First(&identity).Error
		if target != nil {
			if strings.HasPrefix(target.SessionID, "pat:") {
				return ErrUserSessionInactive
			}
			if err := ValidateAuthSessionWithTx(tx, *target); err != nil {
				return err
			}
		}
		if find == nil {
			if target != nil && identity.UserID != target.UserID || target == nil && !identity.Imported {
				return ErrQXChatIdentityConflict
			}
			if err := lockForUpdate(tx).First(&user, identity.UserID).Error; err != nil {
				if offline && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				return ErrQXChatIdentityInactive
			}
			if offline && identity.RequestID != profile.RequestID {
				return ErrQXChatIdentitySnapshotConflict
			}
			if identity.State == QXChatIdentityActive {
				if offline && identity.CredentialVersion == profile.CredentialVersion &&
					identity.SourceUsername == profile.Username && identity.SourceEmail == profile.Email && user.Status == common.UserStatusEnabled {
					return nil
				}
				return ErrQXChatIdentityInactive
			}
			if identity.State != QXChatIdentityPending ||
				(identity.Imported && user.Status != QXChatUserPendingStatus) ||
				(!identity.Imported && user.Status != common.UserStatusEnabled) {
				return ErrQXChatIdentityInactive
			}
			if offline {
				if identity.CredentialVersion != profile.CredentialVersion || identity.SourceUsername != profile.Username ||
					identity.SourceEmail != profile.Email || user.AuthVersion != 1 || user.Password != hash {
					return ErrQXChatIdentitySnapshotConflict
				}
				return nil
			}
			if identity.Imported {
				// Offline reservations deliberately fail this guard, including on older deployed servers.
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
				username = qxchatImportUsername(profile.SourceID, 0)
			}
			if err := withNormalizedEmailLock(tx, profile.Email, func(tx *gorm.DB) error {
				email := profile.Email
				identifiers := []string{username, profile.Email}
				if offline {
					identifiers = append(identifiers, profile.Username)
				}
				var collisions int64
				if err := tx.Unscoped().Model(&User{}).Where("LOWER(username) IN ? OR LOWER(email) IN ?", identifiers, identifiers).Count(&collisions).Error; err != nil {
					return err
				}
				if collisions > 0 && !offline {
					return ErrQXChatIdentityConflict
				}
				if offline {
					if collisions > 0 {
						username = qxchatImportUsername(profile.SourceID, 0)
					}
					available := false
					for attempt := range 100 {
						if err := tx.Unscoped().Model(&User{}).Where("LOWER(username) = ? OR LOWER(email) = ?", username, username).Count(&collisions).Error; err != nil {
							return err
						}
						if collisions == 0 {
							available = true
							break
						}
						username = qxchatImportUsername(profile.SourceID, attempt+1)
					}
					if !available {
						return ErrQXChatIdentityConflict
					}
					if err := tx.Unscoped().Model(&User{}).Where("LOWER(username) = ? OR LOWER(email) = ?", email, email).Count(&collisions).Error; err != nil {
						return err
					}
					if collisions > 0 {
						email = ""
					}
				}
				name := []rune(profile.DisplayName)
				if len(name) > UserNameMaxLength {
					name = name[:UserNameMaxLength]
				}
				user = User{Username: username, Email: email, Password: hash, DisplayName: string(name), Role: common.RoleCommonUser,
					Status: QXChatUserPendingStatus, Group: "default", AffCode: common.GetRandomString(16), AuthVersion: 1}
				if !apply {
					return nil
				}
				// Deliberately bypass Insert/FinishInsert: migration is not registration.
				return tx.Create(&user).Error
			}); err != nil {
				return err
			}
		}
		identity = QXChatIdentity{SourceID: profile.SourceID, UserID: user.Id, SourceUsername: profile.Username, SourceEmail: profile.Email,
			RequestID: profile.RequestID, CredentialVersion: profile.CredentialVersion, AuthorityVersion: 1, State: QXChatIdentityPending, Imported: target == nil}
		if !apply {
			return nil
		}
		return tx.Create(&identity).Error
	})
	return &identity, &user, err
}

func ActivateQXChatIdentity(expected QXChatIdentity, target *AuthSessionIdentity) (*User, error) {
	return activateQXChatIdentity(expected, target, false, "")
}

func QXChatImportPasswordDigest(password string) string {
	digest := sha256.Sum256([]byte(password))
	return hex.EncodeToString(digest[:])
}

func ActivateQXChatImportIdentity(expected QXChatIdentity, passwordDigest string) (*User, error) {
	return activateQXChatIdentity(expected, nil, true, passwordDigest)
}

func activateQXChatIdentity(expected QXChatIdentity, target *AuthSessionIdentity, offline bool, passwordDigest string) (*User, error) {
	var user User
	err := DB.Transaction(func(tx *gorm.DB) error {
		var identity QXChatIdentity
		if err := lockForUpdate(tx).Where("source_id = ?", expected.SourceID).First(&identity).Error; err != nil {
			return err
		}
		if identity.UserID != expected.UserID || identity.RequestID != expected.RequestID || identity.CredentialVersion != expected.CredentialVersion {
			return ErrQXChatIdentityConflict
		}
		if offline && (!identity.Imported || identity.AuthorityVersion != expected.AuthorityVersion ||
			(identity.State != QXChatIdentityPending && identity.State != QXChatIdentityActive)) {
			return ErrQXChatIdentitySnapshotConflict
		}
		if !identity.Imported {
			if target == nil || target.UserID != identity.UserID || strings.HasPrefix(target.SessionID, "pat:") {
				return ErrUserSessionInactive
			}
			// A source commit cannot undo a target logout, password change or session revocation.
			if err := ValidateAuthSessionWithTx(tx, *target); err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).First(&user, identity.UserID).Error; err != nil {
			if offline && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			return ErrQXChatIdentityInactive
		}
		if identity.State == QXChatIdentityActive {
			if user.Status != common.UserStatusEnabled {
				return ErrQXChatIdentityInactive
			}
			return nil
		}
		if identity.Imported {
			prefix := common.QXChatPasswordPrefix
			if offline {
				prefix = QXChatImportPasswordPrefix + common.QXChatPasswordPrefix
			}
			if user.Status != QXChatUserPendingStatus || !strings.HasPrefix(user.Password, prefix) || user.AuthVersion != 1 ||
				(offline && identity.State != QXChatIdentityPending) {
				return ErrQXChatIdentityInactive
			}
			updates := map[string]any{"status": common.UserStatusEnabled}
			if offline {
				if !qxchatCredentialVersion.MatchString(passwordDigest) || QXChatImportPasswordDigest(user.Password) != passwordDigest {
					return ErrQXChatIdentitySnapshotConflict
				}
				hash, err := common.ImportQXChatPasswordHash(strings.TrimPrefix(user.Password, prefix))
				if err != nil || QXChatImportPasswordPrefix+hash != user.Password {
					return ErrQXChatIdentityInactive
				}
				updates["password"] = hash
				user.Password = hash
			}
			if err := tx.Model(&user).Updates(updates).Error; err != nil {
				return err
			}
			user.Status = common.UserStatusEnabled
		} else if user.Status != common.UserStatusEnabled {
			return ErrQXChatIdentityInactive
		}
		return tx.Model(&identity).Update("state", QXChatIdentityActive).Error
	})
	return &user, err
}

type QXChatSessionIdentity struct {
	Active           bool   `json:"active"`
	UserID           int    `json:"newapi_user_id"`
	SourceID         string `json:"source_id"`
	AuthorityVersion int64  `json:"authority_version"`
	Username         string `json:"username"`
	SessionProof     string `json:"session_proof,omitempty"`
	ExpiresAt        int64  `json:"expires_at"`
}

// The one-use PKCE identity grant creates a QX-only server-side session reference.
func ExchangeQXChatSession(token, origin string) (*QXChatSessionIdentity, error) {
	if !communityOpaque.MatchString(token) || origin == "" {
		return nil, ErrAuthFlowInvalid
	}
	var result QXChatSessionIdentity
	_, err := ConsumeAuthFlowWithAction(token, AuthFlowMatch{Purpose: communityIdentityPurpose}, func(tx *gorm.DB, flow *AuthFlow) error {
		grant, err := validateCommunityGrant(tx, flow, origin)
		if err != nil {
			return err
		}
		var mapping QXChatIdentity
		if err := tx.Where("user_id = ? AND state = ?", grant.UserID, QXChatIdentityActive).First(&mapping).Error; err != nil {
			return ErrQXChatIdentityInactive
		}
		var session UserSession
		if err := tx.Where("sid = ? AND user_id = ?", grant.SessionID, grant.UserID).First(&session).Error; err != nil {
			return err
		}
		expires := time.Unix(min(session.ExpiresAt, time.Now().Add(24*time.Hour).Unix()), 0)
		proof, _, err := createAuthFlowWithTx(tx, AuthFlowCreate{Purpose: qxchatSessionPurpose, UserId: grant.UserID, SessionId: grant.SessionID, Payload: flow.Payload, ExpiresAt: expires})
		if err != nil {
			return err
		}
		result = QXChatSessionIdentity{Active: true, UserID: grant.UserID, SourceID: mapping.SourceID, AuthorityVersion: mapping.AuthorityVersion, Username: mapping.SourceUsername, SessionProof: proof, ExpiresAt: expires.Unix()}
		return nil
	})
	return &result, err
}

func ValidateQXChatSession(proof, origin, sourceID string, userID int) (*QXChatSessionIdentity, error) {
	if !communityOpaque.MatchString(proof) || origin == "" || !qxchatSourceID.MatchString(sourceID) || userID <= 0 {
		return nil, ErrAuthFlowInvalid
	}
	var result QXChatSessionIdentity
	err := DB.Transaction(func(tx *gorm.DB) error {
		var flow AuthFlow
		if err := applyAuthFlowMatch(tx, proof, AuthFlowMatch{Purpose: qxchatSessionPurpose, UserId: userID}).
			Where("consumed_at IS NULL AND expires_at > ?", time.Now()).First(&flow).Error; err != nil {
			return ErrAuthFlowInvalid
		}
		if _, err := validateCommunityGrant(tx, &flow, origin); err != nil {
			return err
		}
		var mapping QXChatIdentity
		if err := tx.Where("source_id = ? AND user_id = ? AND state = ?", sourceID, userID, QXChatIdentityActive).First(&mapping).Error; err != nil {
			return ErrQXChatIdentityInactive
		}
		result = QXChatSessionIdentity{Active: true, UserID: userID, SourceID: mapping.SourceID, AuthorityVersion: mapping.AuthorityVersion, Username: mapping.SourceUsername, ExpiresAt: flow.ExpiresAt.Unix()}
		return nil
	})
	return &result, err
}
