package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/credential"
	"github.com/41tair/slopless/pkg/data/ent/idempotency"
	"github.com/41tair/slopless/pkg/data/ent/migrate"
	"github.com/41tair/slopless/pkg/data/ent/rateevent"
	"github.com/41tair/slopless/pkg/model"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrConflict = errors.New("database constraint conflict")

type Store struct {
	client *ent.Client
}

// Open opens a PostgreSQL store from a postgres:// or postgresql:// URL.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, fmt.Errorf("data.dsn must be a postgres:// or postgresql:// URL")
	}
	return openPostgres(ctx, dsn)
}

func initialize(ctx context.Context, client *ent.Client) error {
	// Atlas mutates table/index metadata while planning migrations. Each store
	// needs its own copy so concurrent opens do not race on migrate.Tables.
	tables, err := entschema.CopyTables(migrate.Tables)
	if err != nil {
		return fmt.Errorf("copy schema: %w", err)
	}
	if err := migrate.Create(ctx, client.Schema, tables); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	if err := preserveLegacyAccounts(ctx, client); err != nil {
		return fmt.Errorf("preserve legacy account attribution: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.client.Close() }

// WithTx runs application work on an isolated transaction-bound store. The
// callback must use tx for every read and write; no transaction lives in context.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Store) error) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	if err := fn(&Store{client: tx.Client()}); err != nil {
		return err
	}
	return databaseError(tx.Commit())
}

func databaseError(err error) error {
	if err == nil {
		return nil
	}
	if ent.IsNotFound(err) {
		return errors.Join(model.ErrNotFound, err)
	}
	if ent.IsConstraintError(err) {
		return errors.Join(ErrConflict, err)
	}
	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) {
		if strings.HasPrefix(postgresErr.Code, "23") {
			return errors.Join(ErrConflict, err)
		}
		if strings.HasPrefix(postgresErr.Code, "08") || postgresErr.Code == "40001" ||
			postgresErr.Code == "40P01" || postgresErr.Code == "55P03" ||
			postgresErr.Code == "53300" || postgresErr.Code == "57P01" || postgresErr.Code == "57P02" || postgresErr.Code == "57P03" {
			return errors.Join(model.ErrUnavailable, err)
		}
	}
	if pgconn.SafeToRetry(err) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(model.ErrUnavailable, err)
	}
	return err
}

func (s *Store) Cleanup(ctx context.Context) error {
	return s.WithTx(ctx, func(tx *Store) error { return tx.cleanup(ctx) })
}

func (s *Store) cleanup(ctx context.Context) error {
	now := time.Now()
	if _, err := s.client.Credential.Delete().Where(credential.ExpiresAtLTE(now)).Exec(ctx); err != nil {
		return databaseError(err)
	}
	if _, err := s.client.Idempotency.Delete().Where(idempotency.ExpiresAtLTE(now)).Exec(ctx); err != nil {
		return databaseError(err)
	}
	_, err := s.client.RateEvent.Delete().Where(rateevent.ExpiresAtLTE(now)).Exec(ctx)
	return databaseError(err)
}
