package elasticsearch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	u "github.com/araddon/gou"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/lex"
	"github.com/araddon/qlbridge/value"
	"github.com/dataux/dataux/pkg/models"
	"github.com/dataux/dataux/vendor/mixer/mysql"
	"github.com/dataux/dataux/vendor/mixer/proxy"
	"github.com/kr/pretty"
)

/*

Lets turn this into Three things

- FrontEnd:
  - stmt := stmt.Parse(txt)
  - myresults := NewMysqlResultWriter(stmt)
  - err := NewHandler(myresults, stmt)

- Handler(resultWriter, stmt):
   job := newJobRunner(config, resultWriter)
   err := job.Accept(stmt)

- MysqlResultWriter

*/

var _ = value.ErrValue
var _ = u.EMPTY
var _ = pretty.Diff

const ListenerType = "elasticsearch"

var (
	_ models.HandlerSession = (*HandlerElasticsearch)(nil)
	_ models.Handler        = (*HandlerElasticsearch)(nil)
)

// Handle request splitting, a single connection session
// not threadsafe, not shared
type HandlerElasticsearchShared struct {
	conf    *models.Config
	nodes   map[string]*models.BackendConfig // List of servers
	schemas map[string]*models.Schema        // List of Schemas
	schema  *models.Schema
}

// Handle request splitting, a single connection session
// not threadsafe, not shared
type HandlerElasticsearch struct {
	*HandlerElasticsearchShared
	conn *proxy.Conn
}

func NewHandlerElasticsearch(conf *models.Config) (models.Handler, error) {
	handler := &HandlerElasticsearchShared{conf: conf}
	err := handler.Init()
	connHandler := &HandlerElasticsearch{HandlerElasticsearchShared: handler}
	return connHandler, err
}

func (m *HandlerElasticsearchShared) Init() error {

	u.Debugf("Init()")
	if err := m.findEsNodes(); err != nil {
		u.Errorf("could not init es: %v", err)
		return err
	}
	if err := m.loadSchemasFromConfig(); err != nil {
		return err
	}
	return nil
}

func (m *HandlerElasticsearch) Clone(connI interface{}) models.Handler {

	handler := HandlerElasticsearch{HandlerElasticsearchShared: m.HandlerElasticsearchShared}
	if conn, ok := connI.(*proxy.Conn); ok {
		u.Debugf("Cloning shared handler %v", conn)
		handler.conn = conn
		return &handler
	}
	panic("not cloneable")
}

func (m *HandlerElasticsearch) Close() error {
	return m.conn.Close()
}

func (m *HandlerElasticsearch) Handle(writer models.ResultWriter, req *models.Request) error {
	return m.chooseCommand(writer, req)
}

func (m *HandlerElasticsearch) SchemaUse(db string) *models.Schema {
	schema, ok := m.schemas[db]
	if ok {
		m.schema = schema
		u.Debugf("Use Schema: %v", db)
	} else {
		u.Warnf("Could not find schema for db=%s", db)
	}
	return schema
}

func (m *HandlerElasticsearch) chooseCommand(writer models.ResultWriter, req *models.Request) error {

	cmd := req.Raw[0]
	req.Raw = req.Raw[1:]

	u.Debugf("chooseCommand: %v:%v", cmd, mysql.CommandString(cmd))
	switch cmd {
	case mysql.COM_QUERY:
		return m.handleQuery(string(req.Raw))
	case mysql.COM_PING:
		return m.writeOK(nil)
	case mysql.COM_STMT_PREPARE:
		return m.handleQuery(string(req.Raw))
		//return m.handleStmtPrepare(string(req.Raw))
	// case mysql.COM_STMT_EXECUTE:
	// 	return m.handleStmtExecute(req.Raw)
	case mysql.COM_QUIT:
		m.Close()
		return nil
	case mysql.COM_INIT_DB:
		if s := m.SchemaUse(string(req.Raw)); s == nil {
			return fmt.Errorf("Schema not found %s", string(req.Raw))
		} else {
			return m.writeOK(nil)
		}
	// case mysql.COM_FIELD_LIST:
	// 	return m.handleFieldList(req.Raw)
	// case mysql.COM_STMT_CLOSE:
	// 	return m.handleStmtClose(req.Raw)
	// 	case mysql.COM_STMT_SEND_LONG_DATA:
	// 		return c.handleStmtSendLongData(req.Raw)
	// 	case mysql.COM_STMT_RESET:
	// 		return c.handleStmtReset(req.Raw)
	default:
		msg := fmt.Sprintf("command %d:%s not supported for now", cmd, mysql.CommandString(cmd))
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR, msg)
	}

	return nil
}

