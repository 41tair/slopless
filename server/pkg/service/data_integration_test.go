//go:build integration

package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/41tair/slopless/pkg/config"
	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/d1/d1test"
	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/idempotency"
	"github.com/41tair/slopless/pkg/model"
)

type d1TestDatabase struct {
	accountID  string
	databaseID string
	token      string
}

func d1TestEndpoint(t *testing.T) d1TestDatabase {
	t.Helper()
	database := d1test.New(t)
	return d1TestDatabase{
		accountID: database.AccountID, databaseID: database.ID, token: database.Token,
	}
}

type testService struct {
	*Service
	client *ent.Client // Fixture access for fault injection and persisted-state assertions.
}

func (s *testService) Close() error { return errors.Join(s.client.Close(), s.store.Close()) }

func openFixtureClient(database d1TestDatabase) (*ent.Client, error) {
	driver, err := d1.New(database.accountID, database.databaseID, database.token)
	if err != nil {
		return nil, err
	}
	return ent.NewClient(ent.Driver(driver)), nil
}

func execFixtureSQL(t *testing.T, database d1TestDatabase, query string, args ...any) {
	t.Helper()
	driver, err := d1.New(database.accountID, database.databaseID, database.token)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	if err := driver.Exec(t.Context(), query, args, nil); err != nil {
		t.Fatal(err)
	}
}

func openTestService(ctx context.Context, database d1TestDatabase, credits config.Credits) (*testService, error) {
	store, err := data.Open(
		ctx, database.accountID, database.databaseID, database.token,
	)
	if err != nil {
		return nil, err
	}
	svc, err := New(store, credits)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	client, err := openFixtureClient(database)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &testService{Service: svc, client: client}, nil
}

func testStore(t *testing.T, database d1TestDatabase) *testService {
	t.Helper()
	s, err := openTestService(t.Context(), database, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAccountBudgets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, policy string
		limit        int
	}{
		{name: "minute", policy: "mark.user.accounts.minute", limit: 200},
		{name: "day", policy: "mark.user.accounts.day", limit: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := testStore(t, d1TestEndpoint(t))
			identity := testIdentity(t, s)
			_, err := s.client.RateEvent.Create().SetPolicy(tt.policy).SetSubject(strconv.Itoa(identity.UserID)).
				SetUnits(tt.limit).SetExpiresAt(time.Now().Add(time.Minute)).Save(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			input := model.MarkInput{Accounts: []model.Account{{Platform: "github", Username: "example"}}, Key: "account-budget-key", Marked: true}
			_, _, err = s.Mark(t.Context(), identity, input)
			var rate *model.RateLimitError
			if !errors.As(err, &rate) || rate.RateLimit.Policy != tt.policy {
				t.Fatalf("account budget not enforced: %v", err)
			}
			states, err := s.Query(t.Context(), identity, input.Accounts)
			if err != nil || states[0].Marked {
				t.Fatalf("rejected batch changed marks: %+v, %v", states, err)
			}
		})
	}
}

