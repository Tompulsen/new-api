package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MaxPasswordLoginCandidates = 4

var ErrLoginIdentifierTaken = errors.New("This login name is already reserved by another account.")
var ErrLoginIdentifierAmbiguous = errors.New("This login name requires account confirmation.")
var ErrLoginIdentifierTooMany = errors.New("Please use your current unique account username to sign in.")

// Reservations survive renames and deletion so a former login cannot be claimed
// by somebody else. OwnerID zero preserves an existing cross-account collision.
// Only current users/mappings decide which reservations can actually sign in.
type LoginIdentifierReservation struct {
	IdentifierHash string `gorm:"type:char(64);primaryKey"`
	Identifier     string `gorm:"type:varchar(255);not null"`
	OwnerID        int    `gorm:"not null;index"`
}

func NormalizeLoginIdentifier(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func loginIdentifierHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func normalizedLoginIdentifiers(values ...string) []string {
	unique := make(map[string]bool)
	for _, value := range values {
		if value = NormalizeLoginIdentifier(value); value != "" {
			unique[value] = true
		}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func loginIdentifiersAvailable(tx *gorm.DB, excludeUserID int, values ...string) (bool, error) {
	for _, value := range normalizedLoginIdentifiers(values...) {
		var owners []LoginIdentifierReservation
		if err := tx.Where("identifier_hash = ?", loginIdentifierHash(value)).Find(&owners).Error; err != nil {
			return false, err
		}
		if len(owners) > 0 && (excludeUserID == 0 || owners[0].OwnerID != excludeUserID) {
			return false, nil
		}
	}
	return true, nil
}

// ClaimLoginIdentifiersWithTx must share the transaction that writes the user
// or mapping. A database unique key serializes all username/email/alias writers.
func ClaimLoginIdentifiersWithTx(tx *gorm.DB, userID int, values ...string) error {
	return claimLoginIdentifiersWithTx(tx, userID, false, values...)
}

func claimLoginIdentifiersWithTx(tx *gorm.DB, userID int, legacyImport bool, values ...string) error {
	if tx == nil || userID <= 0 {
		return ErrLoginIdentifierTaken
	}
	for _, value := range normalizedLoginIdentifiers(values...) {
		if len(value) > 255 {
			return ErrLoginIdentifierTaken
		}
		claim := LoginIdentifierReservation{IdentifierHash: loginIdentifierHash(value), Identifier: value, OwnerID: userID}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim).Error; err != nil {
			return err
		}
		var owner LoginIdentifierReservation
		if err := lockForUpdate(tx).Where("identifier_hash = ?", claim.IdentifierHash).First(&owner).Error; err != nil {
			return err
		}
		if owner.Identifier != value {
			return ErrLoginIdentifierTaken
		}
		if owner.OwnerID == userID {
			continue
		}
		if !legacyImport {
			return ErrLoginIdentifierTaken
		}
		if owner.OwnerID != 0 {
			if err := tx.Model(&owner).Update("owner_id", 0).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// InitializeLoginIdentifiers backfills without selecting an arbitrary owner for
// legacy collisions. Re-running only adds reservations or marks a collision.
func InitializeLoginIdentifiers() error {
	var users []User
	if err := DB.Unscoped().Select("id", "username", "email").Find(&users).Error; err != nil {
		return err
	}
	var mappings []QXChatIdentity
	if err := DB.Where("source_email <> ?", "").Find(&mappings).Error; err != nil {
		return err
	}
	owners := make(map[string]int)
	add := func(id int, values ...string) {
		for _, value := range normalizedLoginIdentifiers(values...) {
			if previous, exists := owners[value]; exists && previous != id {
				owners[value] = 0
			} else if !exists {
				owners[value] = id
			}
		}
	}
	for _, user := range users {
		add(user.Id, user.Username, user.Email)
	}
	for _, mapping := range mappings {
		add(mapping.UserID, mapping.SourceUsername, mapping.SourceEmail)
	}
	rows := make([]LoginIdentifierReservation, 0, len(owners))
	for value, owner := range owners {
		rows = append(rows, LoginIdentifierReservation{IdentifierHash: loginIdentifierHash(value), Identifier: value, OwnerID: owner})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].IdentifierHash < rows[j].IdentifierHash })
	return DB.Transaction(func(tx *gorm.DB) error {
		if len(rows) == 0 {
			return nil
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200).Error; err != nil {
			return err
		}
		var stored []LoginIdentifierReservation
		if err := tx.Find(&stored).Error; err != nil {
			return err
		}
		var conflicts []string
		for _, row := range stored {
			if owner, exists := owners[row.Identifier]; exists && owner != row.OwnerID && row.OwnerID != 0 {
				conflicts = append(conflicts, row.IdentifierHash)
			}
		}
		for len(conflicts) > 0 {
			count := min(200, len(conflicts))
			if err := tx.Model(&LoginIdentifierReservation{}).Where("identifier_hash IN ?", conflicts[:count]).Update("owner_id", 0).Error; err != nil {
				return err
			}
			conflicts = conflicts[count:]
		}
		return nil
	})
}

// passwordLoginCandidates returns identities before password verification. The
// caller must not infer identity from which one happens to accept the password.
func passwordLoginCandidates(tx *gorm.DB, identifier string) ([]User, error) {
	raw := strings.TrimSpace(identifier)
	identifier = NormalizeLoginIdentifier(identifier)
	if identifier == "" || len(identifier) > 255 {
		return nil, ErrInvalidCredentials
	}
	var reservations []LoginIdentifierReservation
	if err := tx.Where("identifier_hash = ?", loginIdentifierHash(identifier)).Find(&reservations).Error; err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDatabase, err)
	}
	nativeQuery := "LOWER(TRIM(username)) = ? OR LOWER(TRIM(email)) = ? OR username = ? OR email = ?"
	nativeArgs := []any{identifier, identifier, raw, raw}
	mappingQuery := "LOWER(TRIM(source_username)) = ? OR LOWER(TRIM(source_email)) = ? OR source_username = ? OR source_email = ?"
	mappingArgs := []any{identifier, identifier, raw, raw}
	if len(reservations) > 0 {
		reservation := reservations[0]
		if reservation.Identifier != identifier {
			return nil, ErrInvalidCredentials
		}
		if reservation.OwnerID > 0 {
			// The hash finds Unicode names independently of database collation.
			// The current names below still decide whether this owner may sign in.
			nativeQuery += " OR id = ?"
			nativeArgs = append(nativeArgs, reservation.OwnerID)
			mappingQuery += " OR user_id = ?"
			mappingArgs = append(mappingArgs, reservation.OwnerID)
		} else {
			// SQLite LOWER is ASCII-only. Legacy collisions must also consider
			// non-ASCII spellings before Go normalizes and deduplicates identities.
			nativeQuery += " OR " + nonASCIILoginNames(tx, "username", "email")
			mappingQuery += " OR " + nonASCIILoginNames(tx, "source_username", "source_email")
		}
	}
	var native []User
	if err := tx.Unscoped().Select("id", "username", "email").Where(nativeQuery, nativeArgs...).Find(&native).Error; err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDatabase, err)
	}
	var mappings []QXChatIdentity
	if err := tx.Select("user_id", "source_username", "source_email").Where("state = ? AND source_email <> ?", QXChatIdentityActive, "").
		Where("("+mappingQuery+")", mappingArgs...).Find(&mappings).Error; err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDatabase, err)
	}
	ids := make(map[int]bool)
	for _, user := range native {
		if identifier == NormalizeLoginIdentifier(user.Username) || identifier == NormalizeLoginIdentifier(user.Email) {
			ids[user.Id] = true
		}
	}
	for _, mapping := range mappings {
		if identifier == NormalizeLoginIdentifier(mapping.SourceUsername) || identifier == NormalizeLoginIdentifier(mapping.SourceEmail) {
			ids[mapping.UserID] = true
		}
	}
	if len(ids) > MaxPasswordLoginCandidates {
		return nil, ErrLoginIdentifierTooMany
	}
	if len(ids) == 0 {
		return nil, ErrInvalidCredentials
	}
	keys := make([]int, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	var users []User
	if err := tx.Unscoped().Where("id IN ?", keys).Order("id").Find(&users).Error; err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDatabase, err)
	}
	return users, nil
}

