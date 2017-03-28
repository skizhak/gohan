// Copyright (C) 2015 NTT Innovation Institute, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwan/gohan/db/pagination"
	"github.com/cloudwan/gohan/db/transaction"
	"github.com/cloudwan/gohan/util"

	"database/sql"

	"github.com/cloudwan/gohan/schema"
	"github.com/jmoiron/sqlx"
	sq "github.com/lann/squirrel"
	// DB import
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"
	_ "github.com/nati/go-fakedb"
)

const retryDB = 50
const retryDBWait = 10

const (
	configVersionColumnName   = "config_version"
	stateVersionColumnName    = "state_version"
	stateErrorColumnName      = "state_error"
	stateColumnName           = "state"
	stateMonitoringColumnName = "state_monitoring"
)

//DB is sql implementation of DB
type DB struct {
	sqlType, connectionString string
	handlers                  map[string]propertyHandler
	DB                        *sqlx.DB
}

//Transaction is sql implementation of Transaction
type Transaction struct {
	transaction *sqlx.Tx
	db          *DB
	closed      bool
}

//NewDB constructor
func NewDB() *DB {
	handlers := make(map[string]propertyHandler)
	//TODO(nati) dynamic configuration
	handlers["string"] = &stringHandler{}
	handlers["number"] = &numberHandler{}
	handlers["integer"] = &integerHandler{}
	handlers["object"] = &jsonHandler{}
	handlers["array"] = &jsonHandler{}
	handlers["boolean"] = &boolHandler{}
	return &DB{handlers: handlers}
}

//propertyHandler for each propertys
type propertyHandler interface {
	encode(*schema.Property, interface{}) (interface{}, error)
	decode(*schema.Property, interface{}) (interface{}, error)
	dataType(*schema.Property) string
}

type defaultHandler struct {
}

func (handler *defaultHandler) encode(property *schema.Property, data interface{}) (interface{}, error) {
	return data, nil
}

func (handler *defaultHandler) decode(property *schema.Property, data interface{}) (interface{}, error) {
	return data, nil
}

func (handler *defaultHandler) dataType(property *schema.Property) (res string) {
	// TODO(marcin) extend types for schema. Here is pretty ugly guessing
	if property.ID == "id" || property.Relation != "" || property.Unique {
		res = "varchar(255)"
	} else {
		res = "text"
	}
	return
}

type stringHandler struct {
	defaultHandler
}

func (handler *stringHandler) encode(property *schema.Property, data interface{}) (interface{}, error) {
	return data, nil
}

func (handler *stringHandler) decode(property *schema.Property, data interface{}) (interface{}, error) {
	if bytes, ok := data.([]byte); ok {
		return string(bytes), nil
	}
	return data, nil
}

type boolHandler struct{}

func (handler *boolHandler) encode(property *schema.Property, data interface{}) (interface{}, error) {
	return data, nil
}

func (handler *boolHandler) decode(property *schema.Property, data interface{}) (res interface{}, err error) {
	// different SQL drivers encode result with different type
	// so we need to do manual checks
	if data == nil {
		return nil, nil
	}
	switch t := data.(type) {
	default:
		err = fmt.Errorf("unknown type %T", t)
		return
	case []uint8: // mysql
		res, err = strconv.ParseUint(string(t), 10, 64)
		res = res.(uint64) != 0
	case int64: //apparently also mysql
		res = data.(int64) != 0
	case bool: // sqlite3
		res = data
	}
	return
}

func (handler *boolHandler) dataType(property *schema.Property) string {
	return "boolean"
}

type numberHandler struct{}

func (handler *numberHandler) encode(property *schema.Property, data interface{}) (interface{}, error) {
	return data, nil
}

func (handler *numberHandler) decode(property *schema.Property, data interface{}) (res interface{}, err error) {
	if data == nil {
		return nil, nil
	}
	switch t := data.(type) {
	default:
		return nil, fmt.Errorf("number: unknown type %T", t)

	case []uint8: // mysql
		res, _ = strconv.ParseFloat(string(t), 64)

	case float64: // sqlite3
		res = float64(t)
	case uint64: // sqlite3
		res = float64(t)
	}
	return
}

func (handler *numberHandler) dataType(property *schema.Property) string {
	return "real"
}

type integerHandler struct{}

