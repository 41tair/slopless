//go:build integration

package data_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/41tair/slopless/pkg/config"
	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/d1/d1test"
	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
	"github.com/41tair/slopless/pkg/service"
)

type d1TestDatabase = d1test.Database

func newD1TestDatabase(t *testing.T) d1TestDatabase {
	t.Helper()
	return d1test.New(t)
}

func openD1Service(t *testing.T, database d1TestDatabase) (*data.Store, *service.Service) {
	t.Helper()
	store, err := data.Open(
		t.Context(), database.AccountID, database.ID, database.Token,
	)
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

func createD1Identity(t *testing.T, svc *service.Service, name, invitation string) model.Identity {
	t.Helper()
	user, err := svc.Register(t.Context(), model.Registration{
		Username: name, Password: "test-password", InvitationCode: invitation,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := svc.CreateToken(t.Context(), model.TokenInput{UserID: user.ID, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return model.Identity{UserID: user.ID, TokenID: token.ID}
}

func queryD1Strings(t *testing.T, database d1TestDatabase, query string, args ...any) []string {
	t.Helper()
	driver, err := d1.New(database.AccountID, database.ID, database.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	rows := &entsql.Rows{}
	if err := driver.Query(t.Context(), query, args, rows); err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func credentialColumns(t *testing.T, database d1TestDatabase) []string {
	t.Helper()
	driver, err := d1.New(database.AccountID, database.ID, database.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	rows := &entsql.Rows{}
	if err := driver.Query(t.Context(), "PRAGMA table_info(credentials)", []any{}, rows); err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&id, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func execD1(t *testing.T, database d1TestDatabase, query string, args ...any) {
	t.Helper()
	driver, err := d1.New(database.AccountID, database.ID, database.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	if err := driver.Exec(t.Context(), query, args, nil); err != nil {
		t.Fatal(err)
	}
}

func TestD1InitializesSchemaAndPreservesDataAcrossReopen(t *testing.T) {
	database := newD1TestDatabase(t)
	store, err := data.Open(
		t.Context(), database.AccountID, database.ID, database.Token,
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(store, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	user, err := svc.CreateUser(t.Context(), "persistent", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, secret, err := svc.CreateToken(t.Context(), model.TokenInput{UserID: user.ID, Name: "persistent-token"})
	if err != nil {
		t.Fatal(err)
	}
	target := model.Account{Platform: "github", Username: "persistent-target"}
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

	wantTables := []string{
		"accounts", "confirmations", "credentials", "credit_entries", "idempotencies",
		"invitations", "marks", "rate_events", "users",
	}
	tables := queryD1Strings(t, database,
		"SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
	)
	for _, table := range wantTables {
		if !slices.Contains(tables, table) {
			t.Fatalf("D1 tables = %v, missing %q", tables, table)
		}
	}
	if columns := credentialColumns(t, database); slices.Contains(columns, "scopes") {
		t.Fatalf("fresh credentials columns retained scopes: %v", columns)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	execD1(t, database, "ALTER TABLE credentials ADD COLUMN legacy_note TEXT NOT NULL DEFAULT ''")
	execD1(t, database, "UPDATE credentials SET legacy_note = ? WHERE id = ?", "preserve-me", token.ID)

	reopened, err := data.Open(
		t.Context(), database.AccountID, database.ID, database.Token,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedService, err := service.New(reopened, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reopenedService.AuthenticateAPI(t.Context(), secret)
	if err != nil || identity.UserID != user.ID || identity.TokenID != token.ID {
		t.Fatalf("token authentication after reopen = %+v: %v", identity, err)
	}
	tokens, err := reopened.Tokens(t.Context(), user.ID)
	if err != nil || len(tokens) != 1 || tokens[0].ID != token.ID || tokens[0].Name != "persistent-token" {
		t.Fatalf("tokens after reopen = %+v: %v", tokens, err)
	}
	states, err := reopened.Snapshot(t.Context(), user.ID, []model.Account{target})
	if err != nil || len(states) != 1 || !states[0].Marked || !states[0].MarkedByMe || states[0].MarkCount != 1 {
		t.Fatalf("mark after reopen = %+v: %v", states, err)
	}
	credits, err := reopened.Credits(t.Context(), user.ID, model.Page{Limit: 10})
	if err != nil || credits.Balance != 7 || len(credits.Entries) != 1 || credits.Entries[0].Amount != -3 {
		t.Fatalf("credits after reopen = %+v: %v", credits, err)
	}
	login, err := reopenedService.Login(t.Context(), "persistent", "test-password")
	if err != nil || login.User.ID != user.ID {
		t.Fatalf("login after reopen = %+v: %v", login, err)
	}
	if notes := queryD1Strings(t, database, "SELECT legacy_note FROM credentials WHERE id = ?", token.ID); len(notes) != 1 || notes[0] != "preserve-me" {
		t.Fatalf("legacy note after reopen = %v", notes)
	}
}

func TestD1ConcurrentOpenInitializesOnce(t *testing.T) {
	database := newD1TestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var group sync.WaitGroup
	for i := range workers {
		group.Go(func() {
			<-start
			store, err := data.Open(
				ctx, database.AccountID, database.ID, database.Token,
			)
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
	store, err := data.Open(
		t.Context(), database.AccountID, database.ID, database.Token,
	)
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

func TestD1ConcurrentTransactionsAwardCreditsOnce(t *testing.T) {
	database := newD1TestDatabase(t)
	_, first := openD1Service(t, database)
	_, second := openD1Service(t, database)
	invites, err := first.BootstrapInvitations(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	marker := createD1Identity(t, first, "marker", invites[0].Code)
	confirmer := createD1Identity(t, first, "confirmer", invites[1].Code)
	target := []model.Account{{Platform: "github", Username: "target"}}
	if _, _, err := first.Mark(t.Context(), marker, model.MarkInput{Accounts: target, Marked: true, Key: "d1-first"}); err != nil {
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
				Accounts: target, Marked: true, Key: fmt.Sprintf("d1-confirm-%d", i),
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

func TestD1InvitationIsSingleUseAcrossStores(t *testing.T) {
	database := newD1TestDatabase(t)
	_, first := openD1Service(t, database)
	_, second := openD1Service(t, database)
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

func TestD1TransactionRollback(t *testing.T) {
	database := newD1TestDatabase(t)
	store, _ := openD1Service(t, database)
	wantErr := errors.New("roll back fixture")
	err := store.WithTx(t.Context(), func(tx *data.Store) error {
		if _, err := tx.CreateUser(t.Context(), data.UserRecord{
			User: model.User{Username: "rolled-back"}, PasswordHash: "fixture",
		}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTx error = %v, want %v", err, wantErr)
	}
	if _, err := store.UserByName(t.Context(), "rolled-back"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("rolled-back user lookup error = %v", err)
	}
}

func TestD1MapsConstraintErrors(t *testing.T) {
	database := newD1TestDatabase(t)
	store, _ := openD1Service(t, database)
	input := data.UserRecord{User: model.User{Username: "duplicate"}, PasswordHash: "fixture"}
	if _, err := store.CreateUser(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(t.Context(), input); !errors.Is(err, data.ErrConflict) {
		t.Fatalf("duplicate username error = %v", err)
	}
}
