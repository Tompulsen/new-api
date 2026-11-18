package model

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// OpenQXChatImportDB opens existing tables without migrations, options, root-account creation, or background jobs.
func OpenQXChatImportDB(expectedDatabase, expectedServer string) error {
	dsn := strings.TrimSpace(os.Getenv("SQL_DSN"))
	if dsn == "" || expectedDatabase == "" || expectedServer == "" {
		return errors.New("explicit SQL_DSN, expected database and expected server are required")
	}
	var dialector gorm.Dialector
	dbType := common.DatabaseTypeMySQL
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		parsed, err := url.Parse(dsn)
		if err != nil || strings.TrimPrefix(parsed.Path, "/") != expectedDatabase {
			return errors.New("configured database does not match expected database")
		}
		dbType = common.DatabaseTypePostgreSQL
		dialector = postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{DSN: dsn, PreferSimpleProtocol: true}}}
	case dsn == "local":
		path := os.Getenv("SQLITE_PATH")
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "?#") {
			return errors.New("SQLite import requires an absolute existing SQLITE_PATH without query parameters")
		}
		path, err := filepath.EvalSymlinks(path)
		if err != nil {
			return errors.New("SQLite import file does not exist")
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || expectedDatabase != "main" || path != expectedServer {
			return errors.New("SQLite import target does not match expected file")
		}
		dbType = common.DatabaseTypeSQLite
		dialector = sqlite.Open(path + "?mode=rw&_pragma=busy_timeout(30000)&_txlock=immediate")
	default:
		if isClickHouseDSN(dsn) || strings.HasPrefix(dsn, "local") {
			return errors.New("unsupported import database")
		}
		if !strings.Contains(dsn, "parseTime=") {
			if strings.Contains(dsn, "?") {
				dsn += "&parseTime=true"
			} else {
				dsn += "?parseTime=true"
			}
		}
		dialector = mysqlMigrationDialector{mysql.Dialector{Config: &mysql.Config{DSN: dsn}}}
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return errors.New("import database connection failed")
	}
	connection, err := db.DB()
	if err != nil {
		return errors.New("import database connection unavailable")
	}
	connection.SetMaxOpenConns(2)
	connection.SetMaxIdleConns(2)
	connection.SetConnMaxLifetime(time.Minute)
	accepted := false
	defer func() {
		if !accepted {
			_ = connection.Close()
		}
	}()
	var actual struct {
		Database string
		Server   string
	}
	switch dbType {
	case common.DatabaseTypeMySQL:
		err = db.Raw("SELECT DATABASE() AS `database`, @@server_uuid AS server").Scan(&actual).Error
	case common.DatabaseTypePostgreSQL:
		err = db.Raw(`SELECT current_database() AS "database", COALESCE(inet_server_addr()::text, 'local') || ':' || COALESCE(inet_server_port()::text, '0') AS server`).Scan(&actual).Error
	case common.DatabaseTypeSQLite:
		actual.Database, actual.Server = "main", expectedServer
	}
	if err != nil || actual.Database != expectedDatabase || actual.Server != expectedServer {
		return errors.New("connected database or server does not match expected target")
	}
	for _, entity := range []any{&User{}, &QXChatIdentity{}} {
		statement := &gorm.Statement{DB: db}
		if err := statement.Parse(entity); err != nil {
			return errors.New("import schema validation failed")
		}
		if err := db.Model(entity).Select(statement.Schema.DBNames).Where("1 = 0").Find(entity).Error; err != nil {
			return errors.New("required existing import schema is unavailable")
		}
	}
	common.DebugEnabled = false
	common.SetMainDatabaseType(dbType)
	initCol()
	DB = db
	accepted = true
	return nil
}