func (handler *integerHandler) encode(property *schema.Property, data interface{}) (interface{}, error) {
	return data, nil
}

func (handler *integerHandler) decode(property *schema.Property, data interface{}) (res interface{}, err error) {
	// different SQL drivers encode result with different type
	// so we need to do manual checks
	if data == nil {
		return nil, nil
	}
	switch t := data.(type) {
	default:
		return data, nil
	case []uint8: // mysql
		res, _ = strconv.ParseInt(string(t), 10, 64)
		res = int(res.(int64))
	case int64: // sqlite3
		res = int(t)
	}
	return
}

func (handler *integerHandler) dataType(property *schema.Property) string {
	return "numeric"
}

type jsonHandler struct {
}

func (handler *jsonHandler) encode(property *schema.Property, data interface{}) (interface{}, error) {
	bytes, err := json.Marshal(data)
	//TODO(nati) should handle encoding err
	if err != nil {
		return nil, err
	}
	return string(bytes), nil
}

func (handler *jsonHandler) decode(property *schema.Property, data interface{}) (interface{}, error) {
	if bytes, ok := data.([]byte); ok {
		var ret interface{}
		err := json.Unmarshal(bytes, &ret)
		return ret, err
	}
	return data, nil
}

func (handler *jsonHandler) dataType(property *schema.Property) string {
	return "text"
}

func quote(str string) string {
	return fmt.Sprintf("`%s`", str)
}

func foreignKeyName(fromTable, fromProperty, toTable, toProperty string) string {
	name := fmt.Sprintf("%s_%s_%s_%s", fromTable, fromProperty, toTable, toProperty)
	if len(name) > 64 {
		diff := len(name) - 64
		return name[diff:]
	}
	return name
}

//Connect connec to the db
func (db *DB) Connect(sqlType, conn string, maxOpenConn int) (err error) {
	db.sqlType = sqlType
	db.connectionString = conn
	rawDB, err := sql.Open(db.sqlType, db.connectionString)
	if err != nil {
		return err
	}
	rawDB.SetMaxOpenConns(maxOpenConn)
	rawDB.SetMaxIdleConns(maxOpenConn)
	db.DB = sqlx.NewDb(rawDB, db.sqlType)

	if db.sqlType == "sqlite3" {
		db.DB.Exec("PRAGMA foreign_keys = ON;")
	}

	for i := 0; i < retryDB; i++ {
		err = db.DB.Ping()
		if err == nil {
			return nil
		}
		time.Sleep(retryDBWait * time.Second)
		log.Info("Retrying db connection... (%s)", err)
	}

	return fmt.Errorf("Failed to connect db")
}

func (db *DB) Close() {
	db.DB.Close()
}

//Begin starts new transaction
func (db *DB) Begin() (tx transaction.Transaction, err error) {
	transaction, err := db.DB.Beginx()
	if err != nil {
		return nil, err
	}
	if db.sqlType == "sqlite3" {
		transaction.Exec("PRAGMA foreign_keys = ON;")
	}
	tx = &Transaction{
		db:          db,
		transaction: transaction,
		closed:      false,
	}
	log.Debug("[%p] Created transaction %#v", transaction, transaction)
	return
}

func (db *DB) genTableCols(s *schema.Schema, cascade bool, exclude []string) ([]string, []string, []string) {
	var cols []string
	var relations []string
	var indices []string
	schemaManager := schema.GetManager()
	for _, property := range s.Properties {
		if util.ContainsString(exclude, property.ID) {
			continue
		}
		handler := db.handlers[property.Type]
		sqlDataType := property.SQLType
		sqlDataProperties := ""
		if db.sqlType == "sqlite3" {
			sqlDataType = strings.Replace(sqlDataType, "auto_increment", "autoincrement", 1)
		}
		if sqlDataType == "" {
			sqlDataType = handler.dataType(&property)
			if property.ID == "id" {
				sqlDataProperties = " primary key"
			} else {
				if property.Nullable {
					sqlDataProperties = " null"
				} else {
					sqlDataProperties = " not null"
				}
				if property.Unique {
					sqlDataProperties = " unique"
				}
			}
		}
		sql := "\n\t`" + property.ID + "` " + sqlDataType + sqlDataProperties

		cols = append(cols, sql)
		if property.Relation != "" {
			foreignSchema, _ := schemaManager.Schema(property.Relation)
			if foreignSchema != nil {
				cascadeString := ""
				if cascade ||
					property.OnDeleteCascade ||
					(property.Relation == s.Parent && s.OnParentDeleteCascade) {
					cascadeString = "on delete cascade"
				}

				relationColumn := "id"
				if property.RelationColumn != "" {
					relationColumn = property.RelationColumn
				}

				relations = append(relations, fmt.Sprintf("\n\tconstraint %s foreign key(`%s`) REFERENCES `%s`(%s) %s",
					quote(foreignKeyName(s.GetDbTableName(), property.ID, foreignSchema.GetDbTableName(), relationColumn)),
					property.ID, foreignSchema.GetDbTableName(), relationColumn, cascadeString))
			}
		}

		if property.Indexed {
			prefix := ""
			if sqlDataType == "text" {
				prefix = "(255)"
			}
			indices = append(indices, fmt.Sprintf("CREATE INDEX %s_%s_idx ON `%s`(`%s`%s);", s.Plural, property.ID,
				s.Plural, property.ID, prefix))
		}
	}
	return cols, relations, indices
}

