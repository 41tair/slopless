// Package d1 implements an Ent driver backed by the Cloudflare D1 REST API.
package d1

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
)

const automaticAttempts = 16

// Driver is an Ent SQLite dialect driver whose persistent state lives in D1.
type Driver struct {
	client      *client
	mu          sync.RWMutex
	closed      bool
	foreignKeys bool
}

// New constructs a D1 Ent driver. It validates configuration without making a
// network request.
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
	client, err := newClient(endpoint, token)
	if err != nil {
		return nil, err
	}
	return &Driver{client: client, foreignKeys: true}, nil
}

// Dialect reports the SQLite dialect used by D1 and the local transaction
// engine.
func (*Driver) Dialect() string { return dialect.SQLite }

// Close prevents new operations from starting. Transactions already returned
// to callers retain ownership of their local resources and may be completed.
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

func (d *Driver) settings() (bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return false, fmt.Errorf("d1 driver is closed")
	}
	return d.foreignKeys, nil
}

// Tx starts a real SQLite transaction over an isolated D1 snapshot.
func (d *Driver) Tx(ctx context.Context) (dialect.Tx, error) {
	foreignKeys, err := d.settings()
	if err != nil {
		return nil, err
	}
	state, err := d.client.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	local, err := restoreSnapshot(ctx, state, foreignKeys)
	if err != nil {
		return nil, err
	}
	tx, err := local.db.BeginTx(ctx, nil)
	if err != nil {
		_ = local.db.Close()
		return nil, fmt.Errorf("begin local transaction: %w", err)
	}
	return &transaction{driver: d, ctx: ctx, local: local, tx: tx}, nil
}

// Exec executes one statement as an automatically committed transaction.
func (d *Driver) Exec(ctx context.Context, query string, args, destination any) error {
	arguments, err := argumentsOf(args)
	if err != nil {
		return err
	}
	if destination != nil {
		if _, ok := destination.(*sql.Result); !ok {
			return fmt.Errorf("D1: invalid Exec destination %T; want *sql.Result", destination)
		}
	}
	if enabled, ok := foreignKeyPragma(query, arguments); ok {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return fmt.Errorf("d1 driver is closed")
		}
		d.foreignKeys = enabled
		d.mu.Unlock()
		if destination != nil {
			result := sql.Result(driver.RowsAffected(0))
			*destination.(*sql.Result) = result
		}
		return nil
	}
	for attempt := range automaticAttempts {
		if err := d.checkOpen(); err != nil {
			return err
		}
		result, err := d.execAttempt(ctx, query, arguments)
		if err == nil {
			if destination != nil {
				*destination.(*sql.Result) = result
			}
			return nil
		}
		if !errors.Is(err, ErrConflict) || attempt+1 == automaticAttempts {
			return err
		}
		if err := waitForRetry(ctx, attempt); err != nil {
			return err
		}
	}
	panic("unreachable")
}