func TestExpiredIdempotencyKeyCanBeReused(t *testing.T) {
	t.Parallel()
	database := d1TestEndpoint(t)
	s := testStore(t, database)
	identity := testIdentity(t, s)
	input := model.MarkInput{Accounts: []model.Account{{Platform: "github", Username: "example"}}, Key: "expired-operation-key", Marked: true}
	if _, _, err := s.Mark(t.Context(), identity, input); err != nil {
		t.Fatal(err)
	}
	entry, err := s.client.Idempotency.Query().Where(idempotency.KeyEQ(input.Key)).Only(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(entry.ExpiresAt) < 24*time.Hour {
		t.Fatal("retention is shorter than 24 hours")
	}
	execFixtureSQL(
		t,
		database,
		`UPDATE "idempotencies" SET "expires_at" = ? WHERE "id" = ?`,
		time.Now().Add(-time.Hour),
		entry.ID,
	)
	input.Marked = false
	states, replayed, err := s.Mark(t.Context(), identity, input)
	if err != nil || replayed || states[0].Marked {
		t.Fatalf("expired key not replaced: %+v, %v, %v", states, replayed, err)
	}
}

func testIdentity(t *testing.T, s *testService) model.Identity {
	t.Helper()
	invites, err := s.BootstrapInvitations(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Register(t.Context(), model.Registration{
		Username: "alice", Password: "test-password", InvitationCode: invites[0].Code,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.CreateToken(t.Context(), model.TokenInput{
		UserID: u.ID, Name: "extension",
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.Identity{UserID: u.ID, TokenID: token.ID}
}

func TestConcurrentMarkAndPersistence(t *testing.T) {
	t.Parallel()
	database := d1TestEndpoint(t)
	first := testStore(t, database)
	second := testStore(t, database)
	identity := testIdentity(t, first)
	input := model.MarkInput{Accounts: []model.Account{{Platform: "github", Username: "example"}}, Marked: true, Key: "same-operation-key"}
	type result struct {
		states   []model.AccountState
		replayed bool
		err      error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			<-start
			store := first
			if i%2 != 0 {
				store = second
			}
			states, replayed, err := store.Mark(t.Context(), identity, input)
			results <- result{states: states, replayed: replayed, err: err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	committed := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.states) != 1 || result.states[0].MarkCount != 1 {
			t.Fatalf("bad concurrent result: %+v", result)
		}
		if !result.replayed {
			committed++
		}
	}
	if committed != 1 {
		t.Fatalf("committed %d times", committed)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := testStore(t, database)
	if _, err := reopened.Login(t.Context(), "alice", "test-password"); err != nil {
		t.Fatal(err)
	}
	withdrawal := input
	withdrawal.Key, withdrawal.Marked = "withdraw-operation", false
	if _, _, err := reopened.Mark(t.Context(), identity, withdrawal); err != nil {
		t.Fatal(err)
	}
	states, replayed, err := reopened.Mark(t.Context(), identity, input)
	if err != nil || !replayed || !states[0].MarkedByMe {
		t.Fatalf("lost persisted replay: %+v, %v, %v", states, replayed, err)
	}
	states, err = reopened.Query(t.Context(), identity, input.Accounts)
	if err != nil || states[0].Marked {
		t.Fatalf("replay changed current state: %+v, %v", states, err)
	}
}

func TestMarkRollsBackWholeBatch(t *testing.T) {
	t.Parallel()
	database := d1TestEndpoint(t)
	s := testStore(t, database)
	identity := testIdentity(t, s)
	execFixtureSQL(t, database, `CREATE TRIGGER reject_account
		BEFORE INSERT ON marks WHEN NEW.username = 'reject'
		BEGIN SELECT RAISE(ABORT, 'reject account'); END`)
	input := model.MarkInput{
		Accounts: []model.Account{{Platform: "github", Username: "accepted"}, {Platform: "github", Username: "reject"}},
		Marked:   true, Key: "rollback-operation",
	}
	if _, _, err := s.Mark(t.Context(), identity, input); err == nil {
		t.Fatal("expected database failure")
	}
	states, err := s.Query(t.Context(), identity, input.Accounts)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Marked {
			t.Fatalf("partial write: %+v", states)
		}
	}
	execFixtureSQL(t, database, "DROP TRIGGER reject_account")
	states, replayed, err := s.Mark(t.Context(), identity, input)
	if err != nil || replayed || states[0].MarkCount != 1 || states[1].MarkCount != 1 {
		t.Fatalf("failed request consumed idempotency key: %+v, %v, %v", states, replayed, err)
	}
}

func TestRateBudgetsSharedAndDurable(t *testing.T) {
	t.Parallel()
	database := d1TestEndpoint(t)
	s := testStore(t, database)
	identity := testIdentity(t, s)
	for range 10 {
		if err := s.AdmitRequests(t.Context(), "mark", identity); err != nil {
			t.Fatal(err)
		}
	}
	err := s.AdmitRequests(t.Context(), "mark", identity)
	var rate *model.RateLimitError
	if !errors.As(err, &rate) || rate.RateLimit.Policy != "mark.token.requests.minute" || rate.RetryAfterSeconds < 1 {
		t.Fatalf("missing token budget: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, database)
	err = s.AdmitRequests(t.Context(), "mark", identity)
	if !errors.As(err, &rate) {
		t.Fatalf("budget lost on restart: %v", err)
	}
	token, _, err := s.CreateToken(t.Context(), model.TokenInput{
		UserID: identity.UserID, Name: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	identity.TokenID = token.ID
	for range 10 {
		if err := s.AdmitRequests(t.Context(), "mark", identity); err != nil {
			t.Fatal(err)
		}
	}
	token, _, err = s.CreateToken(t.Context(), model.TokenInput{
		UserID: identity.UserID, Name: "third",
	})
	if err != nil {
		t.Fatal(err)
	}
	identity.TokenID = token.ID
	err = s.AdmitRequests(t.Context(), "mark", identity)
	if !errors.As(err, &rate) || rate.RateLimit.Policy != "mark.user.requests.minute" {
		t.Fatalf("missing shared user budget: %v", err)
	}
	execFixtureSQL(
		t,
		database,
		`UPDATE "rate_events" SET "expires_at" = ? WHERE "policy" LIKE ?`,
		time.Now().Add(-time.Minute),
		"%.minute",
	)
	if err := s.AdmitRequests(t.Context(), "mark", identity); err != nil {
		t.Fatalf("expired windows still block: %v", err)
	}
}

func TestRateRetryUsesAllBlockingPolicies(t *testing.T) {
	t.Parallel()
	database := d1TestEndpoint(t)
	s := testStore(t, database)
	now := time.Now()
	for i, seconds := range []int{10, 40} {
		_, err := s.client.RateEvent.Create().SetPolicy(fmt.Sprintf("policy-%d", i)).SetSubject("subject").
			SetUnits(1).SetExpiresAt(now.Add(time.Duration(seconds) * time.Second)).Save(t.Context())
		if err != nil {
			t.Fatal(err)
		}
	}
	err := s.store.WithTx(t.Context(), func(tx *data.Store) error {
		return admit(t.Context(), tx, []model.RatePolicy{
			{Policy: "policy-0", Subject: "subject", Limit: 1, Units: 1, WindowSeconds: 60, Unit: "requests"},
			{Policy: "policy-1", Subject: "subject", Limit: 1, Units: 1, WindowSeconds: 60, Unit: "requests"},
		})
	})
	var rate *model.RateLimitError
	if !errors.As(err, &rate) || rate.RateLimit.Policy != "policy-1" || rate.RetryAfterSeconds < 35 {
		t.Fatalf("wrong retry deadline: %v", err)
	}
	execFixtureSQL(t, database, `UPDATE "rate_events" SET "expires_at" = ?`, now.Add(-time.Minute))
	if err := s.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	count, err := s.client.RateEvent.Query().Count(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("expired budget not cleaned: %d, %v", count, err)
	}
}
