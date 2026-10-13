//go:build integration

package data_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/41tair/slopless/pkg/config"
	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
	"github.com/41tair/slopless/pkg/service"
	"github.com/google/uuid"
)

func postgresTestDSN(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_POSTGRES_DSN")
	if raw == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	dsn, err := url.Parse(raw)
	if err != nil || (dsn.Scheme != "postgres" && dsn.Scheme != "postgresql") {
		t.Fatalf("TEST_POSTGRES_DSN must be a postgres:// or postgresql:// URL: %v", err)
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	// UUID hex digits make this a safe SQL identifier.
	schema := "slopless_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close PostgreSQL administration connection: %v", err)
		}
	})
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func postgresService(t *testing.T, dsn string) (*data.Store, *service.Service) {
	t.Helper()
	store, err := data.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := service.New(store, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	return store, svc
}

func postgresIdentity(t *testing.T, svc *service.Service, name, invitation string) model.Identity {
	t.Helper()
	u, err := svc.Register(t.Context(), model.Registration{
		Username: name, Password: "test-password", InvitationCode: invitation,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := svc.CreateToken(t.Context(), model.TokenInput{
		UserID: u.ID, Name: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.Identity{UserID: u.ID, TokenID: token.ID}
}

func postgresColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1
			  AND column_name = $2
		)
	`, table, column).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestPostgresRemovesOnlyLegacyCredentialScopes(t *testing.T) {
	dsn := postgresTestDSN(t)
	store, err := data.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := service.New(store, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	user, err := svc.CreateUser(t.Context(), "legacy-user", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, secret, err := svc.CreateToken(t.Context(), model.TokenInput{UserID: user.ID, Name: "legacy-reader"})
	if err != nil {
		t.Fatal(err)
	}
	target := model.Account{Platform: "github", Username: "legacy-target"}
	if err := store.CreateMark(t.Context(), user.ID, target); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBalance(t.Context(), user.ID, 7); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateCreditEntry(t.Context(), data.CreditRecord{
		UserID: user.ID, Amount: -3, BalanceAfter: 7, Reason: "invitation_purchase",
	}); err != nil {
		t.Fatal(err)
	}

	inspector, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspector.Close() })
	if postgresColumnExists(t, inspector, "credentials", "scopes") {
		t.Fatal("fresh credentials table retained scopes")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := inspector.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(t.Context(), `
		ALTER TABLE credentials
		ADD COLUMN scopes jsonb NOT NULL DEFAULT '[]'::jsonb,
		ADD COLUMN legacy_note text NOT NULL DEFAULT ''
	`); err != nil {
		_ = legacy.Rollback()
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(t.Context(),
		"UPDATE credentials SET scopes = $1::jsonb, legacy_note = $2 WHERE id = $3",
		`["accounts:read"]`, "preserve-me", token.ID,
	); err != nil {
		_ = legacy.Rollback()
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(t.Context(),
		"ALTER TABLE credentials ALTER COLUMN scopes DROP DEFAULT",
	); err != nil {
		_ = legacy.Rollback()
		t.Fatal(err)
	}
	if err := legacy.Commit(); err != nil {
		t.Fatal(err)
	}
	var dataType, nullable string
	if err := inspector.QueryRowContext(t.Context(), `
		SELECT data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'credentials'
		  AND column_name = 'scopes'
	`).Scan(&dataType, &nullable); err != nil {
		t.Fatal(err)
	}
	if dataType != "jsonb" || nullable != "NO" {
		t.Fatalf("legacy scopes type = %s nullable = %s", dataType, nullable)
	}

	upgraded, err := data.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	if postgresColumnExists(t, inspector, "credentials", "scopes") {
		t.Fatal("legacy scopes column survived upgrade")
	}
	if !postgresColumnExists(t, inspector, "credentials", "legacy_note") {
		t.Fatal("upgrade removed an unrelated legacy column")
	}
	var note string
	if err := inspector.QueryRowContext(t.Context(),
		"SELECT legacy_note FROM credentials WHERE id = $1", token.ID,
	).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "preserve-me" {
		t.Fatalf("legacy note = %q", note)
	}
	upgradedService, err := service.New(upgraded, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := upgradedService.AuthenticateAPI(t.Context(), secret)
	if err != nil || identity.UserID != user.ID || identity.TokenID != token.ID {
		t.Fatalf("legacy token authentication = %+v: %v", identity, err)
	}
	tokens, err := upgraded.Tokens(t.Context(), user.ID)
	if err != nil || len(tokens) != 1 || tokens[0].ID != token.ID || tokens[0].Name != "legacy-reader" {
		t.Fatalf("legacy tokens = %+v: %v", tokens, err)
	}
	states, err := upgraded.Snapshot(t.Context(), user.ID, []model.Account{target})
	if err != nil || len(states) != 1 || !states[0].Marked || !states[0].MarkedByMe || states[0].MarkCount != 1 {
		t.Fatalf("legacy mark = %+v: %v", states, err)
	}
	credits, err := upgraded.Credits(t.Context(), user.ID, model.Page{Limit: 10})
	if err != nil || credits.Balance != 7 || len(credits.Entries) != 1 || credits.Entries[0].Amount != -3 {
		t.Fatalf("legacy credits = %+v: %v", credits, err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := data.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if postgresColumnExists(t, inspector, "credentials", "scopes") {
		t.Fatal("scopes column returned after reopen")
	}
	reopenedService, err := service.New(reopened, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	identity, err = reopenedService.AuthenticateAPI(t.Context(), secret)
	if err != nil || identity.UserID != user.ID || identity.TokenID != token.ID {
		t.Fatalf("legacy token authentication after reopen = %+v: %v", identity, err)
	}
	if err := inspector.QueryRowContext(t.Context(),
		"SELECT legacy_note FROM credentials WHERE id = $1", token.ID,
	).Scan(&note); err != nil || note != "preserve-me" {
		t.Fatalf("legacy note after reopen = %q: %v", note, err)
	}
}

func TestPostgresConcurrentOpenMigratesOnce(t *testing.T) {
	dsn := postgresTestDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var group sync.WaitGroup
	for i := range workers {
		group.Go(func() {
			<-start
			store, err := data.Open(ctx, dsn)
			if err == nil {
				_, err = store.CreateUser(ctx, data.UserRecord{
					User: model.User{Username: fmt.Sprintf("worker%d", i)}, PasswordHash: "fixture",
				})
				err = errors.Join(err, store.Close())
			}
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
	store, err := data.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := range workers {
		if _, err := store.UserByName(t.Context(), fmt.Sprintf("worker%d", i)); err != nil {
			t.Fatalf("worker %d was not persisted: %v", i, err)
		}
	}
}

func TestPostgresConcurrentTransactionsAwardCreditsOnce(t *testing.T) {
	dsn := postgresTestDSN(t)
	_, first := postgresService(t, dsn)
	_, second := postgresService(t, dsn)
	invites, err := first.BootstrapInvitations(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	marker := postgresIdentity(t, first, "marker", invites[0].Code)
	confirmer := postgresIdentity(t, first, "confirmer", invites[1].Code)
	target := []model.Account{{Platform: "github", Username: "target"}}
	if _, _, err := first.Mark(t.Context(), marker, model.MarkInput{Accounts: target, Marked: true, Key: "postgres-first"}); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	results := make(chan error, workers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range workers {
		group.Go(func() {
			<-start
			svc := first
			if i%2 != 0 {
				svc = second
			}
			_, _, err := svc.Mark(t.Context(), confirmer, model.MarkInput{
				Accounts: target, Marked: true, Key: fmt.Sprintf("postgres-confirm-%d", i),
			})
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	credits, err := first.Credits(t.Context(), marker.UserID, model.Page{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if credits.Balance != 10 || len(credits.Entries) != 1 || credits.Entries[0].Reason != "first_mark" {
		t.Fatalf("concurrent confirmations produced %+v", credits)
	}
	states, err := second.Query(t.Context(), confirmer, target)
	if err != nil || len(states) != 1 || states[0].MarkCount != 2 {
		t.Fatalf("persisted state = %+v: %v", states, err)
	}
}

func TestPostgresInvitationIsSingleUseAcrossStores(t *testing.T) {
	dsn := postgresTestDSN(t)
	_, first := postgresService(t, dsn)
	_, second := postgresService(t, dsn)
	invites, err := first.BootstrapInvitations(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 6
	results := make(chan error, workers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range workers {
		group.Go(func() {
			<-start
			svc := first
			if i%2 != 0 {
				svc = second
			}
			_, err := svc.Register(t.Context(), model.Registration{
				Username: fmt.Sprintf("invitee%d", i), Password: "test-password", InvitationCode: invites[0].Code,
			})
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, model.ErrInvalidInvitation) {
			t.Fatalf("registration error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("invitation was consumed %d times", successes)
	}
}

func TestPostgresTransactionLockCancellationAndRelease(t *testing.T) {
	dsn := postgresTestDSN(t)
	first, _ := postgresService(t, dsn)
	second, _ := postgresService(t, dsn)
	acquired := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	holderCtx, cancelHolder := context.WithCancel(t.Context())
	defer cancelHolder()
	go func() {
		holderDone <- first.WithTx(holderCtx, func(*data.Store) error {
			close(acquired)
			select {
			case <-release:
				return nil
			case <-holderCtx.Done():
				return holderCtx.Err()
			}
		})
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		cancelHolder()
		t.Fatal("first transaction did not acquire the serialization lock")
	}

	waitCtx, cancelWait := context.WithTimeout(t.Context(), 150*time.Millisecond)
	err := second.WithTx(waitCtx, func(*data.Store) error { return nil })
	cancelWait()
	close(release)
	holderErr := <-holderDone
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock waiter ignored cancellation: %v", err)
	}
	if holderErr != nil {
		t.Fatalf("release holder: %v", holderErr)
	}
	if err := second.WithTx(t.Context(), func(tx *data.Store) error {
		_, err := tx.CreateUser(t.Context(), data.UserRecord{
			User: model.User{Username: "after-release"}, PasswordHash: "fixture",
		})
		return err
	}); err != nil {
		t.Fatalf("transaction after lock release: %v", err)
	}
}

func TestPostgresPersistsAcrossReopen(t *testing.T) {
	dsn := postgresTestDSN(t)
	store, err := data.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(store, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	invites, err := svc.BootstrapInvitations(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.Register(t.Context(), model.Registration{
		Username: "persistent", Password: "test-password", InvitationCode: invites[0].Code,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := data.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedService, err := service.New(reopened, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	login, err := reopenedService.Login(t.Context(), "persistent", "test-password")
	if err != nil || login.User.ID != u.ID {
		t.Fatalf("login after reopen = %+v: %v", login, err)
	}
}

func TestPostgresMapsConstraintErrors(t *testing.T) {
	dsn := postgresTestDSN(t)
	store, _ := postgresService(t, dsn)
	input := data.UserRecord{User: model.User{Username: "duplicate"}, PasswordHash: "fixture"}
	if _, err := store.CreateUser(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(t.Context(), input); !errors.Is(err, data.ErrConflict) {
		t.Fatalf("duplicate username error = %v", err)
	}
}