func (d *Driver) execAttempt(ctx context.Context, query string, args []any) (sql.Result, error) {
	tx, err := d.newAutomaticTransaction(ctx)
	if err != nil {
		return nil, err
	}
	result, err := tx.tx.ExecContext(ctx, query, args...)
	if err != nil {
		err = classifyStatementError(query, err)
		return nil, errors.Join(err, tx.rollback())
	}
	if isDDL(query) {
		tx.ddl = append(tx.ddl, query)
	}
	if err := tx.commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// Query executes one statement as an automatically committed transaction and
// buffers all rows before releasing the local snapshot.
func (d *Driver) Query(ctx context.Context, query string, args, destination any) error {
	arguments, err := argumentsOf(args)
	if err != nil {
		return err
	}
	rowsDestination, ok := destination.(*entsql.Rows)
	if !ok {
		return fmt.Errorf("D1: invalid Query destination %T; want *sql.Rows", destination)
	}
	if isReadOnly(query) {
		rows, err := d.readAttempt(ctx, query, arguments)
		if err != nil {
			return err
		}
		*rowsDestination = entsql.Rows{ColumnScanner: rows}
		return nil
	}
	for attempt := range automaticAttempts {
		if err := d.checkOpen(); err != nil {
			return err
		}
		rows, err := d.queryAttempt(ctx, query, arguments)
		if err == nil {
			*rowsDestination = entsql.Rows{ColumnScanner: rows}
			return nil
		}
		if !errors.Is(err, ErrConflict) || attempt+1 == automaticAttempts {
			return err
		}
		if err := waitForRetry(ctx, attempt); err != nil {
			return err
		}
	}
	panic("unreachable")
}

func (d *Driver) readAttempt(ctx context.Context, query string, args []any) (*sql.Rows, error) {
	foreignKeys, err := d.settings()
	if err != nil {
		return nil, err
	}
	state, err := d.client.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	local, err := restoreSnapshot(ctx, state, foreignKeys)
	if err != nil {
		return nil, err
	}
	rows, err := local.db.QueryContext(ctx, query, args...)
	if err != nil {
		_ = local.db.Close()
		return nil, err
	}
	// database/sql keeps the Rows connection alive after Close and releases it
	// when Rows is closed. Returning native rows is required by Atlas migration.
	if err := local.db.Close(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("close local read database: %w", err)
	}
	return rows, nil
}

func (d *Driver) queryAttempt(ctx context.Context, query string, args []any) (*bufferedRows, error) {
	tx, err := d.newAutomaticTransaction(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		err = classifyStatementError(query, err)
		return nil, errors.Join(err, tx.rollback())
	}
	buffered, err := bufferRows(rows)
	if err != nil {
		return nil, errors.Join(err, tx.rollback())
	}
	if isDDL(query) {
		tx.ddl = append(tx.ddl, query)
	}
	if err := tx.commit(); err != nil {
		return nil, err
	}
	return buffered, nil
}

func (d *Driver) newAutomaticTransaction(ctx context.Context) (*transaction, error) {
	foreignKeys, err := d.settings()
	if err != nil {
		return nil, err
	}
	state, err := d.client.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	local, err := restoreSnapshot(ctx, state, foreignKeys)
	if err != nil {
		return nil, err
	}
	tx, err := local.db.BeginTx(ctx, nil)
	if err != nil {
		_ = local.db.Close()
		return nil, fmt.Errorf("begin local transaction: %w", err)
	}
	return &transaction{driver: d, ctx: ctx, local: local, tx: tx}, nil
}

type transaction struct {
	driver *Driver
	ctx    context.Context
	local  *localDatabase
	tx     *sql.Tx
	ddl    []string

	mu   sync.Mutex
	done bool
}

func (t *transaction) Exec(ctx context.Context, query string, args, destination any) error {
	arguments, err := argumentsOf(args)
	if err != nil {
		return err
	}
	if err := t.active(); err != nil {
		return err
	}
	switch destination := destination.(type) {
	case nil:
		_, err = t.tx.ExecContext(ctx, query, arguments...)
	case *sql.Result:
		var result sql.Result
		result, err = t.tx.ExecContext(ctx, query, arguments...)
		if err == nil {
			*destination = result
		}
	default:
		return fmt.Errorf("D1: invalid Exec destination %T; want *sql.Result", destination)
	}
	if err == nil && isDDL(query) {
		t.ddl = append(t.ddl, query)
	}
	return t.classifyError(query, err)
}

func (t *transaction) Query(ctx context.Context, query string, args, destination any) error {
	arguments, err := argumentsOf(args)
	if err != nil {
		return err
	}
	if err := t.active(); err != nil {
		return err
	}
	rowsDestination, ok := destination.(*entsql.Rows)
	if !ok {
		return fmt.Errorf("D1: invalid Query destination %T; want *sql.Rows", destination)
	}
	rows, err := t.tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return t.classifyError(query, err)
	}
	*rowsDestination = entsql.Rows{ColumnScanner: rows}
	if isDDL(query) {
		t.ddl = append(t.ddl, query)
	}
	return nil
}

func (t *transaction) active() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return fmt.Errorf("d1 transaction is already closed")
	}
	return nil
}

// Commit validates the native SQLite transaction and atomically publishes its
// row diff to D1.
func (t *transaction) Commit() error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return fmt.Errorf("d1 transaction is already closed")
	}
	t.done = true
	t.mu.Unlock()
	return t.commit()
}

