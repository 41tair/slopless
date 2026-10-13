package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/schema"
	"github.com/41tair/slopless/pkg/model"
)

var ErrConflict = errors.New("database constraint conflict")

type Store struct {
	client *ent.Client
	driver *d1.Driver
	plan   *mutationPlan
}

// Open connects to the D1 management REST API. Apply release migrations before
// opening a store; application startup only checks the schema.
func Open(ctx context.Context, accountID, databaseID, token string) (*Store, error) {
	driver, err := d1.New(accountID, databaseID, token)
	if err != nil {
		return nil, err
	}
	return openDriver(ctx, driver)
}

// OpenBinding uses the private D1 binding adapter supplied by the Container's
// outbound Worker. The application does not need a Cloudflare API token.
func OpenBinding(ctx context.Context) (*Store, error) {
	driver, err := d1.NewBinding()
	if err != nil {
		return nil, err
	}
	return openDriver(ctx, driver)
}

func openDriver(ctx context.Context, driver *d1.Driver) (*Store, error) {
	if err := schema.Check(ctx, driver); err != nil {
		_ = driver.Close()
		return nil, fmt.Errorf("check D1 schema: %w", databaseError(err))
	}
	return &Store{client: ent.NewClient(ent.Driver(driver)), driver: driver}, nil
}

func (s *Store) Close() error { return s.client.Close() }

// WithTx plans changes using targeted reads and atomically commits them with a
// revision check. It does not open an interactive SQL transaction. A concurrent
// writer reruns fn with a fresh plan; fn must reset outputs and have no external
// side effects. Every application-table writer must advance the same revision.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.plan != nil {
		return errors.New("nested D1 mutation plans are unsupported")
	}
	for attempt := range 32 {
		version, err := s.driver.Version(ctx)
		if err != nil {
			return databaseError(err)
		}
		plan := &mutationPlan{base: s, now: time.Now().UTC()}
		tx := &Store{client: ent.NewClient(ent.Driver(readOnlyDriver{s.driver})), driver: s.driver, plan: plan}
		callbackErr := fn(tx)
		if errors.Is(callbackErr, model.ErrUnavailable) || errors.Is(callbackErr, context.Canceled) || errors.Is(callbackErr, context.DeadlineExceeded) {
			return databaseError(callbackErr)
		}
		var mutations []d1.Statement
		if callbackErr == nil {
			mutations, err = plan.statements()
			if err != nil {
				return err
			}
		}
		// Even a business rejection must observe one stable revision. Otherwise
		// a mixed set of reads can incorrectly reject a concurrent operation.
		if callbackErr != nil || len(mutations) == 0 {
			current, readErr := s.driver.Version(ctx)
			if readErr != nil {
				return databaseError(readErr)
			}
			if current == version {
				return callbackErr
			}
		} else {
			err = s.driver.Commit(ctx, version, mutations)
			if !errors.Is(err, d1.ErrConflict) {
				return databaseError(err)
			}
		}
		if err := waitConflict(ctx, attempt); err != nil {
			return databaseError(err)
		}
	}
	return errors.Join(model.ErrUnavailable, d1.ErrConflict)
}

// The gate prevents an overlooked Ent mutation from escaping the plan before
// its final batch. Ent supplies targeted reads and entity scanning.
type readOnlyDriver struct{ driver *d1.Driver }

func (d readOnlyDriver) Dialect() string { return d.driver.Dialect() }
func (readOnlyDriver) Close() error      { return nil }
func (readOnlyDriver) Tx(context.Context) (dialect.Tx, error) {
	return nil, d1.ErrTransactionsUnsupported
}
func (readOnlyDriver) Exec(context.Context, string, any, any) error {
	return errors.New("Ent writes must use the D1 mutation plan")
}
func (d readOnlyDriver) Query(ctx context.Context, query string, args, destination any) error {
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SELECT ") {
		return errors.New("only Ent SELECT queries are allowed while planning D1 mutations")
	}
	return d.driver.Query(ctx, query, args, destination)
}

func waitConflict(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(min(1<<attempt, 100)) * time.Millisecond)
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
	if ent.IsConstraintError(err) || d1.IsConstraintError(err) {
		return errors.Join(ErrConflict, err)
	}
	if errors.Is(err, d1.ErrUnavailable) || errors.Is(err, d1.ErrConflict) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.Join(model.ErrUnavailable, err)
	}
	return err
}

func (s *Store) Cleanup(ctx context.Context) error {
	return s.WithTx(ctx, func(tx *Store) error {
		for _, table := range []string{"credentials", "idempotencies", "rate_events"} {
			tx.plan.extra = append(tx.plan.extra, d1.Statement{SQL: `DELETE FROM "` + table + `" WHERE "expires_at" <= ?`, Params: []any{tx.plan.now}})
		}
		return nil
	})
}
