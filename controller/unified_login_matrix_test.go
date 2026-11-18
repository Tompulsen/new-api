package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestUnifiedLoginUnicodeMatrix(t *testing.T) {
	owner, _ := setupSecurityEnrollmentTest(t)
	owner.Username = "ÜSER"
	require.NoError(t, owner.Update(false))
	// MySQL's default collation considers these near-matches equal; they must
	// not count toward the identity limit after Go's exact normalization.
	for index := range 5 {
		user := model.User{Username: fmt.Sprintf("accent-other-%d", index), Password: owner.Password, AffCode: fmt.Sprintf("accent-aff-%d", index)}
		require.NoError(t, model.DB.Create(&user).Error)
		require.NoError(t, model.DB.Create(&model.QXChatIdentity{SourceID: fmt.Sprintf("aaaaaaaa-bbbb-4ccc-8ddd-%012d", index),
			UserID: user.Id, SourceUsername: "user", SourceEmail: fmt.Sprintf("accent-%d@example.com", index), State: model.QXChatIdentityActive}).Error)
	}
	require.NoError(t, model.InitializeLoginIdentifiers())
	for _, identifier := range []string{"ÜSER", "üser", "ÜseR"} {
		user, selection, err := model.AuthenticatePasswordIdentifier(identifier, "enrollment-password", "")
		require.NoError(t, err, identifier)
		require.Nil(t, selection)
		assert.Equal(t, owner.Id, user.Id)
	}
	for index := range 4 {
		user := model.User{Username: fmt.Sprintf("unicode-other-%d", index), Password: owner.Password, AuthVersion: 1, AffCode: fmt.Sprintf("unicode-aff-%d", index)}
		require.NoError(t, model.DB.Create(&user).Error)
		require.NoError(t, model.DB.Create(&model.QXChatIdentity{SourceID: fmt.Sprintf("ffffffff-bbbb-4ccc-8ddd-%012d", index),
			UserID: user.Id, SourceUsername: "Üser", SourceEmail: fmt.Sprintf("unicode-%d@example.com", index), State: model.QXChatIdentityActive}).Error)
		require.NoError(t, model.InitializeLoginIdentifiers())
		for _, identifier := range []string{"ÜSER", "üser", "ÜseR"} {
			user, selection, err := model.AuthenticatePasswordIdentifier(identifier, "enrollment-password", "")
			if index == 3 {
				assert.ErrorIs(t, err, model.ErrLoginIdentifierTooMany)
				assert.Nil(t, selection)
				continue
			}
			require.NoError(t, err)
			assert.Nil(t, user)
			require.NotNil(t, selection)
			require.Len(t, selection.Candidates, index+2, "Unicode aliases must not disappear from confirmation")
			selected, err := model.ConfirmPasswordLoginIdentity(selection.FlowToken, selection.Candidates[0].SelectionID, "")
			require.NoError(t, err)
			assert.Equal(t, owner.Id, selected.Id)
		}
	}
}

