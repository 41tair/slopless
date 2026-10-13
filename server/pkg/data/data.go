package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/credential"
	"github.com/41tair/slopless/pkg/data/ent/idempotency"
	"github.com/41tair/slopless/pkg/data/ent/migrate"
	"github.com/41tair/slopless/pkg/data/ent/rateevent"
	"github.com/41tair/slopless/pkg/model"
	"modernc.org/sqlite"
)

var ErrConflict = errors.New("database constraint conflict")

type Store struct {
	client *ent.Client
}

// Open connects Ent to the D1 REST API and initializes the application schema.
func Open(ctx context.Context, accountID, databaseID, token string) (*Store, error) {
	driver, err := d1.New(accountID, databaseID, token)
	if err != nil {
		return nil, err
	}
	client := ent.NewClient(ent.Driver(driver))
	for attempt := range 16 {
		err = initialize(ctx, client)
		if err == nil {
			return &Store{client: client}, nil
		}
		if !errors.Is(err, d1.ErrConflict) {
			break
		}
		if pauseErr := waitConflict(ctx, attempt); pauseErr != nil {
			err = pauseErr
			break
		}
	}
	_ = client.Close()
	return nil, fmt.Errorf("initialize D1 database: %w", databaseError(err))
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

// WithTx runs application work on an isolated D1 snapshot and commits its changes
// atomically. On a version conflict, fn runs again with a fresh snapshot; fn must
// reset its outputs and perform no external side effects. Use tx for all work.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Store) error) error {
	for attempt := range 16 {
		tx, err := s.client.Tx(ctx)
		if err != nil {
			return databaseError(err)
		}
		if err := fn(&Store{client: tx.Client()}); err != nil {
			_ = tx.Rollback()
			return err
		}
		err = tx.Commit()
		_ = tx.Rollback()
		if !errors.Is(err, d1.ErrConflict) {
			return databaseError(err)
		}
		if err := waitConflict(ctx, attempt); err != nil {
			return databaseError(err)
		}
	}
	return errors.Join(model.ErrUnavailable, d1.ErrConflict)
}

func waitConflict(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(min(1<<attempt, 50)) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case 19: // SQLITE_CONSTRAINT, including extended constraint codes.
			return errors.Join(ErrConflict, err)
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED.
			return errors.Join(model.ErrUnavailable, err)
		}
	}
	if errors.Is(err, d1.ErrUnavailable) || errors.Is(err, d1.ErrConflict) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
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