func (t *transaction) commit() error {
	after, sequences, err := inspectDatabase(t.ctx, t.tx)
	if err != nil {
		return errors.Join(err, t.rollbackResources())
	}
	changes, err := diffTables(t.local.before.Tables, after)
	if err != nil {
		return errors.Join(err, t.rollbackResources())
	}
	if len(t.ddl) > 0 {
		changes, err = fullRowsForDDL(changes, after, t.ddl)
		if err != nil {
			return errors.Join(err, t.rollbackResources())
		}
	}
	if err := t.tx.Commit(); err != nil {
		return errors.Join(fmt.Errorf("commit local transaction: %w", err), t.local.db.Close())
	}
	if err := t.local.db.Close(); err != nil {
		return fmt.Errorf("close local transaction database: %w", err)
	}
	if len(t.ddl) == 0 && len(changes) == 0 && sameSequences(t.local.before.Sequences, sequences) {
		return nil
	}
	request := commitRequest{
		Version:   t.local.before.Version,
		TxID:      uuid.NewString(),
		DDL:       append([]string(nil), t.ddl...),
		Changes:   changes,
		Sequences: sequences,
	}
	return t.driver.client.commit(t.ctx, request)
}

// Rollback discards the local transaction without changing D1.
func (t *transaction) Rollback() error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return fmt.Errorf("d1 transaction is already closed")
	}
	t.done = true
	t.mu.Unlock()
	return t.rollbackResources()
}

func (t *transaction) rollback() error {
	t.mu.Lock()
	t.done = true
	t.mu.Unlock()
	return t.rollbackResources()
}

func (t *transaction) rollbackResources() error {
	return errors.Join(t.tx.Rollback(), t.local.db.Close())
}

func argumentsOf(value any) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	arguments, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("D1: invalid argument type %T; want []any", value)
	}
	normalized := make([]any, len(arguments))
	for i, argument := range arguments {
		if timestamp, ok := argument.(time.Time); ok {
			normalized[i] = formatSQLiteTime(timestamp)
		} else {
			normalized[i] = argument
		}
	}
	return normalized, nil
}

func foreignKeyPragma(query string, arguments []any) (bool, bool) {
	if len(arguments) != 0 {
		return false, false
	}
	normalized := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(strings.ToLower(query))
	normalized = strings.TrimSuffix(normalized, ";")
	switch normalized {
	case "pragmaforeign_keys=on", "pragmaforeign_keys=1", "pragmaforeign_keys(on)", "pragmaforeign_keys(1)":
		return true, true
	case "pragmaforeign_keys=off", "pragmaforeign_keys=0", "pragmaforeign_keys(off)", "pragmaforeign_keys(0)":
		return false, true
	default:
		return false, false
	}
}

func isDDL(query string) bool {
	keyword := sqlKeyword(query)
	switch strings.ToUpper(keyword) {
	case "ALTER", "CREATE", "DROP", "REINDEX":
		return true
	default:
		return false
	}
}

func classifyStatementError(query string, err error) error {
	if err == nil || !isDDL(query) {
		return err
	}
	return classifyStaleSchemaError(err)
}

func (t *transaction) classifyError(query string, err error) error {
	if err == nil {
		return nil
	}
	if isDDL(query) || len(t.ddl) > 0 {
		return classifyStaleSchemaError(err)
	}
	return err
}

func classifyStaleSchemaError(err error) error {
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"already exists",
		"duplicate column name",
		"no such table",
		"no such index",
		"no such trigger",
		"no such column",
		"has no column named",
	} {
		if strings.Contains(message, fragment) {
			return errors.Join(ErrConflict, err)
		}
	}
	return err
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
	if query == "" {
		return ""
	}
	keyword := query
	if index := strings.IndexAny(keyword, " \t\r\n("); index >= 0 {
		keyword = keyword[:index]
	}
	return strings.ToUpper(keyword)
}

// IsConstraintError reports whether err is a SQLite constraint failure.
func IsConstraintError(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 19
}

var _ dialect.Driver = (*Driver)(nil)
var _ dialect.Tx = (*transaction)(nil)