func TestUnifiedLoginMigrationMatrix(t *testing.T) {
	for _, scenario := range []string{"fresh", "upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			dialect := os.Getenv("TEST_SECURITY_DIALECT")
			if dialect == "" {
				dialect = "sqlite"
			}
			db, _ := newAuditTestDatabase(t, dialect, os.Getenv("TEST_"+strings.ToUpper(dialect)+"_DSN"))
			previousDB := model.DB
			model.DB = db
			t.Cleanup(func() { model.DB = previousDB })
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.QXChatIdentity{}))
			var expectedUsers []model.User
			var expectedMappings []model.QXChatIdentity
			if scenario == "upgrade" {
				users := []model.User{
					{Username: "renamed-owner", Email: "source@example.com", Password: "preserve-hash", Quota: 21871, AuthVersion: 5, AffCode: "migration-first"},
					{Username: "old-source", Email: "other@example.com", Password: "other-hash", Quota: 37441, AuthVersion: 8, AffCode: "migration-other"},
				}
				require.NoError(t, db.Create(&users).Error)
				require.NoError(t, db.Create(&model.QXChatIdentity{SourceID: qxchatTestSource, UserID: users[0].Id,
					SourceUsername: "old-source", SourceEmail: "source@example.com", State: model.QXChatIdentityActive}).Error)
				require.NoError(t, db.Create(&model.QXChatIdentity{SourceID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", UserID: users[1].Id,
					SourceUsername: "pending-name", SourceEmail: "pending@example.com", State: model.QXChatIdentityPending}).Error)
			}
			require.NoError(t, db.Unscoped().Order("id").Find(&expectedUsers).Error)
			require.NoError(t, db.Order("source_id").Find(&expectedMappings).Error)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.LoginIdentifierReservation{}))
				require.NoError(t, model.InitializeLoginIdentifiers())
				var actualUsers []model.User
				var actualMappings []model.QXChatIdentity
				require.NoError(t, db.Unscoped().Order("id").Find(&actualUsers).Error)
				require.NoError(t, db.Order("source_id").Find(&actualMappings).Error)
				assert.Equal(t, expectedUsers, actualUsers)
				assert.Equal(t, expectedMappings, actualMappings)
				assert.True(t, db.Migrator().HasIndex(&model.User{}, "Username"))
				assert.True(t, db.Migrator().HasIndex(&model.User{}, "Email"))
				assert.True(t, db.Migrator().HasIndex(&model.QXChatIdentity{}, "UserID"))
				var count int64
				require.NoError(t, db.Model(&model.LoginIdentifierReservation{}).Count(&count).Error)
				if scenario == "fresh" {
					assert.Zero(t, count)
				} else {
					assert.EqualValues(t, 6, count)
					var collision model.LoginIdentifierReservation
					require.NoError(t, db.First(&collision, "identifier = ?", "old-source").Error)
					assert.Zero(t, collision.OwnerID)
					var pending model.LoginIdentifierReservation
					require.NoError(t, db.First(&pending, "identifier = ?", "pending-name").Error)
					assert.Equal(t, expectedUsers[1].Id, pending.OwnerID)
				}
			}
		})
	}
}

func TestUnifiedLoginConcurrentClaimMatrix(t *testing.T) {
	for _, scenario := range []string{"registration", "username-email", "legacy-alias"} {
		t.Run(scenario, func(t *testing.T) {
			owner, _ := setupSecurityEnrollmentTest(t)
			require.NoError(t, model.InitializeLoginIdentifiers())
			if scenario == "legacy-alias" {
				require.NoError(t, model.DB.Create(&model.QXChatIdentity{SourceID: qxchatTestSource, UserID: owner.Id,
					SourceUsername: "reserved-name", SourceEmail: "source@example.com", State: model.QXChatIdentityActive}).Error)
				require.NoError(t, model.InitializeLoginIdentifiers())
			}
			connection, err := model.DB.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(8)
			first, err := connection.Conn(context.Background())
			require.NoError(t, err)
			second, err := connection.Conn(context.Background())
			require.NoError(t, err)
			require.GreaterOrEqual(t, connection.Stats().OpenConnections, 2)
			require.NoError(t, first.Close())
			require.NoError(t, second.Close())
			if connection.Driver() != nil && model.DB.Dialector.Name() == "sqlite" {
				var mode string
				require.NoError(t, model.DB.Raw("PRAGMA journal_mode").Scan(&mode).Error)
				assert.Equal(t, "wal", mode)
			}
			users := []model.User{
				{Username: "reserved-name", Password: "Registration-password-123"},
				{Username: "RESERVED-NAME", Password: "Registration-password-456"},
			}
			if scenario == "username-email" {
				users[0].Username = "shared@example.com"
				users[1].Username, users[1].Email = "new-user", "shared@example.com"
			}
			start, results := make(chan struct{}), make(chan error, 2)
			for i := range users {
				go func(index int) {
					<-start
					results <- model.DB.Transaction(func(tx *gorm.DB) error { return users[index].InsertWithTx(tx, 0) })
				}(i)
			}
			close(start)
			successes := 0
			for range users {
				if err := <-results; err == nil {
					successes++
				}
			}
			if scenario == "legacy-alias" {
				assert.Zero(t, successes)
			} else {
				assert.Equal(t, 1, successes)
			}
			var count int64
			require.NoError(t, model.DB.Model(&model.User{}).Count(&count).Error)
			assert.EqualValues(t, 1+successes, count, "failed claims roll back user creation")
			assert.Equal(t, common.UserStatusEnabled, owner.Status)
		})
	}
}