func (m *HandlerElasticsearch) handleQuery(sql string) (err error) {
	u.Debugf("handleQuery: %v", sql)
	if !m.conf.SupressRecover {
		//u.Debugf("running recovery? ")
		defer func() {
			if e := recover(); e != nil {
				u.Errorf("recover? %v", e)
				err = fmt.Errorf("handle query %s error %v", sql, e)
				return
			}
		}()
	}

	// Ensure it parses, right now we can't handle multiple statement (ie with semi-colons separating)
	//sql = strings.TrimRight(sql, ";")
	stmt, err := expr.ParseSql(sql)
	if err != nil {
		sql = strings.ToLower(sql)
		switch {
		case strings.Contains(sql, "set autocommit"):
			return m.conn.WriteOK(nil)
		case strings.Contains(sql, "set session transaction isolation"):
			// SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ
			return m.conn.WriteOK(nil)
		}
		u.Debugf("error on parse sql statement: %v", err)
		return err
	}

	//u.Debugf("handleQuery: %T %#v", stmt, stmt)
	switch stmtNode := stmt.(type) {
	case *expr.SqlDescribe:
		switch {
		case stmtNode.Identity != "":
			//u.Warnf("describe not supported?  %v  %T", v, stmt)
			//return fmt.Errorf("Describe not supported yet")
			return m.handleDescribeTable(sql, stmtNode)
		case stmtNode.Stmt != nil && stmtNode.Stmt.Keyword() == lex.TokenSelect:
			u.Infof("describe/explain: %#v", stmtNode)
		default:
			u.Warnf("unrecognized describe/explain: %#v", stmtNode)
		}
		return fmt.Errorf("describe/explain not yet supported: %#v", stmtNode)
	// case *sqlparser.SimpleSelect:
	// 		return m.handleSimpleSelect(sql, stmtNode)
	case *expr.SqlSelect:
		if sysVar := stmtNode.SysVariable(); len(sysVar) > 0 {
			return m.handleSelectSysVariable(sql, stmtNode, sysVar)
		}
		return m.handleSelect(sql, stmtNode, nil)
	// case *sqlparser.Insert:
	// 	return m.handleExec(stmt, sql, nil)
	// case *sqlparser.Update:
	// 	return m.handleExec(stmt, sql, nil)
	// case *sqlparser.Delete:
	// 	return m.handleExec(stmt, sql, nil)
	// case *sqlparser.Replace:
	// 	return m.handleExec(stmt, sql, nil)
	// case *sqlparser.Set:
	// 	return m.handleSet(stmtNode)
	// case *sqlparser.Begin:
	// 	return m.handleBegin()
	//case *sqlparser.Commit:
	// 	return m.handleCommit()
	// case *sqlparser.Rollback:
	// 	return m.handleRollback()
	case *expr.SqlShow:
		return m.handleShow(sql, stmtNode)
	// case *sqlparser.Admin:
	// 	return m.handleAdmin(stmtNode)
	default:
		u.Warnf("sql not supported?  %v  %T", stmtNode, stmtNode)
		return fmt.Errorf("statement %T not support now", stmtNode)
	}

	return nil
}

