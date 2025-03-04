/*---------------------------------------------------------------------------------------------
 *  Copyright (c) IBAX. All rights reserved.
 *  See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package sqldb

import (
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/IBAX-io/go-ibax/packages/conf"
	"github.com/IBAX-io/go-ibax/packages/consts"
	"github.com/IBAX-io/go-ibax/packages/converter"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	// DBConn is orm connection
	DBConn *gorm.DB

	// ErrRecordNotFound is Not Found Record wrapper
	ErrRecordNotFound = gorm.ErrRecordNotFound

	// ErrDBConn database connection error
	ErrDBConn = errors.New("Database connection error")
)
var notAutoIncrement = map[string]bool{
	"1_keys": true,
}

// non-self-increasing costs
const notAutoIncrementCost int64 = 1

type KeyTableChecker struct{}

func (ktc KeyTableChecker) IsKeyTable(tableName string) bool {
	val, exist := converter.FirstEcosystemTables[tableName]
	return exist && val
}

type NextIDGetter struct {
	Tx *DbTransaction
}

func (g NextIDGetter) GetNextID(tableName string) (int64, error) {
	return GetNextID(g.Tx, tableName)
}
func isFound(db *gorm.DB) (bool, error) {
	if errors.Is(db.Error, ErrRecordNotFound) {
		return false, nil
	}
	return true, db.Error
}

// GormInit is initializes Gorm connection
func GormInit(conf conf.DBConfig) error {
	var err error
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable password=%s TimeZone=UTC", conf.Host, conf.Port, conf.User, conf.Name, conf.Password)
open:
	DBConn, err = gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true, // disables implicit prepared statement usage
	}), &gorm.Config{
		AllowGlobalUpdate: true, //allow global update
		//PrepareStmt:       true,
		Logger: logger.Default.LogMode(logger.Silent), // start Logger，show detail log
	})
	//DBConn, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		if strings.Contains(err.Error(), "SQLSTATE 3D000") {
			err := createDatabase(fmt.Sprintf("host=%s port=%d user=%s password=%s sslmode=disable TimeZone=UTC", conf.Host, conf.Port, conf.User, conf.Password), conf.Name)
			if err != nil {
				return err
			}
			goto open
		}
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("cant open connection to DB")
		DBConn = nil
		return err
	}
	sqlDB, err := DBConn.DB()
	if err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("cant get sql DB")
		DBConn = nil
		return err
	}

	sqlDB.SetConnMaxLifetime(time.Minute * 10)
	sqlDB.SetMaxIdleConns(conf.MaxIdleConns)
	sqlDB.SetMaxOpenConns(conf.MaxOpenConns)

	if err = setupConnOptions(DBConn); err != nil {
		return err
	}
	return nil
}

func createDatabase(dsn string, dbName string) error {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Error),
	})
	if err != nil {
		return err
	}
	result := db.Exec("create database " + dbName)
	defer func() {
		d, _ := db.DB()
		d.Close()
	}()
	return result.Error
}

func setupConnOptions(conr *gorm.DB) error {
	if err := conr.Exec(fmt.Sprintf(`set lock_timeout = %d;`, conf.Config.DB.LockTimeout)).Error; err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("can't set lock timeout")
		return err
	}

	if err := conr.Exec(fmt.Sprintf(`set idle_in_transaction_session_timeout = %d;`, conf.Config.DB.IdleInTxTimeout)).Error; err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("can't set idle_in_transaction_session_timeout")
		return err
	}
	return conr.Exec("SET TIME ZONE 'UTC'").Error
}

// GormClose is closing Gorm connection
func GormClose() error {
	if DBConn != nil {
		sqlDB, err := DBConn.DB()
		if err != nil {
			return err
		}
		if err = sqlDB.Close(); err != nil {
			return err
		}
		DBConn = nil
	}
	return nil
}

// DbTransaction is gorm.DB wrapper
type DbTransaction struct {
	conn *gorm.DB
}

func NewDbTransaction(conn *gorm.DB) *DbTransaction {
	return &DbTransaction{conn: conn}
}

// StartTransaction is beginning transaction
func StartTransaction() (*DbTransaction, error) {
	conn := DBConn.Begin()
	if conn.Error != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": conn.Error}).Error("cannot start transaction because of connection error")
		return nil, conn.Error
	}

	if err := setupConnOptions(conn); err != nil {
		return nil, err
	}

	return &DbTransaction{
		conn: conn,
	}, nil
}

// Rollback is transaction rollback
func (tr *DbTransaction) Rollback() error {
	return tr.conn.Rollback().Error
}

// Commit is transaction commit
func (tr *DbTransaction) Commit() error {
	return tr.conn.Commit().Error
}

// Connection returns connection of database
func (tr *DbTransaction) Connection() *gorm.DB {
	return tr.conn
}

// Savepoint creates PostgreSQL Savepoint
func (tr *DbTransaction) Savepoint(mark string) error {
	return tr.Connection().SavePoint(mark).Error
}

// RollbackSavepoint rollbacks PostgreSQL Savepoint
func (tr *DbTransaction) RollbackSavepoint(mark string) error {
	return tr.Connection().RollbackTo(mark).Error
}

func (tr *DbTransaction) ResetSavepoint(mark string) error {
	if err := tr.RollbackSavepoint(mark); err != nil {
		return err
	}
	return tr.Savepoint(mark)
}

// GetDB is returning gorm.DB
func GetDB(tr *DbTransaction) *gorm.DB {
	if tr != nil && tr.conn != nil {
		return tr.conn
	}
	return DBConn
}

// DropTables is dropping all of the tables
func DropTables() error {
	return DBConn.Exec(`
	DO $$ DECLARE
	    r RECORD;
	BEGIN
	    FOR r IN (SELECT tablename FROM pg_tables WHERE schemaname = current_schema()) LOOP
		EXECUTE 'DROP TABLE IF EXISTS ' || quote_ident(r.tablename) || ' CASCADE';
	    END LOOP;
	END $$;
	`).Error
}

// GetRecordsCountTx is counting all records of table in transaction
func GetRecordsCountTx(db *DbTransaction, tableName, where string) (count int64, err error) {
	dbQuery := GetDB(db).Table(tableName)
	if len(where) > 0 {
		dbQuery = dbQuery.Where(where)
	}
	if !notAutoIncrement[tableName] {
		err := dbQuery.Select("id").Order("id DESC").Limit(1).Scan(&count).Error
		if err != nil {
			return 0, err
		}
	} else {
		//err = dbQuery.Count(&count).Error
		count = notAutoIncrementCost
	}
	return count, err
}

// Update is updating table rows
func Update(transaction *DbTransaction, tblname, set, where string) error {
	return GetDB(transaction).Exec(`UPDATE "` + strings.Trim(tblname, `"`) + `" SET ` + set + " " + where).Error
}

// Delete is deleting table rows
func Delete(transaction *DbTransaction, tblname, where string) error {
	return GetDB(transaction).Exec(`DELETE FROM "` + tblname + `" ` + where).Error
}

// GetColumnCount is counting rows in table
func GetColumnCount(tableName string) (int64, error) {
	var count int64
	err := DBConn.Raw("SELECT count(*) FROM information_schema.columns WHERE table_name=?", tableName).Row().Scan(&count)
	if err == gorm.ErrRecordNotFound {
		return 0, nil
	}
	if err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("executing raw query")
		return 0, err
	}
	return count, nil
}

// AlterTableAddColumn is adding column to table
func AlterTableAddColumn(transaction *DbTransaction, tableName, columnName, columnType string) error {
	return GetDB(transaction).Exec(`ALTER TABLE "` + tableName + `" ADD COLUMN "` + columnName + `" ` + columnType).Error
}

// AlterTableDropColumn is dropping column from table
func AlterTableDropColumn(transaction *DbTransaction, tableName, columnName string) error {
	return GetDB(transaction).Exec(`ALTER TABLE "` + tableName + `" DROP COLUMN "` + columnName + `"`).Error
}

// CreateIndex is creating index on table column
func CreateIndex(transaction *DbTransaction, indexName, tableName, onColumn string) error {
	return GetDB(transaction).Exec(`CREATE INDEX "` + indexName + `_index" ON "` + tableName + `" (` + onColumn + `)`).Error
}

// GetColumnDataTypeCharMaxLength is returns max length of table column
func GetColumnDataTypeCharMaxLength(tableName, columnName string) (map[string]string, error) {
	return GetOneRow(`select data_type,character_maximum_length from
			 information_schema.columns where table_name = ? AND column_name = ?`,
		tableName, columnName).String()
}

// GetAllColumnTypes returns column types for table
func GetAllColumnTypes(tblname string) ([]map[string]string, error) {
	return GetAll(`SELECT column_name, data_type
		FROM information_schema.columns
		WHERE table_name = ?
		ORDER BY ordinal_position ASC`, -1, tblname)
}

func DataTypeToColumnType(dataType string) string {
	var itype string
	switch {
	case dataType == "character varying":
		itype = `varchar`
	case dataType == `bigint`:
		itype = "number"
	case dataType == `jsonb`:
		itype = "json"
	case strings.HasPrefix(dataType, `timestamp`):
		itype = "datetime"
	case strings.HasPrefix(dataType, `numeric`):
		itype = "money"
	case strings.HasPrefix(dataType, `double`):
		itype = "double"
	case strings.HasPrefix(dataType, `bytea`):
		itype = "bytea"
	default:
		itype = dataType
	}
	return itype
}

// GetColumnType is returns type of column
func GetColumnType(tblname, column string) (itype string, err error) {
	coltype, err := GetColumnDataTypeCharMaxLength(tblname, column)
	if err != nil {
		return
	}
	if dataType, ok := coltype["data_type"]; ok {
		itype = DataTypeToColumnType(dataType)
	}
	return
}

// DropTable is dropping table
func DropTable(transaction *DbTransaction, tableName string) error {
	return GetDB(transaction).Migrator().DropTable(tableName)
}

// NumIndexes is counting table indexes
func NumIndexes(tblname string) (int, error) {
	var indexes int64
	err := DBConn.Raw(fmt.Sprintf(`select count( i.relname) from pg_class t, pg_class i, pg_index ix, pg_attribute a
	 where t.oid = ix.indrelid and i.oid = ix.indexrelid and a.attrelid = t.oid and a.attnum = ANY(ix.indkey)
         and t.relkind = 'r'  and t.relname = '%s'`, tblname)).Row().Scan(&indexes)
	if err == gorm.ErrRecordNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(indexes - 1), nil
}

// IsIndex returns is table column is an index
func IsIndex(tblname, column string) (bool, error) {
	row, err := GetOneRow(`select t.relname as table_name, i.relname as index_name, a.attname as column_name
	 from pg_class t, pg_class i, pg_index ix, pg_attribute a 
	 where t.oid = ix.indrelid and i.oid = ix.indexrelid and a.attrelid = t.oid and a.attnum = ANY(ix.indkey)
		 and t.relkind = 'r'  and t.relname = ?  and a.attname = ?`, tblname, column).String()
	return len(row) > 0 && row[`column_name`] == column, err
}

// ListResult is a structure for the list result
type ListResult struct {
	result []string
	err    error
}

// String return the slice of strings
func (r *ListResult) String() ([]string, error) {
	if r.err != nil {
		return r.result, r.err
	}
	return r.result, nil
}

// GetList returns the result of the query as ListResult variable
func GetList(query string, args ...interface{}) *ListResult {
	var result []string
	all, err := GetAll(query, -1, args...)
	if err != nil {
		return &ListResult{result, err}
	}
	for _, v := range all {
		for _, v2 := range v {
			result = append(result, v2)
		}
	}
	return &ListResult{result, nil}
}

// GetNextID returns next ID of table
func GetNextID(transaction *DbTransaction, table string) (int64, error) {
	var id int64
	rows, err := GetDB(transaction).Raw(`select id from "` + table + `" order by id desc limit 1`).Rows()
	if err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err, "table": table}).Error("selecting next id from table")
		return 0, err
	}
	rows.Next()
	rows.Scan(&id)
	rows.Close()
	return id + 1, err
}

// IsTable returns is table exists
func IsTable(tblname string) bool {
	var name string
	err := DBConn.Table("information_schema.tables").
		Where("table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema') AND table_name=?", tblname).
		Select("table_name").Row().Scan(&name)
	if err != nil {
		return false
	}

	return name == tblname
}

func HasTableOrView(tr *DbTransaction, names string) bool {
	var name string
	DBConn.Table("information_schema.tables").
		Where("table_type IN ('BASE TABLE', 'VIEW') AND table_schema NOT IN ('pg_catalog', 'information_schema') AND table_name=?", names).
		Select("table_name").Row().Scan(&name)

	return name == names
}

type Namer struct {
	TableType string
}

type SchemaInter interface {
	HasExists(tr *DbTransaction, name string) bool
}

func (v Namer) HasExists(tr *DbTransaction, names string) bool {
	var typs string
	switch v.TableType {
	case "table":
		typs = `= 'BASE TABLE'`
	case "view":
		typs = `= 'VIEW'`
	default:
		typs = `IN ('BASE TABLE', 'VIEW')`
	}
	var name string
	GetDB(tr).Table("information_schema.tables").
		Where(fmt.Sprintf("table_type %s AND table_schema NOT IN ('pg_catalog', 'information_schema') AND table_name='%s'", typs, names)).
		Select("table_name").Row().Scan(&name)
	return name == names
}

// GetColumnByID returns the value of the column from the table by id
func GetColumnByID(table, column, id string) (result string, err error) {
	err = DBConn.Table(table).Select(column).Where(`id=?`, id).Row().Scan(&result)
	if err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting column by id")
	}
	return
}

// InitDB drop all tables and exec db schema
func InitDB(cfg conf.DBConfig) error {

	err := GormInit(cfg)
	if err != nil || DBConn == nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("initializing DB")
		return ErrDBConn
	}
	if err = DropTables(); err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("dropping all tables")
		return err
	}

	if conf.Config.Redis.Enable {
		err = RedisInit(conf.Config.Redis)
		if err != nil {
			log.WithFields(log.Fields{
				"host": conf.Config.Redis.Host, "port": conf.Config.Redis.Port, "db_password": conf.Config.Redis.Password, "db_name": conf.Config.Redis.DbName, "type": consts.DBError,
			}).Error("can't init redis")
			return err
		}

		var rd RedisParams
		err := rd.Cleardb()
		if err != nil {
			return err
		}

	}

	if err = ExecSchema(); err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("executing db schema")
		return err
	}

	install := &Install{Progress: ProgressComplete}
	if err = install.Create(); err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("creating install")
		return err
	}

	if err := ExecCLBSchema(consts.DefaultCLB, conf.Config.KeyID); err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("creating CLB schema")
		return err
	}
	if err := ExecSubSchema(); err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("creating CLB schema")
		return err
	}

	return nil
}

// DropDatabase kill all process and drop database
func DropDatabase(name string) error {
	query := `SELECT
	pg_terminate_backend (pg_stat_activity.pid)
   FROM
	pg_stat_activity
   WHERE
	pg_stat_activity.datname = ?`

	if err := DBConn.Exec(query, name).Error; err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err, "dbname": name}).Error("on kill db process")
		return err
	}

	if err := DBConn.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", name)).Error; err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err, "dbname": name}).Error("on drop db")
		return err
	}

	return nil
}

// GetColumnByID returns the value of the column from the table by id
func GetSumColumn(table, column, where string) (result string, err error) {
	err = DBConn.Table(table).Select("sum(" + column + ")").Where(where).Row().Scan(&result)
	if err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("sum column")
	}
	return
}

//GetColumnByID returns the value of the column from the table by id
func GetSumColumnCount(table, column, where string) (result int, err error) {
	err = DBConn.Table(table).Select("count(*)").Where(where).Row().Scan(&result)
	if err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("sum column")
	}
	return
}
