// Package d1 implements an Ent SQLite driver backed directly by Cloudflare D1.
package d1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// Driver executes SQLite statements against a D1 database.
type Driver struct {
	client *client
	mu     sync.RWMutex
	closed bool
}

// New constructs a driver for the Cloudflare management REST API.
func New(accountID, databaseID, token string) (*Driver, error) {
	accountID = strings.TrimSpace(accountID)
	databaseID = strings.TrimSpace(databaseID)
	if accountID == "" {
		return nil, fmt.Errorf("D1 account ID is required")
	}
	if databaseID == "" {
		return nil, fmt.Errorf("D1 database ID is required")
	}
	endpoint := apiEndpoint + "/accounts/" + url.PathEscape(accountID) +
		"/d1/database/" + url.PathEscape(databaseID) + "/query"
	value, err := newClient(endpoint, token)
	if err != nil {
		return nil, err
	}
	return &Driver{client: value}, nil
}

// NewBinding constructs a driver for the private D1 binding adapter exposed to
// a Cloudflare Container.
func NewBinding() (*Driver, error) {
	value, err := newClient(bindingEndpoint, "")
	if err != nil {
		return nil, err
	}
	return &Driver{client: value}, nil
}

func (*Driver) Dialect() string { return dialect.SQLite }

func (d *Driver) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

func (d *Driver) checkOpen() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return fmt.Errorf("d1 driver is closed")
	}
	return nil
}

// Tx is intentionally unsupported: D1 transactions cannot remain open across
// HTTP requests. Application transactions use Version and Commit instead.
func (d *Driver) Tx(context.Context) (dialect.Tx, error) {
	return nil, ErrTransactionsUnsupported
}

func (d *Driver) Exec(ctx context.Context, query string, args, destination any) error {
	if err := d.checkOpen(); err != nil {
		return err
	}
	arguments, err := argumentsOf(args)
	if err != nil {
		return err
	}
	if destination != nil {
		if _, ok := destination.(*sql.Result); !ok {
			return fmt.Errorf("D1: invalid Exec destination %T; want *sql.Result", destination)
		}
	}
	result, err := d.write(ctx, Statement{SQL: query, Params: arguments})
	if err != nil {
		return err
	}
	if destination != nil {
		*destination.(*sql.Result) = writeResult{lastInsertID: result.LastInsertID, rowsAffected: result.Changes}
	}
	return nil
}

func (d *Driver) Query(ctx context.Context, query string, args, destination any) error {
	if err := d.checkOpen(); err != nil {
		return err
	}
	arguments, err := argumentsOf(args)
	if err != nil {
		return err
	}
	rowsDestination, ok := destination.(*entsql.Rows)
	if !ok {
		return fmt.Errorf("D1: invalid Query destination %T; want *sql.Rows", destination)
	}
	statement := Statement{SQL: query, Params: arguments}
	var result Result
	if isReadOnly(query) {
		result, err = d.Read(ctx, statement)
	} else {
		result, err = d.write(ctx, statement)
	}
	if err != nil {
		return err
	}
	*rowsDestination = entsql.Rows{ColumnScanner: newBufferedRows(result)}
	return nil
}

type writeResult struct {
	lastInsertID int64
	rowsAffected int64
}

func (r writeResult) LastInsertId() (int64, error) { return r.lastInsertID, nil }
func (r writeResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

func argumentsOf(value any) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	arguments, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("D1: invalid argument type %T; want []any", value)
	}
	return append([]any(nil), arguments...), nil
}

func isReadOnly(query string) bool {
	switch sqlKeyword(query) {
	case "SELECT", "PRAGMA", "EXPLAIN":
		return true
	default:
		return false
	}
}

func sqlKeyword(query string) string {
	query = strings.TrimSpace(query)
	for strings.HasPrefix(query, "--") || strings.HasPrefix(query, "/*") {
		if strings.HasPrefix(query, "--") {
			if newline := strings.IndexByte(query, '\n'); newline >= 0 {
				query = strings.TrimSpace(query[newline+1:])
				continue
			}
			return ""
		}
		end := strings.Index(query[2:], "*/")
		if end < 0 {
			return ""
		}
		query = strings.TrimSpace(query[end+4:])
	}
	if query == "" {
		return ""
	}
	if index := strings.IndexAny(query, " \t\r\n("); index >= 0 {
		query = query[:index]
	}
	return strings.ToUpper(query)
}

// IsConstraintError reports whether err represents a SQLite constraint error.
func IsConstraintError(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 19
}

var _ dialect.Driver = (*Driver)(nil)
var _ sql.Result = writeResult{}