func (m *HandlerElasticsearch) handleSelect(sql string, req *expr.SqlSelect, args []interface{}) error {

	// UGH, this code is horrible, it was a spike to learn
	// what is possible with sql -> es -> sql mapping
	// now have a better idea on limitations, etc and can start to build out a
	// better set of code
	u.Debugf("handleSelect: \n\t%v", sql)
	if m.schema == nil {
		u.Warnf("missing schema?  ")
		return fmt.Errorf("no schema in use")
	}

	tblName := ""
	if len(req.From) > 1 {
		return fmt.Errorf("join not implemented")
	}
	tblName = strings.ToLower(req.From[0].Name)

	tbl, err := m.loadTableSchema(tblName)
	if err != nil {
		u.Errorf("error: %v", err)
		return fmt.Errorf("Could not find table '%v' schema", tblName)
	}
	es := NewSqlToEs(tbl)
	u.Debugf("sqltoes: %#v", es)
	resp, err := es.Query(req)
	if err != nil {
		u.Error(err)
		return err
	}

	rw := NewMysqlResultWriter(m.conn, req, resp, es)
	// Lets write out Column Headers if this
	// resultWriter needs it
	if err := rw.WriteHeaders(); err != nil {
		return err
	}

	if err := rw.Finalize(); err != nil {
		u.Error(err)
		return err
	}
	return m.conn.WriteResultset(m.conn.Status, rw.rs)
}

func (m *HandlerElasticsearch) handleSelectSysVariable(sql string, stmt *expr.SqlSelect, sysVar string) error {
	switch sysVar {
	case "@@max_allowed_packet":

	}
	//r, _ := m.conn.BuildSimpleShowResultset(values, fmt.Sprintf("%v", 1024*1024))
	r, _ := proxy.BuildSimpleSelectResult(1024*1024, []byte(sysVar), nil)
	return m.conn.WriteResultset(m.conn.Status, r)
}

func (m *HandlerElasticsearch) handleDescribeTable(sql string, req *expr.SqlDescribe) error {

	s := m.schema
	if s == nil {
		return mysql.NewDefaultError(mysql.ER_NO_DB_ERROR)
	}
	tableName := strings.ToLower(req.Identity)

	tbl, err := m.loadTableSchema(tableName)
	if err != nil {
		return err
	}

	return m.conn.WriteResultset(m.conn.Status, tbl.DescribeResultset())
}

func (m *HandlerElasticsearch) handleShow(sql string, stmt *expr.SqlShow) error {
	var err error
	var r *mysql.Resultset
	switch strings.ToLower(stmt.Identity) {
	case "databases":
		r, err = m.handleShowDatabases()
	case "tables":
		r, err = m.handleShowTables(sql, stmt)
	// case "proxy":
	// 	r, err = m.handleShowProxy(sql, stmt)
	default:
		err = fmt.Errorf("unsupport show %s now", sql)
	}

	if err != nil {
		return err
	}

	return m.conn.WriteResultset(m.conn.Status, r)
}

func (m *HandlerElasticsearch) handleShowDatabases() (*mysql.Resultset, error) {
	dbs := make([]interface{}, 0, len(m.schemas))
	for key := range m.schemas {
		dbs = append(dbs, key)
	}

	return m.conn.BuildSimpleShowResultset(dbs, "Database")
}

func (m *HandlerElasticsearch) handleShowTables(sql string, stmt *expr.SqlShow) (*mysql.Resultset, error) {
	s := m.schema
	if stmt.From != "" {
		s = m.getSchema(strings.ToLower(stmt.From))
	}

	if s == nil {
		u.Warnf("no schema? %v", stmt)
		return nil, mysql.NewDefaultError(mysql.ER_NO_DB_ERROR)
	}
	if len(s.TableNames) == 0 {
		u.Errorf("no tables? %#v", s)
		return nil, fmt.Errorf("No tables found?")
	}

	values := make([]interface{}, len(s.TableNames))
	for i, name := range s.TableNames {
		values[i] = name
	}
	u.Debugf("values: %v", values)
	return m.conn.BuildSimpleShowResultset(values, fmt.Sprintf("Tables_in_%s", s.Db))
}