//AlterTableDef generates alter table sql
func (db *DB) AlterTableDef(s *schema.Schema, cascade bool) (string, []string, error) {
	var existing []string
	rows, err := db.DB.Query(fmt.Sprintf("select * from `%s` limit 1;", s.GetDbTableName()))
	if err == nil {
		defer rows.Close()
		existing, err = rows.Columns()
	}

	if err != nil {
		return "", nil, err
	}

	cols, relations, indices := db.genTableCols(s, cascade, existing)
	cols = append(cols, relations...)

	if len(cols) == 0 {
		return "", nil, nil
	}
	alterTable := fmt.Sprintf("alter table`%s` add (%s);\n", s.GetDbTableName(), strings.Join(cols, ","))
	log.Debug("Altering table: " + alterTable)
	log.Debug("Altering indices: " + strings.Join(indices, ""))
	return alterTable, indices, nil
}

//GenTableDef generates create table sql
func (db *DB) GenTableDef(s *schema.Schema, cascade bool) (string, []string) {
	cols, relations, indices := db.genTableCols(s, cascade, nil)

	if s.StateVersioning() {
		cols = append(cols, quote(configVersionColumnName)+"int not null default 1")
		cols = append(cols, quote(stateVersionColumnName)+"int not null default 0")
		cols = append(cols, quote(stateErrorColumnName)+"text not null default ''")
		cols = append(cols, quote(stateColumnName)+"text not null default ''")
		cols = append(cols, quote(stateMonitoringColumnName)+"text not null default ''")
	}

	cols = append(cols, relations...)
	tableSQL := fmt.Sprintf("create table `%s` (%s\n);\n", s.GetDbTableName(), strings.Join(cols, ","))
	log.Debug("Creating table: " + tableSQL)
	log.Debug("Creating indices: " + strings.Join(indices, ""))
	return tableSQL, indices
}

//RegisterTable creates table in the db
func (db *DB) RegisterTable(s *schema.Schema, cascade, migrate bool) error {
	if s.IsAbstract() {
		return nil
	}
	tableDef, indices, err := db.AlterTableDef(s, cascade)
	if !migrate {
		if tableDef != "" || (indices != nil && len(indices) > 0) {
			return fmt.Errorf("needs migration, run \"gohan migrate\"")
		}
	}
	if err != nil {
		tableDef, indices = db.GenTableDef(s, cascade)
	}
	if tableDef == "" {
		return nil
	}
	_, err = db.DB.Exec(tableDef)
	if err != nil && indices != nil {
		for _, indexSql := range indices {
			_, err = db.DB.Exec(indexSql)
			if err != nil {
				return err
			}
		}
	}
	return err
}

//DropTable drop table definition
func (db *DB) DropTable(s *schema.Schema) error {
	if s.IsAbstract() {
		return nil
	}
	sql := fmt.Sprintf("drop table if exists %s\n", quote(s.GetDbTableName()))
	_, err := db.DB.Exec(sql)
	return err
}

func escapeID(ID string) string {
	return strings.Replace(ID, "-", "_escape_", -1)
}

func (tx *Transaction) logQuery(sql string, args ...interface{}) {
	sqlFormat := strings.Replace(sql, "?", "%s", -1)
	query := fmt.Sprintf(sqlFormat, args...)
	log.Debug("[%p] Executing SQL query '%s'", tx.transaction, query)
}

