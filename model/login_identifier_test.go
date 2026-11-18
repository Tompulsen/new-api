package model

import (
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func unifiedLoginOwner(t *testing.T) User {
	t.Helper()
	setupUserUpdateTestState(t)
	hash, err := common.Password2Hash("NewAPI-password-123")
	require.NoError(t, err)
	user := User{Username: "12334", Email: "owner@example.com", Password: hash, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "unified-owner"}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Create(&QXChatIdentity{SourceID: "11111111-2222-4333-8444-555555555555", UserID: user.Id,
		SourceUsername: "tomplusen", SourceEmail: user.Email, State: QXChatIdentityActive, Imported: false}).Error)
	require.NoError(t, InitializeLoginIdentifiers())
	return user
}

func TestUnifiedLoginOldNameUsesCurrentNewAPIPassword(t *testing.T) {
	owner := unifiedLoginOwner(t)
	login := User{Username: "tomplusen", Password: "NewAPI-password-123"}
	require.NoError(t, login.ValidateAndFill())
	assert.Equal(t, owner.Id, login.Id)
	wrong := User{Username: "tomplusen", Password: "former-QX-password"}
	assert.ErrorIs(t, wrong.ValidateAndFill(), ErrInvalidCredentials)
}

func TestUnifiedLoginUnicodeIdentifiersKeepCurrentCredentials(t *testing.T) {
	owner := unifiedLoginOwner(t)
	owner.Username = "ÜSER"
	require.NoError(t, owner.Update(false))
	for _, identifier := range []string{"ÜSER", "üser"} {
		user, _, err := AuthenticatePasswordIdentifier(identifier, "NewAPI-password-123", "")
		require.NoError(t, err, identifier)
		assert.Equal(t, owner.Id, user.Id)
	}
}

func TestUnifiedLoginNameCannotBeClaimedByAnotherUser(t *testing.T) {
	unifiedLoginOwner(t)
	newUser := User{Username: "tomplusen", Password: "Another-user-password-123", Status: common.UserStatusEnabled}
	assert.Error(t, newUser.Insert(0), "a migrated login name must remain owned by the original identity")
}

func unifiedLoginCollision(t *testing.T, samePassword bool) (User, User) {
	t.Helper()
	owner := unifiedLoginOwner(t)
	password := owner.Password
	if !samePassword {
		var err error
		password, err = common.Password2Hash("Different-account-password-456")
		require.NoError(t, err)
	}
	other := User{Username: "tomplusen", Password: password, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "unified-other"}
	require.NoError(t, DB.Create(&other).Error)
	require.NoError(t, InitializeLoginIdentifiers())
	return owner, other
}

func TestUnifiedLoginCollisionRequiresConfirmationEvenWhenOnlyOnePasswordMatches(t *testing.T) {
	for _, samePassword := range []bool{false, true} {
		t.Run(map[bool]string{false: "one-password", true: "same-password"}[samePassword], func(t *testing.T) {
			owner, _ := unifiedLoginCollision(t, samePassword)
			user, selection, err := AuthenticatePasswordIdentifier(" ToMplUsEn ", "NewAPI-password-123", "https://accounts.example")
			require.NoError(t, err)
			require.Nil(t, user)
			require.NotNil(t, selection)
			want := 1
			if samePassword {
				want = 2
			}
			require.Len(t, selection.Candidates, want)
			assert.Len(t, selection.FlowToken, 43)
			assert.True(t, selection.RequiresIdentitySelection)
			assert.InDelta(t, time.Now().Add(5*time.Minute).Unix(), selection.ExpiresAt, 2)
			var candidate LoginIdentityCandidate
			for _, value := range selection.Candidates {
				if value.Username == owner.Username {
					candidate = value
				}
			}
			require.Len(t, candidate.SelectionID, 22)
			flow, err := GetAuthFlow(selection.FlowToken, AuthFlowMatch{Purpose: AuthFlowPurposeLoginIdentity})
			require.NoError(t, err)
			assert.NotContains(t, flow.Payload, "NewAPI-password-123")
			assert.NotContains(t, flow.Payload, owner.Password)
			_, err = ConfirmPasswordLoginIdentity(selection.FlowToken, candidate.SelectionID, "https://attacker.example")
			assert.ErrorIs(t, err, ErrInvalidCredentials)
			selected, err := ConfirmPasswordLoginIdentity(selection.FlowToken, candidate.SelectionID, "https://accounts.example")
			require.NoError(t, err)
			assert.Equal(t, owner.Id, selected.Id)
			_, err = ConfirmPasswordLoginIdentity(selection.FlowToken, candidate.SelectionID, "https://accounts.example")
			assert.ErrorIs(t, err, ErrInvalidCredentials)
			legacy := User{Username: "tomplusen", Password: "NewAPI-password-123"}
			assert.ErrorIs(t, legacy.ValidateAndFill(), ErrLoginIdentifierAmbiguous)
		})
	}
}