func (m *HandlerElasticsearchShared) loadSchemasFromConfig() error {

	m.schemas = make(map[string]*models.Schema)

	for _, schemaConf := range m.conf.Schemas {

		if schemaConf.BackendType == "elasticsearch" {

			u.Debugf("parse schemas: %v", schemaConf)
			if _, ok := m.schemas[schemaConf.DB]; ok {
				return fmt.Errorf("duplicate schema '%s'", schemaConf.DB)
			}
			if len(schemaConf.Backends) == 0 {
				u.Warnf("schema '%s' should have at least one node", schemaConf.DB)
			}

			schema := &models.Schema{
				Db:          schemaConf.DB,
				Address:     schemaConf.Address,
				BackendType: schemaConf.BackendType,
				Tables:      make(map[string]*models.Table),
			}

			m.schemas[schemaConf.DB] = schema
			m.schema = schema

		} else {
			u.Debugf("found schema not intended for this handler; %v", schemaConf.BackendType)
		}
	}

	m.loadTables()

	return nil
}

func (m *HandlerElasticsearchShared) loadTables() error {

	jh, err := u.JsonHelperHttp("GET", "http://localhost:9200/_aliases", nil)
	if err != nil {
		u.Error("error on es read: %v", err)
		return err
	}
	//u.Debugf("resp: %v", jh)
	tables := []string{}
	for alias, _ := range jh {
		tables = append(tables, alias)
	}
	sort.Strings(tables)

	// move this to an initial load?
	if m.schema == nil {
		u.Infof("no schema? %v")
	}
	m.schema.TableNames = tables
	u.Debugf("found tables: %v", m.schema.TableNames)

	return nil
}

func (m *HandlerElasticsearchShared) loadTableSchema(table string) (*models.Table, error) {

	if m.schema == nil {
		return nil, fmt.Errorf("no schema in use")
	}
	// check cache first
	if tbl, ok := m.schema.Tables[table]; ok {
		return tbl, nil
	}

	s := m.schema
	host := s.ChooseBackend()
	if m.schema.Address == "" {
		u.Errorf("missing address: %#v", m.schema)
	}
	tbl := models.NewTable(table, m.schema)

	indexUrl := fmt.Sprintf("%s/%s/_mapping", host, tbl.Name)
	respJh, err := u.JsonHelperHttp("GET", indexUrl, nil)
	if err != nil {
		u.Error("error on es read: url=%v  err=%v", indexUrl, err)
	}
	u.Debugf("url: %v", indexUrl)
	respJh = respJh.Helper(table + ".mappings")
	respKeys := respJh.Keys()
	//u.Infof("keys:%v  resp:%v", respKeys, respJh)
	if len(respKeys) < 1 {
		u.Errorf("could not get data? %v   %v", indexUrl, respJh)
		return nil, fmt.Errorf("Could not process desribe")
	}
	indexType := "user"
	for _, key := range respKeys {
		if key != "_default_" {
			indexType = key
			break
		}
	}

	jh := respJh.Helper(indexType)
	//u.Debugf("resp: %v", jh)
	jh = jh.Helper("properties")

	// tbl.AddField(mysql.NewField("_id", s.Db, s.Db, 24, mysql.MYSQL_TYPE_STRING))
	// tbl.AddField(mysql.NewField("type", s.Db, s.Db, 24, mysql.MYSQL_TYPE_STRING))
	// tbl.AddField(mysql.NewField("_score", s.Db, s.Db, 24, mysql.MYSQL_TYPE_FLOAT))
	tbl.AddField(models.NewField("_id", value.StringType, 24, "AUTOGEN"))
	tbl.AddField(models.NewField("type", value.StringType, 24, "tbd"))
	tbl.AddField(models.NewField("_score", value.NumberType, 24, "Created per Search By Elasticsearch"))

	tbl.AddValues([]interface{}{"_id", "string", "NO", "PRI", "AUTOGEN", ""})
	tbl.AddValues([]interface{}{"type", "string", "NO", "", nil, "tbd"})
	tbl.AddValues([]interface{}{"_score", "float", "NO", "", nil, "Created per search"})

	buildEsFields(s, tbl, jh, "", 0)
	m.schema.Tables[table] = tbl

	return tbl, nil
}