func nonASCIILoginNames(tx *gorm.DB, columns ...string) string {
	conditions := make([]string, 0, len(columns))
	for _, column := range columns {
		byteLength, charLength := "LENGTH(CAST("+column+" AS BLOB))", "LENGTH("+column+")"
		switch tx.Dialector.Name() {
		case "mysql":
			byteLength, charLength = "LENGTH("+column+")", "CHAR_LENGTH("+column+")"
		case "postgres":
			byteLength, charLength = "OCTET_LENGTH("+column+")", "CHAR_LENGTH("+column+")"
		}
		conditions = append(conditions, byteLength+" <> "+charLength)
	}
	return strings.Join(conditions, " OR ")
}

func passwordLoginAllowed(user User, password string) bool {
	return !user.DeletedAt.Valid && user.Status == common.UserStatusEnabled && user.Password != "" &&
		common.ValidatePasswordAndHash(password, user.Password)
}

type UserLoginIdentifiers struct {
	Username         string   `json:"username"`
	Email            string   `json:"email"`
	LoginIdentifiers []string `json:"login_identifiers"`
}

func GetUserLoginIdentifiers(userID int) (*UserLoginIdentifiers, error) {
	var user User
	if err := DB.First(&user, userID).Error; err != nil {
		return nil, err
	}
	values := []string{user.Username, user.Email}
	var mappings []QXChatIdentity
	if err := DB.Where("user_id = ? AND state = ? AND source_email <> ?", userID, QXChatIdentityActive, "").Find(&mappings).Error; err != nil {
		return nil, err
	}
	for _, mapping := range mappings {
		values = append(values, mapping.SourceUsername, mapping.SourceEmail)
	}
	result := &UserLoginIdentifiers{Username: user.Username, Email: user.Email, LoginIdentifiers: []string{}}
	for _, value := range normalizedLoginIdentifiers(values...) {
		candidates, err := passwordLoginCandidates(DB, value)
		if errors.Is(err, ErrLoginIdentifierTooMany) || errors.Is(err, ErrInvalidCredentials) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(candidates) == 1 && candidates[0].Id == userID && user.Status == common.UserStatusEnabled {
			result.LoginIdentifiers = append(result.LoginIdentifiers, value)
		}
	}
	return result, nil
}
