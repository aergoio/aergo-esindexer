package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"

	doc "github.com/aergoio/aergo-indexer/indexer/documents"
	// import mysql driver
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// MariaDbController implements DbController
type MariaDbController struct {
	Client *sqlx.DB
}

// NewMariaDbController creates a new instance of ElasticsearchDbController
func NewMariaDbController(dbURL string) (*MariaDbController, error) {
	client, err := sqlx.Connect("mysql", dbURL+"?parseTime=true")
	if err != nil {
		return nil, err
	}
	return &MariaDbController{
		Client: client,
	}, nil
}

func prepareFieldsAndBinds(document doc.DocType) ([]string, []string) {
	t := reflect.TypeOf(document)
	v := reflect.ValueOf(document)
	fields := []string{"id"}
	binds := []string{":id"}
	for i := 0; i < v.NumField(); i++ {
		tag := t.Field(i).Tag.Get("db")
		//fmt.Printf("%+v\n", t.Field(i))
		if tag == "" {
			continue
		}
		fields = append(fields, tag)
		binds = append(binds, ":"+tag)
	}
	return fields, binds
}

func prepareSelectFields(fields []string) string {
	fieldStr := "*"
	if fields != nil {
		quotedFields := make([]string, 0, len(fields))
		for _, f := range fields {
			quotedFields = append(quotedFields, "`"+f+"`")
		}
		fieldStr = strings.Join(quotedFields, ",")
	}
	return fieldStr
}

// Insert inserts a single document using the updata params
// It returns the number of inserted documents (1) or an error
func (mdb MariaDbController) Insert(document doc.DocType, params UpdateParams) (uint64, error) {
	fields, binds := prepareFieldsAndBinds(document)
	method := "INSERT"
	if params.Upsert {
		method = "REPLACE"
	}
	query := fmt.Sprintf("%s INTO `%s` (%s) VALUES (%s)", method, params.IndexName, strings.Join(fields, ","), strings.Join(binds, ","))
	result, err := mdb.Client.NamedExec(query, document)
	if err != nil {
		return 0, err
	}
	rowsAffected, _ := result.RowsAffected()
	return uint64(rowsAffected), nil
}

// InsertBulk inserts documents arriving in documentChannel in bulk using the updata params
// It returns the number of inserted documents or an error
func (mdb MariaDbController) InsertBulk(documentChannel chan doc.DocType, params UpdateParams) (uint64, error) {
	ctx := context.Background()
	var fields []string
	var binds []string
	method := "INSERT"
	if params.Upsert {
		method = "REPLACE"
	}
	query := fmt.Sprintf("%s IGNORE INTO `%s` (%s) VALUES (%s)", method, params.IndexName, strings.Join(fields, ","), strings.Join(binds, ","))
	var total uint64
	var bulk []doc.DocType

	commitBulk := func(bulk []doc.DocType) error {
		if len(bulk) == 0 {
			return nil
		}
		result, err := mdb.Client.NamedExec(query, bulk)
		if err != nil {
			fmt.Printf("Error while inserting bulk: %+v\n", err)
			return err
		}
		rowsAffected, _ := result.RowsAffected()
		atomic.AddUint64(&total, uint64(rowsAffected))
		return nil
	}

	for d := range documentChannel {
		if len(fields) == 0 {
			fields, binds = prepareFieldsAndBinds(d)
			query = fmt.Sprintf("INSERT IGNORE INTO `%s` (%s) VALUES (%s)", params.IndexName, strings.Join(fields, ","), strings.Join(binds, ","))
		}
		bulk = append(bulk, d)
		if len(bulk) >= params.Size {
			if err := commitBulk(bulk); err != nil {
				return total, err
			}
			bulk = nil
		}

		select {
		default:
		case <-ctx.Done():
			return total, ctx.Err()
		}
	}
	// Commit the final batch before exiting
	if err := commitBulk(bulk); err != nil {
		return total, err
	}
	return total, nil
}

// Delete removes documents specified by the query params
func (mdb *MariaDbController) Delete(params QueryParams) (uint64, error) {
	// TODO
	return 0, nil
}