func TestUnifiedLoginSelectionRejectsChangedOrExpiredProof(t *testing.T) {
	for _, change := range []string{"password", "auth_version", "status", "deleted", "mapping_pending", "expired"} {
		t.Run(change, func(t *testing.T) {
			owner, _ := unifiedLoginCollision(t, false)
			_, selection, err := AuthenticatePasswordIdentifier("tomplusen", "NewAPI-password-123", "")
			require.NoError(t, err)
			require.Len(t, selection.Candidates, 1)
			switch change {
			case "password":
				require.NoError(t, DB.Model(&owner).Update("password", "different-hash-without-version-change").Error)
			case "auth_version":
				require.NoError(t, DB.Model(&owner).Update("auth_version", 2).Error)
			case "status":
				require.NoError(t, DB.Model(&owner).Update("status", common.UserStatusDisabled).Error)
			case "deleted":
				require.NoError(t, DB.Delete(&owner).Error)
			case "mapping_pending":
				require.NoError(t, DB.Model(&QXChatIdentity{}).Where("user_id = ?", owner.Id).Update("state", QXChatIdentityPending).Error)
			case "expired":
				require.NoError(t, DB.Model(&AuthFlow{}).Where("purpose = ?", AuthFlowPurposeLoginIdentity).Update("expires_at", time.Now().Add(-time.Minute)).Error)
			}
			_, err = ConfirmPasswordLoginIdentity(selection.FlowToken, selection.Candidates[0].SelectionID, "")
			assert.Error(t, err)
		})
	}
}

func TestUnifiedLoginPendingAndDisplayNamesCannotAuthenticate(t *testing.T) {
	for _, state := range []string{QXChatIdentityPending, QXChatIdentityProvisioning, "display_name_only"} {
		t.Run(state, func(t *testing.T) {
			owner := unifiedLoginOwner(t)
			changes := map[string]any{"state": state}
			if state == "display_name_only" {
				changes = map[string]any{"source_email": ""}
			}
			require.NoError(t, DB.Model(&QXChatIdentity{}).Where("user_id = ?", owner.Id).Updates(changes).Error)
			_, _, err := AuthenticatePasswordIdentifier("tomplusen", "NewAPI-password-123", "")
			assert.ErrorIs(t, err, ErrInvalidCredentials)
			user, selection, err := AuthenticatePasswordIdentifier(owner.Username, "NewAPI-password-123", "")
			require.NoError(t, err, "a native credential remains usable while its QX provisioning is pending")
			assert.Nil(t, selection)
			assert.Equal(t, owner.Id, user.Id)
		})
	}
}

func TestUnifiedLoginReservationsSurviveRenameAndDoNotBlockUnrelatedUpdates(t *testing.T) {
	owner, other := unifiedLoginCollision(t, true)
	owner.DisplayName = "Updated profile"
	require.NoError(t, owner.Update(false), "legacy ambiguous names must not block unrelated profile updates")
	other.Username = "new-native-name"
	require.NoError(t, other.Update(false))
	var reservation LoginIdentifierReservation
	require.NoError(t, DB.First(&reservation, "identifier_hash = ?", loginIdentifierHash("tomplusen")).Error)
	assert.Zero(t, reservation.OwnerID)
	newUser := User{Username: "tomplusen", Password: "Fresh-user-password-123", Status: common.UserStatusEnabled}
	assert.ErrorIs(t, newUser.Insert(0), ErrLoginIdentifierTaken)
	login, choice, err := AuthenticatePasswordIdentifier("tomplusen", "NewAPI-password-123", "")
	require.NoError(t, err)
	assert.Nil(t, choice)
	assert.Equal(t, owner.Id, login.Id)
}