// Exec executes sql in transaction
func (tx *Transaction) Exec(sql string, args ...interface{}) error {
	tx.logQuery(sql, args...)
	_, err := tx.transaction.Exec(sql, args...)
	return err
}

//Create create resource in the db
func (tx *Transaction) Create(resource *schema.Resource) error {
	var cols []string
	var values []interface{}
	db := tx.db
	s := resource.Schema()
	data := resource.Data()
	q := sq.Insert(quote(s.GetDbTableName()))
	for _, attr := range s.Properties {
		//TODO(nati) support optional value
		if _, ok := data[attr.ID]; ok {
			handler := db.handler(&attr)
			cols = append(cols, quote(attr.ID))
			encoded, err := handler.encode(&attr, data[attr.ID])
			if err != nil {
				return fmt.Errorf("SQL Create encoding error: %s", err)
			}
			values = append(values, encoded)
		}
	}
	q = q.Columns(cols...).Values(values...)
	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}
	return tx.Exec(sql, args...)
}

func (tx *Transaction) updateQuery(resource *schema.Resource) (sq.UpdateBuilder, error) {
	s := resource.Schema()
	db := tx.db
	data := resource.Data()
	q := sq.Update(quote(s.GetDbTableName()))
	for _, attr := range s.Properties {
		//TODO(nati) support optional value
		if _, ok := data[attr.ID]; ok {
			handler := db.handler(&attr)
			encoded, err := handler.encode(&attr, data[attr.ID])
			if err != nil {
				return q, fmt.Errorf("SQL Update encoding error: %s", err)
			}
			q = q.Set(quote(attr.ID), encoded)
		}
	}
	if s.Parent != "" {
		q = q.Set(s.ParentSchemaPropertyID(), resource.ParentID())
	}
	return q, nil
}

//Update update resource in the db
func (tx *Transaction) Update(resource *schema.Resource) error {
	q, err := tx.updateQuery(resource)
	if err != nil {
		return err
	}
	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}
	if resource.Schema().StateVersioning() {
		sql += ", `" + configVersionColumnName + "` = `" + configVersionColumnName + "` + 1"
	}
	sql += " WHERE id = ?"
	args = append(args, resource.ID())
	return tx.Exec(sql, args...)
}

//StateUpdate update resource state
func (tx *Transaction) StateUpdate(resource *schema.Resource, state *transaction.ResourceState) error {
	q, err := tx.updateQuery(resource)
	if err != nil {
		return err
	}
	if resource.Schema().StateVersioning() && state != nil {
		q = q.Set(quote(stateVersionColumnName), state.StateVersion)
		q = q.Set(quote(stateErrorColumnName), state.Error)
		q = q.Set(quote(stateColumnName), state.State)
		q = q.Set(quote(stateMonitoringColumnName), state.Monitoring)
	}
	q = q.Where(sq.Eq{"id": resource.ID()})
	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}
	return tx.Exec(sql, args...)
}

//Delete delete resource from db
func (tx *Transaction) Delete(s *schema.Schema, resourceID interface{}) error {
	sql, args, err := sq.Delete(quote(s.GetDbTableName())).Where(sq.Eq{"id": resourceID}).ToSql()
	if err != nil {
		return err
	}
	return tx.Exec(sql, args...)
}

func (db *DB) handler(property *schema.Property) propertyHandler {
	handler, ok := db.handlers[property.Type]
	if ok {
		return handler
	}
	return &defaultHandler{}
}

func makeColumnID(tableName string, property schema.Property) string {
	return fmt.Sprintf("%s__%s", tableName, property.ID)
}

func makeColumn(tableName string, property schema.Property) string {
	return fmt.Sprintf("%s.%s", tableName, quote(property.ID))
}

func makeAliasTableName(tableName string, property schema.Property) string {
	return fmt.Sprintf("%s__%s", tableName, property.RelationProperty)
}

//normField returns field prefixed with schema ID.
func normField(field, schemaID string) string {
	if strings.Contains(field, ".") {
		return field
	}

	return fmt.Sprintf("%s.%s", schemaID, field)
}