// Count returns the number of indexed documents
func (mdb *MariaDbController) Count(params QueryParams) (int64, error) {
	var count int64
	query := fmt.Sprintf("SELECT count(*) FROM `%s`", params.IndexName)
	err := mdb.Client.Get(&count, query)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// SelectOne selects a single document
func (mdb *MariaDbController) SelectOne(params QueryParams, document doc.DocType) error {
	sortOrder := "DESC"
	if params.SortAsc {
		sortOrder = "ASC"
	}
	fields := prepareSelectFields(params.SelectFields)
	query := fmt.Sprintf("SELECT %s FROM `%s` ORDER BY `%s` %s LIMIT 1", fields, params.IndexName, params.SortField, sortOrder)
	err := mdb.Client.Get(document, query)
	if err != nil {
		return err
	}
	return nil
}

// UpdateAlias updates an alias with a new index name
func (mdb *MariaDbController) UpdateAlias(aliasName string, indexName string) error {
	query := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS SELECT * FROM `%s`;", aliasName, indexName)
	_, err := mdb.Client.Exec(query)
	return err
}

// GetExistingIndexPrefix checks for existing indices and returns the prefix, if any
func (mdb *MariaDbController) GetExistingIndexPrefix(aliasName string, documentType string) (bool, string, error) {
	// Get list of views
	var tableName string
	query := fmt.Sprintf(`
		select 
		case 
			when view_definition regexp '.*from +.*'
			then substring_index(substring_index(view_definition, 'from ', -1), ' ', 1)
		end as 'table_name'
		from information_schema.views where table_name LIKE "%%_%s"`, documentType)
	err := mdb.Client.Get(&tableName, query)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	// Extract table name from view definition
	re := regexp.MustCompile(fmt.Sprintf("\\.`(.*?)%s", documentType))
	matches := re.FindStringSubmatch(tableName)
	if len(matches) > 1 {
		return true, matches[1], nil
	}
	return false, "", fmt.Errorf("could not match table prefix in %s", tableName)
}

// CreateIndex creates index according to documentType definition
func (mdb *MariaDbController) CreateIndex(indexName string, documentType string) error {
	statement := strings.Replace(doc.SQLSchemas[documentType], "%indexName%", indexName, -1)
	_, err := mdb.Client.Exec(statement)
	return err
}

// Scroll creates a new scroll instance with the specified query and unmarshal function
func (mdb *MariaDbController) Scroll(params QueryParams, createDocument CreateDocFunction) ScrollInstance {
	return &MariaScrollInstance{
		ctx:            context.Background(),
		createDocument: createDocument,
		client:         mdb.Client,
		params:         params,
		currentFrom:    0,
		current:        0,
	}
}

// MariaScrollInstance is an instance of a scroll for ES
type MariaScrollInstance struct {
	result  *sqlx.Rows
	current int
	//currentLength  int
	currentFrom    int
	ctx            context.Context
	createDocument CreateDocFunction
	client         *sqlx.DB
	params         QueryParams
}

// Next returns the next document of a scroll or io.EOF
func (scroll *MariaScrollInstance) Next() (doc.DocType, error) {
	// Load next part of scroll
	hasNext := scroll.result != nil && scroll.result.Next()
	if scroll.result == nil || !hasNext && scroll.current >= scroll.params.Size {
		sortOrder := "DESC"
		if scroll.params.SortAsc {
			sortOrder = "ASC"
		}
		fields := prepareSelectFields(scroll.params.SelectFields)
		query := fmt.Sprintf(
			"SELECT %s FROM `%s` ORDER BY `%s` %s LIMIT %d, %d",
			fields,
			scroll.params.IndexName,
			scroll.params.SortField,
			sortOrder,
			scroll.currentFrom,
			scroll.params.Size,
		)
		scroll.currentFrom += scroll.params.Size
		result, err := scroll.client.Queryx(query)
		if err != nil {
			return nil, err // returns io.EOF when scroll is done
		}
		scroll.result = result
		scroll.current = 0
	}

	// Return next document
	if hasNext || scroll.result.Next() {
		doc := scroll.createDocument()
		scroll.current++
		err := scroll.result.StructScan(doc)
		if err != nil {
			return nil, err
		}
		return doc, nil
	}

	return nil, io.EOF
}