func TestUnifiedLoginIdentifiersOnlyListUniqueActiveNames(t *testing.T) {
	owner := unifiedLoginOwner(t)
	values, err := GetUserLoginIdentifiers(owner.Id)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"12334", "owner@example.com", "tomplusen"}, values.LoginIdentifiers)
	other := User{Username: "tomplusen", Email: owner.Email, AffCode: "other-identifiers"}
	require.NoError(t, DB.Create(&other).Error)
	values, err = GetUserLoginIdentifiers(owner.Id)
	require.NoError(t, err)
	assert.Equal(t, []string{"12334"}, values.LoginIdentifiers)
	assert.Equal(t, owner.Username, values.Username)
	assert.Equal(t, owner.Email, values.Email)
}

func TestUnifiedLoginOldEmailReservationIsNotLoginOrRecovery(t *testing.T) {
	owner := unifiedLoginOwner(t)
	owner.Email = "new@example.com"
	require.NoError(t, owner.Update(false))
	// The old address remains an explicit QX alias but does not regain reset rights.
	_, err := GetUniqueUserByEmail("owner@example.com")
	assert.ErrorIs(t, err, ErrEmailNotFound)
	values, err := GetUserLoginIdentifiers(owner.Id)
	require.NoError(t, err)
	assert.Contains(t, values.LoginIdentifiers, "owner@example.com")
	require.NoError(t, DB.Model(&QXChatIdentity{}).Where("user_id = ?", owner.Id).Update("source_email", "source@example.com").Error)
	_, _, err = AuthenticatePasswordIdentifier("owner@example.com", "NewAPI-password-123", "")
	assert.ErrorIs(t, err, ErrInvalidCredentials)
	available, err := IsEmailAvailable("owner@example.com", 0)
	require.NoError(t, err)
	assert.False(t, available)
}

func TestUnifiedLoginChangedNamesCannotClaimReservedAliases(t *testing.T) {
	owner := unifiedLoginOwner(t)
	other := User{Username: "other-user", Email: "other@example.com", Password: owner.Password, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "rename-other"}
	require.NoError(t, DB.Create(&other).Error)
	require.NoError(t, InitializeLoginIdentifiers())
	for _, update := range []string{"self", "admin", "email", "username-email"} {
		t.Run(update, func(t *testing.T) {
			copy := other
			var err error
			switch update {
			case "self":
				copy.Username = " TOMPLUSEN "
				err = copy.Update(false)
			case "admin":
				copy.Username = "tomplusen"
				err = copy.Edit(false)
			case "email":
				err = BindEmailToUser(&copy, "owner@example.com")
			case "username-email":
				copy.Username = "owner@example.com"
				err = copy.Update(false)
			}
			assert.Error(t, err)
			var stored User
			require.NoError(t, DB.First(&stored, other.Id).Error)
			assert.Equal(t, other.Username, stored.Username)
			assert.Equal(t, other.Email, stored.Email)
		})
	}
}

func TestUnifiedLoginCandidateLimitDoesNotSilentlyTruncate(t *testing.T) {
	owner := unifiedLoginOwner(t)
	for i := 0; i < MaxPasswordLoginCandidates; i++ {
		name := "extra" + strings.Repeat("x", i+1)
		user := User{Username: name, Email: "tomplusen", Password: owner.Password, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: name}
		require.NoError(t, DB.Create(&user).Error)
	}
	user, choice, err := AuthenticatePasswordIdentifier("tomplusen", "NewAPI-password-123", "")
	assert.ErrorIs(t, err, ErrLoginIdentifierTooMany)
	assert.Nil(t, user)
	assert.Nil(t, choice)
	var count int64
	require.NoError(t, DB.Model(&AuthFlow{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestUnifiedLoginClaimRollbackAndIdempotentBackfill(t *testing.T) {
	owner := unifiedLoginOwner(t)
	require.NoError(t, InitializeLoginIdentifiers())
	var before int64
	require.NoError(t, DB.Model(&LoginIdentifierReservation{}).Count(&before).Error)
	require.NoError(t, InitializeLoginIdentifiers())
	var after int64
	require.NoError(t, DB.Model(&LoginIdentifierReservation{}).Count(&after).Error)
	assert.Equal(t, before, after)
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := ClaimLoginIdentifiersWithTx(tx, owner.Id, "temporary-name"); err != nil {
			return err
		}
		return assert.AnError
	})
	assert.ErrorIs(t, err, assert.AnError)
	require.NoError(t, DB.Model(&LoginIdentifierReservation{}).Where("identifier_hash = ?", loginIdentifierHash("temporary-name")).Count(&after).Error)
	assert.Zero(t, after)
}