// MakeColumns generates an array that has Gohan style colmun names
func MakeColumns(s *schema.Schema, tableName string, fields []string, join bool) []string {
	manager := schema.GetManager()

	var include map[string]bool
	if fields != nil {
		include = make(map[string]bool)
		for _, f := range fields {
			include[f] = true
		}
	}

	var cols []string
	for _, property := range s.Properties {
		if include != nil && !include[normField(property.ID, s.ID)] {
			continue
		}

		cols = append(cols, makeColumn(tableName, property)+" as "+quote(makeColumnID(tableName, property)))
		if property.RelationProperty != "" && join {
			relatedSchema, _ := manager.Schema(property.Relation)
			aliasTableName := makeAliasTableName(tableName, property)
			cols = append(cols, MakeColumns(relatedSchema, aliasTableName, fields, true)...)
		}
	}
	return cols
}

func makeStateColumns(s *schema.Schema) (cols []string) {
	dbTableName := s.GetDbTableName()
	cols = append(cols, dbTableName+"."+configVersionColumnName+" as "+quote(configVersionColumnName))
	cols = append(cols, dbTableName+"."+stateVersionColumnName+" as "+quote(stateVersionColumnName))
	cols = append(cols, dbTableName+"."+stateErrorColumnName+" as "+quote(stateErrorColumnName))
	cols = append(cols, dbTableName+"."+stateColumnName+" as "+quote(stateColumnName))
	cols = append(cols, dbTableName+"."+stateMonitoringColumnName+" as "+quote(stateMonitoringColumnName))
	return cols
}

func makeJoin(s *schema.Schema, tableName string, q sq.SelectBuilder) sq.SelectBuilder {
	manager := schema.GetManager()
	for _, property := range s.Properties {
		if property.RelationProperty == "" {
			continue
		}
		relatedSchema, _ := manager.Schema(property.Relation)
		aliasTableName := makeAliasTableName(tableName, property)
		q = q.LeftJoin(
			fmt.Sprintf("%s as %s on %s.%s = %s.id", quote(relatedSchema.GetDbTableName()), quote(aliasTableName),
				quote(tableName), quote(property.ID), quote(aliasTableName)))
		q = makeJoin(relatedSchema, aliasTableName, q)
	}
	return q
}

func (tx *Transaction) decode(s *schema.Schema, tableName string, data map[string]interface{}, resource map[string]interface{}) {
	manager := schema.GetManager()
	db := tx.db
	for _, property := range s.Properties {
		handler := db.handler(&property)
		value := data[makeColumnID(tableName, property)]
		if value != nil || property.Nullable {
			decoded, err := handler.decode(&property, value)
			if err != nil {
				log.Error(fmt.Sprintf("SQL List decoding error: %s", err))
			}
			resource[property.ID] = decoded
		}
		if property.RelationProperty != "" {
			relatedSchema, _ := manager.Schema(property.Relation)
			resourceData := map[string]interface{}{}
			aliasTableName := makeAliasTableName(tableName, property)
			tx.decode(relatedSchema, aliasTableName, data, resourceData)
			resource[property.RelationProperty] = resourceData
		}
	}
}

func decodeState(data map[string]interface{}, state *transaction.ResourceState) error {
	var ok bool
	state.ConfigVersion, ok = data[configVersionColumnName].(int64)
	if !ok {
		return fmt.Errorf("Wrong state column %s returned from query", configVersionColumnName)
	}
	state.StateVersion, ok = data[stateVersionColumnName].(int64)
	if !ok {
		return fmt.Errorf("Wrong state column %s returned from query", stateVersionColumnName)
	}
	stateError, ok := data[stateErrorColumnName].([]byte)
	if !ok {
		return fmt.Errorf("Wrong state column %s returned from query", stateErrorColumnName)
	}
	state.Error = string(stateError)
	stateState, ok := data[stateColumnName].([]byte)
	if !ok {
		return fmt.Errorf("Wrong state column %s returned from query", stateColumnName)
	}
	state.State = string(stateState)
	stateMonitoring, ok := data[stateMonitoringColumnName].([]byte)
	if !ok {
		return fmt.Errorf("Wrong state column %s returned from query", stateMonitoringColumnName)
	}
	state.Monitoring = string(stateMonitoring)
	return nil
}

