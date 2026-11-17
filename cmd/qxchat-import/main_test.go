package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestImportMaintenanceDatabaseAndPreview(t *testing.T) {
	oldDB, oldType, oldDebug := model.DB, common.MainDatabaseType(), common.DebugEnabled
	t.Cleanup(func() { model.DB = oldDB; common.SetMainDatabaseType(oldType); common.DebugEnabled = oldDebug })
	dsn := os.Getenv("TEST_QXCHAT_IMPORT_DSN")
	database := os.Getenv("TEST_QXCHAT_IMPORT_DATABASE")
	server := os.Getenv("TEST_QXCHAT_IMPORT_SERVER")
	var driver gorm.Dialector
	if dsn == "" {
		path := filepath.Join(t.TempDir(), "import.db")
		t.Setenv("SQLITE_PATH", path)
		dsn, database, server = "local", "main", path
		driver = sqlite.Open(path)
	} else {
		require.True(t, strings.HasPrefix(database, "qxchat_import_test_"), "external import tests require a dedicated empty test database")
		require.NotEmpty(t, server)
		if strings.HasPrefix(dsn, "postgres") {
			driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
		} else {
			driver = mysql.Open(dsn)
		}
	}
	seed, err := gorm.Open(driver, &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	require.False(t, seed.Migrator().HasTable(&model.User{}), "test database must be empty")
	require.NoError(t, seed.AutoMigrate(&model.User{}, &model.QXChatIdentity{}, &model.Option{}))
	t.Cleanup(func() {
		if model.DB != nil && model.DB != oldDB {
			connection, _ := model.DB.DB()
			if connection != nil {
				_ = connection.Close()
			}
		}
		if dsn != "local" {
			require.NoError(t, seed.Migrator().DropTable(&model.QXChatIdentity{}, &model.User{}, &model.Option{}))
		}
		connection, _ := seed.DB()
		if connection != nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, seed.Create(&model.Option{Key: "import-test-sentinel", Value: "preserved"}).Error)
	model.DB = seed
	t.Setenv("SQL_DSN", "")
	assert.Error(t, model.OpenQXChatImportDB(database, server))
	t.Setenv("SQL_DSN", dsn)
	assert.Error(t, model.OpenQXChatImportDB(database+"-wrong", server))
	assert.Error(t, model.OpenQXChatImportDB(database, server+"-wrong"))
	assert.Same(t, seed, model.DB)
	require.NoError(t, model.OpenQXChatImportDB(database, server))
	var options int64
	require.NoError(t, model.DB.Model(&model.Option{}).Count(&options).Error)
	assert.EqualValues(t, 1, options)
	profile := &model.QXChatSourceProfile{SourceID: "11111111-2222-4333-8444-555555555555", RequestID: "66666666-7777-4888-9999-aaaaaaaaaaaa",
		Username: "import-user", Email: "import@example.com", PasswordHash: strings.Repeat("a", 32) + ":" + strings.Repeat("b", 128),
		CredentialVersion: strings.Repeat("c", 64), Eligible: true, EmailVerified: true, Authority: "qxchat"}
	input := importRow{Status: "eligible", Profile: profile}
	preview, err := processImportRow(context.Background(), "inspect", false, input)
	require.NoError(t, err)
	assert.Equal(t, "eligible", preview.Status)
	assert.Zero(t, preview.Mapping.NewAPIUserID)
	var users int64
	require.NoError(t, model.DB.Model(&model.User{}).Count(&users).Error)
	assert.Zero(t, users)
	rejected, err := processImportRow(context.Background(), "prepare", true, input)
	require.NoError(t, err)
	assert.Equal(t, "source_reservation_required", rejected.Code)
	input.Status = "prepared"
	prepared, err := processImportRow(context.Background(), "prepare", true, input)
	require.NoError(t, err)
	require.Equal(t, "prepared", prepared.Status)
	assert.NotEmpty(t, prepared.Mapping.PasswordDigest)
	encoded, err := common.Marshal(prepared)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), profile.PasswordHash)
	assert.Equal(t, profile.Username, prepared.Mapping.TargetUsername)
	assert.Equal(t, profile.Email, prepared.Mapping.TargetEmail)
	repeated, err := processImportRow(context.Background(), "prepare", true, input)
	require.NoError(t, err)
	assert.Equal(t, prepared.Mapping.NewAPIUserID, repeated.Mapping.NewAPIUserID)
	inspected, err := processImportRow(context.Background(), "inspect", false, prepared)
	require.NoError(t, err)
	assert.Equal(t, "prepared", inspected.Status)
	require.NoError(t, model.DB.Model(&model.User{}).Count(&users).Error)
	assert.EqualValues(t, 1, users)
	require.NoError(t, model.DB.Model(&model.Option{}).Count(&options).Error)
	assert.EqualValues(t, 1, options)
}

func TestImportJSONLRejectsUnsafeAndDuplicateInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("server CLI requires Unix private file permissions")
	}
	path := filepath.Join(t.TempDir(), "source.jsonl")
	line := `{"status":"rejected","source_id":"11111111-2222-4333-8444-555555555555","request_id":"66666666-7777-4888-9999-aaaaaaaaaaaa"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(line), 0600))
	rows, err := readImportRows(path)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	link := filepath.Join(t.TempDir(), "link.jsonl")
	require.NoError(t, os.Symlink(path, link))
	_, err = readImportRows(link)
	assert.Error(t, err)
	require.NoError(t, os.Chmod(path, 0644))
	_, err = readImportRows(path)
	assert.Error(t, err)
	require.NoError(t, os.Chmod(path, 0600))
	require.NoError(t, os.WriteFile(path, []byte(line+line), 0600))
	_, err = readImportRows(path)
	assert.Error(t, err)
	require.NoError(t, os.WriteFile(path, []byte("invalid\n"), 0600))
	_, err = readImportRows(path)
	assert.Error(t, err)
}