func buildEsFields(s *models.Schema, tbl *models.Table, jh u.JsonHelper, prefix string, depth int) {
	for field, _ := range jh {

		if h := jh.Helper(field); len(h) > 0 {
			jb, _ := json.Marshal(h)
			//jb, _ := json.MarshalIndent(h, " ", " ")
			fieldName := prefix + field
			var fld *models.Field
			//u.Infof("%v %v", fieldName, h)
			switch esType := h.String("type"); esType {
			case "boolean":
				tbl.AddValues([]interface{}{fieldName, esType, "YES", "", nil, jb})
				//fld = mysql.NewField(fieldName, s.Db, s.Db, 1, mysql.MYSQL_TYPE_TINY)
				fld = models.NewField(fieldName, value.BoolType, 1, string(jb))
			case "string":
				tbl.AddValues([]interface{}{fieldName, esType, "YES", "", nil, jb})
				//fld = mysql.NewField(fieldName, s.Db, s.Db, 512, mysql.MYSQL_TYPE_STRING)
				fld = models.NewField(fieldName, value.StringType, 512, string(jb))
			case "date":
				tbl.AddValues([]interface{}{fieldName, esType, "YES", "", nil, jb})
				//fld = mysql.NewField(fieldName, s.Db, s.Db, 32, mysql.MYSQL_TYPE_DATETIME)
				fld = models.NewField(fieldName, value.TimeType, 4, string(jb))
			case "int", "long", "integer":
				tbl.AddValues([]interface{}{fieldName, esType, "YES", "", nil, jb})
				//fld = mysql.NewField(fieldName, s.Db, s.Db, 64, mysql.MYSQL_TYPE_LONG)
				fld = models.NewField(fieldName, value.IntType, 8, string(jb))
			case "nested":
				tbl.AddValues([]interface{}{fieldName, esType, "YES", "", nil, jb})
				//fld = mysql.NewField(fieldName, s.Db, s.Db, 2000, mysql.MYSQL_TYPE_BLOB)
				fld = models.NewField(fieldName, value.StringType, 2000, string(jb))
			default:
				tbl.AddValues([]interface{}{fieldName, "object", "YES", "", nil, `{"type":"object"}`})
				//fld = mysql.NewField(fieldName, s.Db, s.Db, 2000, mysql.MYSQL_TYPE_BLOB)
				fld = models.NewField(fieldName, value.StringType, 2000, `{"type":"object"}`)
				props := h.Helper("properties")
				if len(props) > 0 {
					buildEsFields(s, tbl, props, fieldName+".", depth+1)
				} else {
					u.Debugf("unknown type: %v", string(jb))
				}

			}
			if fld != nil {
				tbl.AddField(fld)
			}

		}
	}
}

func (m *HandlerElasticsearchShared) getSchema(db string) *models.Schema {
	u.Debugf("get schema for %s", db)
	return m.schemas[db]
}

func (m *HandlerElasticsearchShared) findEsNodes() error {

	//m.nodes = make(map[string]*Node)

	for _, be := range m.conf.Backends {
		if be.BackendType == "" {
			for _, schemaConf := range m.conf.Schemas {
				for _, bename := range schemaConf.Backends {
					if bename == be.Name {
						be.BackendType = schemaConf.BackendType
					}
				}
			}
		}
		if be.BackendType == ListenerType {
			// if _, ok := m.nodes[be.Name]; ok {
			// 	return fmt.Errorf("duplicate node '%s'", be.Name)
			// }

			// n, err := m.startMysqlNode(be)
			// if err != nil {
			// 	return err
			// }

			u.Debugf("adding node: %s", be.String())
			//m.nodes[be.Name] = n
		}
	}

	return nil
}

func (m *HandlerElasticsearch) writeOK(r *mysql.Result) error {
	return m.conn.WriteOK(r)
}

func makeBindVars(args []interface{}) map[string]interface{} {
	bindVars := make(map[string]interface{}, len(args))

	for i, v := range args {
		bindVars[fmt.Sprintf("v%d", i+1)] = v
	}

	return bindVars
}
