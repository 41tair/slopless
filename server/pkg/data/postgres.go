package data

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/41tair/slopless/pkg/data/ent"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// All instances use the same database-scoped lock. Serializing application
// transactions preserves the invitation, credit, idempotency, and rate-budget
// consistency required by this small community.
const postgresTransactionLock int64 = 0x736c6f706c657373

type postgresDriver struct{ *entsql.Driver }

func (d *postgresDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	return d.BeginTx(ctx, nil)
}

func (d *postgresDriver) BeginTx(ctx context.Context, opts *sql.TxOptions) (dialect.Tx, error) {
	tx, err := d.Driver.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", []any{postgresTransactionLock}, nil); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func openPostgres(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL database: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	store, err := initializePostgres(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func initializePostgres(ctx context.Context, db *sql.DB) (*Store, error) {
	// A dedicated transaction holds the advisory lock while migrations use the
	// remaining pool connections. Rollback always releases the lock, including on
	// cancellation; no session-level lock can leak into a reused connection.
	lock, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin PostgreSQL initialization: %w", err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", postgresTransactionLock); err != nil {
		return nil, fmt.Errorf("lock database initialization: %w", err)
	}
	driver := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(driver))
	if err := initialize(ctx, client); err != nil {
		return nil, err
	}
	if _, err := lock.ExecContext(ctx, "ALTER TABLE IF EXISTS credentials DROP COLUMN IF EXISTS scopes"); err != nil {
		return nil, fmt.Errorf("remove legacy credential scopes: %w", err)
	}
	if err := lock.Commit(); err != nil {
		return nil, fmt.Errorf("commit PostgreSQL initialization: %w", err)
	}
	return &Store{client: ent.NewClient(ent.Driver(&postgresDriver{driver}))}, nil
}