func buildSelect(s *schema.Schema, filter transaction.Filter, pg *pagination.Paginator, join bool) (string, []interface{}, error) {
	var fields []string
	if pg != nil {
		fields = pg.Fields
	}
	if fields != nil {
		for i, f := range fields {
			fields[i] = normField(f, s.ID)
		}
	}

	cols := MakeColumns(s, s.GetDbTableName(), fields, join)
	q := sq.Select(cols...).From(quote(s.GetDbTableName()))
	q, err := addFilterToQuery(s, q, filter, join)
	if err != nil {
		return "", nil, err
	}
	if pg != nil {
		property, err := s.GetPropertyByID(pg.Key)
		if err == nil {
			q = q.OrderBy(makeColumn(s.GetDbTableName(), *property) + " " + pg.Order)
			if pg.Limit > 0 {
				q = q.Limit(pg.Limit)
			}
			if pg.Offset > 0 {
				q = q.Offset(pg.Offset)
			}
		}
	}
	if join {
		q = makeJoin(s, s.GetDbTableName(), q)
	}
	return q.ToSql()
}

func executeSelect(s *schema.Schema, filter transaction.Filter, sql string, args []interface{}, tx *Transaction) (list []*schema.Resource, total uint64, err error) {
	tx.logQuery(sql, args...)
	rows, err := tx.transaction.Queryx(sql, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	list, err = tx.decodeRows(s, rows, list)
	if err != nil {
		return nil, 0, err
	}
	total, err = tx.count(s, filter)
	return
}

//List resources in the db
func (tx *Transaction) List(s *schema.Schema, filter transaction.Filter, pg *pagination.Paginator) (list []*schema.Resource, total uint64, err error) {
	sql, args, err := buildSelect(s, filter, pg, true)
	if err != nil {
		return nil, 0, err
	}

	return executeSelect(s, filter, sql, args, tx)
}

func shouldJoin(policy schema.LockPolicy) bool {
	switch policy {
	case schema.LockRelatedResources:
		return true
	case schema.SkipRelatedResources:
		return false
	default:
		log.Fatalf("Unknown lock policy %+v", policy)
		panic("Unexpected locking policy")
	}
}

//Lock resources in the db
func (tx *Transaction) LockList(s *schema.Schema, filter transaction.Filter, pg *pagination.Paginator, lockPolicy schema.LockPolicy) (list []*schema.Resource, total uint64, err error) {
	sql, args, err := buildSelect(s, filter, pg, shouldJoin(lockPolicy))
	if err != nil {
		return nil, 0, err
	}

	if tx.db.sqlType == "mysql" {
		sql += " FOR UPDATE"
	}

	return executeSelect(s, filter, sql, args, tx)
}

// Query with raw sql string
func (tx *Transaction) Query(s *schema.Schema, query string, arguments []interface{}) (list []*schema.Resource, err error) {
	tx.logQuery(query, arguments...)
	rows, err := tx.transaction.Queryx(query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("Failed to run query: %s", query)
	}

	defer rows.Close()
	list, err = tx.decodeRows(s, rows, list)
	if err != nil {
		return nil, err
	}

	return
}

func (tx *Transaction) decodeRows(s *schema.Schema, rows *sqlx.Rows, list []*schema.Resource) ([]*schema.Resource, error) {
	for rows.Next() {
		resourceData := map[string]interface{}{}
		data := map[string]interface{}{}
		rows.MapScan(data)

		var resource *schema.Resource
		tx.decode(s, s.GetDbTableName(), data, resourceData)
		resource, err := schema.NewResource(s, resourceData)
		if err != nil {
			return nil, fmt.Errorf("Failed to decode rows")
		}
		list = append(list, resource)
	}
	return list, nil
}

//count count all matching resources in the db
func (tx *Transaction) count(s *schema.Schema, filter transaction.Filter) (res uint64, err error) {
	q := sq.Select("Count(id) as count").From(quote(s.GetDbTableName()))
	//Filter get already tested
	q, _ = addFilterToQuery(s, q, filter, false)
	sql, args, err := q.ToSql()
	if err != nil {
		return
	}
	result := map[string]interface{}{}
	err = tx.transaction.QueryRowx(sql, args...).MapScan(result)
	if err != nil {
		return
	}
	count, _ := result["count"]
	decoder := &integerHandler{}
	decoded, decodeErr := decoder.decode(nil, count)
	if decodeErr != nil {
		err = fmt.Errorf("SQL List decoding error: %s", decodeErr)
		return
	}
	res = uint64(decoded.(int))
	return
}

//Fetch resources by ID in the db
func (tx *Transaction) Fetch(s *schema.Schema, filter transaction.Filter) (*schema.Resource, error) {
	list, _, err := tx.List(s, filter, nil)
	if len(list) < 1 {
		return nil, fmt.Errorf("Failed to fetch %s", filter)
	}
	return list[0], err
}

//Fetch & lock a resource
func (tx *Transaction) LockFetch(s *schema.Schema, filter transaction.Filter, lockPolicy schema.LockPolicy) (*schema.Resource, error) {
	list, _, err := tx.LockList(s, filter, nil, lockPolicy)
	if len(list) < 1 {
		return nil, fmt.Errorf("Failed to fetch and lock %s", filter)
	}
	return list[0], err
}

//StateFetch fetches the state of the specified resource
func (tx *Transaction) StateFetch(s *schema.Schema, filter transaction.Filter) (state transaction.ResourceState, err error) {
	if !s.StateVersioning() {
		err = fmt.Errorf("Schema %s does not support state versioning.", s.ID)
		return
	}
	cols := makeStateColumns(s)
	q := sq.Select(cols...).From(quote(s.GetDbTableName()))
	q, _ = addFilterToQuery(s, q, filter, true)
	sql, args, err := q.ToSql()
	if err != nil {
		return
	}
	tx.logQuery(sql, args...)
	rows, err := tx.transaction.Queryx(sql, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	if !rows.Next() {
		err = fmt.Errorf("No resource found")
		return
	}
	data := map[string]interface{}{}
	rows.MapScan(data)
	err = decodeState(data, &state)
	return
}

//RawTransaction returns raw transaction
func (tx *Transaction) RawTransaction() *sqlx.Tx {
	return tx.transaction
}

//SetIsolationLevel specify transaction isolation level
func (tx *Transaction) SetIsolationLevel(level transaction.Type) error {
	log.Debug("[%p] Setting isolation level for transaction %#v %s", tx.transaction, tx, level)
	if tx.db.sqlType == "mysql" {
		err := tx.Exec(fmt.Sprintf("set session transaction isolation level %s", level))
		return err
	}
	return nil
}

//Commit commits transaction
func (tx *Transaction) Commit() error {
	log.Debug("[%p] Committing transaction %#v", tx.transaction, tx)
	err := tx.transaction.Commit()
	if err != nil {
		log.Error("[%p] Commit %#v failed: %s", tx.transaction, tx, err)
		return err
	}
	tx.closed = true
	return nil
}

//Close closes connection
func (tx *Transaction) Close() error {
	//Rollback if it isn't committed yet
	log.Debug("[%p] Closing transaction %#v", tx.transaction, tx)
	var err error
	if !tx.closed {
		log.Debug("[%p] Rolling back %#v", tx.transaction, tx)
		err = tx.transaction.Rollback()
		if err != nil {
			log.Error("[%p] Rolling back %#v failed: %s", tx.transaction, tx, err)
			return err
		}
		tx.closed = true
	}
	return nil
}

//Closed returns whether the transaction is closed
func (tx *Transaction) Closed() bool {
	return tx.closed
}

func addFilterToQuery(s *schema.Schema, q sq.SelectBuilder, filter map[string]interface{}, join bool) (sq.SelectBuilder, error) {
	if filter == nil {
		return q, nil
	}
	for key, value := range filter {
		property, err := s.GetPropertyByID(key)

		if err != nil {
			return q, err
		}

		var column string
		if join {
			column = makeColumn(s.GetDbTableName(), *property)
		} else {
			column = quote(key)
		}

		queryValues, ok := value.([]string)
		if ok && property.Type == "boolean" {
			v := make([]bool, len(queryValues))
			for i, j := range queryValues {
				v[i], _ = strconv.ParseBool(j)
			}
			q = q.Where(sq.Eq{column: v})
		} else {
			q = q.Where(sq.Eq{column: value})
		}
	}
	return q, nil
}

//SetMaxOpenConns limit maximum connections
func (db *DB) SetMaxOpenConns(maxIdleConns int) {
	// db.DB.SetMaxOpenConns(maxIdleConns)
	// db.DB.SetMaxIdleConns(maxIdleConns)
}
