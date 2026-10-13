//go:build integration

package service

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/41tair/slopless/pkg/config"
	"github.com/41tair/slopless/pkg/data/ent/account"
	"github.com/41tair/slopless/pkg/data/ent/confirmation"
	"github.com/41tair/slopless/pkg/data/ent/invitation"
	"github.com/41tair/slopless/pkg/data/ent/migrate"
	"github.com/41tair/slopless/pkg/model"
)

// Test-only sponsored invitations build a tree without unrelated earning work.
// The HTTP integration test separately exercises earning and paying every cost.
func creditUser(t *testing.T, s *testService, name string, inviterID int) model.Identity {
	t.Helper()
	create := s.client.Invitation.Create().SetCode(newSecret("sli_")).SetPurchaseKey("fixture-" + name)
	if inviterID != 0 {
		create.SetIssuerID(inviterID)
	}
	invite, err := create.Save(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Register(t.Context(), model.Registration{
		Username: name, Password: "test-password", InvitationCode: invite.Code,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.CreateToken(t.Context(), model.TokenInput{
		UserID: u.ID, Name: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.Identity{UserID: u.ID, TokenID: token.ID}
}

func creditMark(t *testing.T, s *testService, identity model.Identity, input model.MarkInput) {
	t.Helper()
	if _, _, err := s.Mark(t.Context(), identity, input); err != nil {
		t.Fatal(err)
	}
}

func balance(t *testing.T, s *testService, userID int, want int64) *model.Credits {
	t.Helper()
	credits, err := s.Credits(t.Context(), userID, model.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if credits.Balance != want {
		t.Fatalf("user %d balance = %d, want %d", userID, credits.Balance, want)
	}
	var total int64
	for _, entry := range credits.Entries {
		total += entry.Amount
	}
	if credits.NextCursor == 0 && total != credits.Balance {
		t.Fatalf("ledger sum %d != balance %d", total, credits.Balance)
	}
	return credits
}

func TestPropagationAndBranchDeduplication(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	root := creditUser(t, s, "root", 0)
	a := creditUser(t, s, "alice", root.UserID)
	b := creditUser(t, s, "bob", a.UserID)
	c := creditUser(t, s, "carol", b.UserID)
	d := creditUser(t, s, "david", c.UserID)
	x := []model.Account{{Platform: "github", Username: "target-x"}}
	creditMark(t, s, a, model.MarkInput{Accounts: x, Marked: true, Key: "a-first"})
	balance(t, s, a.UserID, 0)
	creditMark(t, s, c, model.MarkInput{Accounts: x, Marked: true, Key: "c-confirms"})
	aCredits := balance(t, s, a.UserID, 10)
	bCredits := balance(t, s, b.UserID, 2)
	balance(t, s, root.UserID, 0) // A is skipped, without promoting the third ancestor.
	balance(t, s, c.UserID, 0)
	if aCredits.Entries[0].Reason != "first_mark" || bCredits.Entries[0].Reason != "invitation_contribution" {
		t.Fatal("reward origins were lost")
	}
	if *aCredits.Entries[0].ConfirmedByID != c.UserID || *bCredits.Entries[0].ConfirmedByID != c.UserID {
		t.Fatal("confirmation provenance was lost")
	}
	creditMark(t, s, d, model.MarkInput{Accounts: x, Marked: true, Key: "d-same-branch"})
	creditMark(t, s, c, model.MarkInput{Accounts: x, Marked: false, Key: "c-withdraws"})
	creditMark(t, s, c, model.MarkInput{Accounts: x, Marked: true, Key: "c-repeats"})
	creditMark(t, s, c, model.MarkInput{Accounts: x, Marked: true, Key: "c-confirms"})
	balance(t, s, a.UserID, 10)
	balance(t, s, b.UserID, 2)
	balance(t, s, c.UserID, 0)
	outsider := creditUser(t, s, "outsider", 0)
	y := []model.Account{{Platform: "github", Username: "target-y"}}
	creditMark(t, s, outsider, model.MarkInput{Accounts: y, Marked: true, Key: "outsider-first"})
	creditMark(t, s, d, model.MarkInput{Accounts: y, Marked: true, Key: "d-confirms-outsider"})
	balance(t, s, outsider.UserID, 10)
	balance(t, s, c.UserID, 2)
	balance(t, s, b.UserID, 3)
	balance(t, s, a.UserID, 10)
	creditMark(t, s, c, model.MarkInput{Accounts: y, Marked: true, Key: "c-same-outside-root"})
	balance(t, s, outsider.UserID, 10)
}

func TestRewardRequiresActiveFirstMarker(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	a := creditUser(t, s, "alice", 0)
	b := creditUser(t, s, "bob", 0)
	x := []model.Account{{Platform: "github", Username: "target"}}
	creditMark(t, s, a, model.MarkInput{Accounts: x, Marked: true, Key: "first"})
	creditMark(t, s, a, model.MarkInput{Accounts: x, Marked: false, Key: "withdraw"})
	creditMark(t, s, b, model.MarkInput{Accounts: x, Marked: true, Key: "confirm"})
	balance(t, s, a.UserID, 0)
	balance(t, s, b.UserID, 0)
	creditMark(t, s, a, model.MarkInput{Accounts: x, Marked: true, Key: "restore"})
	balance(t, s, a.UserID, 10)
	creditMark(t, s, a, model.MarkInput{Accounts: x, Marked: false, Key: "withdraw-again"})
	creditMark(t, s, b, model.MarkInput{Accounts: x, Marked: false, Key: "confirm-withdraw"})
	creditMark(t, s, b, model.MarkInput{Accounts: x, Marked: true, Key: "confirm-again"})
	creditMark(t, s, a, model.MarkInput{Accounts: x, Marked: true, Key: "restore-again"})
	balance(t, s, a.UserID, 10)
	balance(t, s, b.UserID, 0)
}

func TestRewardCountAndTotalCaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		maximum       int64
		confirmations int
		expected      int64
	}{
		{name: "confirmation count", maximum: 100, confirmations: 1, expected: 13},
		{name: "total credits", maximum: 20, confirmations: 3, expected: 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := config.DefaultCredits()
			policy.MaxRewardPerAccount = tt.maximum
			policy.MaxConfirmationsPerAccount = tt.confirmations
			s, err := openTestService(t.Context(), postgresTestDSN(t), policy)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			first := creditUser(t, s, "first", 0)
			x := []model.Account{{Platform: "github", Username: "target"}}
			creditMark(t, s, first, model.MarkInput{Accounts: x, Marked: true, Key: "first"})
			for i := range 2 {
				root := creditUser(t, s, fmt.Sprintf("root%d", i), 0)
				parent := creditUser(t, s, fmt.Sprintf("parent%d", i), root.UserID)
				child := creditUser(t, s, fmt.Sprintf("child%d", i), parent.UserID)
				creditMark(t, s, child, model.MarkInput{Accounts: x, Marked: true, Key: "confirm"})
			}
			target, err := s.client.Account.Query().Where(account.UsernameEQ("target")).Only(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if target.AwardedCredits != tt.expected || target.RewardedConfirmations != 1 {
				t.Fatalf("budget exceeded: %+v", target)
			}
			balance(t, s, first.UserID, 10)
		})
	}
}

func TestConcurrentRewardsAcrossStores(t *testing.T) {
	t.Parallel()
	dsn := postgresTestDSN(t)
	first, second := testStore(t, dsn), testStore(t, dsn)
	a := creditUser(t, first, "alice", 0)
	b := creditUser(t, first, "bob", a.UserID)
	c := creditUser(t, first, "carol", b.UserID)
	x := []model.Account{{Platform: "github", Username: "target"}}
	creditMark(t, first, a, model.MarkInput{Accounts: x, Marked: true, Key: "first"})
	var workers sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		workers.Go(func() {
			store := first
			if i%2 != 0 {
				store = second
			}
			_, _, err := store.Mark(t.Context(), c, model.MarkInput{Accounts: x, Marked: true, Key: fmt.Sprintf("confirm-%d", i)})
			results <- err
		})
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	balance(t, first, a.UserID, 10)
	balance(t, first, b.UserID, 2)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := testStore(t, dsn)
	creditMark(t, reopened, c, model.MarkInput{Accounts: x, Marked: false, Key: "withdraw"})
	creditMark(t, reopened, c, model.MarkInput{Accounts: x, Marked: true, Key: "restore"})
	balance(t, reopened, a.UserID, 10)
	balance(t, reopened, b.UserID, 2)
}

func TestCreditFailureRollsBackMarksAndAwards(t *testing.T) {
	t.Parallel()
	dsn := postgresTestDSN(t)
	s := testStore(t, dsn)
	a := creditUser(t, s, "alice", 0)
	b := creditUser(t, s, "bob", a.UserID)
	c := creditUser(t, s, "carol", b.UserID)
	x := []model.Account{{Platform: "github", Username: "target"}}
	creditMark(t, s, a, model.MarkInput{Accounts: x, Marked: true, Key: "first"})
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `ALTER TABLE credit_entries ADD CONSTRAINT reject_reward
		CHECK (reason <> 'invitation_contribution')`)
	if err != nil {
		t.Fatal(err)
	}
	input := model.MarkInput{Accounts: x, Marked: true, Key: "confirmation"}
	if _, _, err := s.Mark(t.Context(), c, input); err == nil {
		t.Fatal("expected payout failure")
	}
	balance(t, s, a.UserID, 0)
	balance(t, s, b.UserID, 0)
	states, err := s.Query(t.Context(), c, x)
	if err != nil || states[0].MarkedByMe {
		t.Fatalf("failed payout left a mark: %+v, %v", states, err)
	}
	count, err := s.client.Confirmation.Query().Where(confirmation.UserIDEQ(c.UserID)).Count(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("failed payout left a confirmation: %d, %v", count, err)
	}
	if _, err := db.ExecContext(t.Context(), "ALTER TABLE credit_entries DROP CONSTRAINT reject_reward"); err != nil {
		t.Fatal(err)
	}
	creditMark(t, s, c, input)
	balance(t, s, a.UserID, 10)
	balance(t, s, b.UserID, 2)
}

func TestInvitationSingleUseAndRollback(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	issuer := creditUser(t, s, "issuer", 0)
	invite, err := s.client.Invitation.Create().SetCode(newSecret("sli_")).SetIssuerID(issuer.UserID).
		SetPurchaseKey("registration-test").Save(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Register(t.Context(), model.Registration{Username: "issuer", Password: "test-password", InvitationCode: invite.Code})
	if !errors.Is(err, model.ErrUsernameTaken) {
		t.Fatalf("duplicate user returned %v", err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 4)
	for i := range 4 {
		workers.Go(func() {
			u, err := s.Register(t.Context(), model.Registration{
				Username: fmt.Sprintf("invitee%d", i), Password: "test-password", InvitationCode: invite.Code,
			})
			if err == nil && (u.InviterID == nil || *u.InviterID != issuer.UserID) {
				err = fmt.Errorf("missing invitation relationship")
			}
			results <- err
		})
	}
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, model.ErrInvalidInvitation) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("invitation used %d times", successes)
	}
	invites, _, err := s.Invitations(t.Context(), issuer.UserID, model.Page{Limit: 20})
	if err != nil || len(invites) != 1 || invites[0].RedeemerID == nil || invites[0].UsedAt == nil {
		t.Fatalf("missing invitation trail: %+v, %v", invites, err)
	}
	balance(t, s, issuer.UserID, 0)
}

func TestConcurrentInvitationPurchase(t *testing.T) {
	t.Parallel()
	dsn := postgresTestDSN(t)
	s, other := testStore(t, dsn), testStore(t, dsn)
	a := creditUser(t, s, "alice", 0)
	witness := creditUser(t, s, "witness", 0)
	accounts := make([]model.Account, 10)
	for i := range accounts {
		accounts[i] = model.Account{Platform: "github", Username: fmt.Sprintf("target-%d", i)}
	}
	creditMark(t, s, a, model.MarkInput{Accounts: accounts, Marked: true, Key: "first"})
	creditMark(t, s, witness, model.MarkInput{Accounts: accounts, Marked: true, Key: "confirm"})
	balance(t, s, a.UserID, 100)
	type purchase struct {
		invite *model.Invitation
		key    string
		err    error
	}
	results := make(chan purchase, 2)
	var workers sync.WaitGroup
	for i := range 2 {
		workers.Go(func() {
			store := s
			if i != 0 {
				store = other
			}
			key := fmt.Sprintf("purchase-%d", i)
			invite, _, err := store.PurchaseInvitation(t.Context(), a.UserID, key)
			results <- purchase{invite: invite, key: key, err: err}
		})
	}
	workers.Wait()
	close(results)
	var winner purchase
	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			winner = result
			continue
		}
		if !errors.Is(result.err, model.ErrInsufficientCredits) {
			t.Fatal(result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("purchased %d invitations", successes)
	}
	balance(t, s, a.UserID, 0)
	replayed, ok, err := other.PurchaseInvitation(t.Context(), a.UserID, winner.key)
	if err != nil || !ok || replayed.Code != winner.invite.Code {
		t.Fatalf("purchase replay failed: %+v, %v", replayed, err)
	}
	count, err := s.client.Invitation.Query().Where(invitation.IssuerIDEQ(a.UserID)).Count(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("purchase was duplicated: %d, %v", count, err)
	}
	balance(t, s, a.UserID, 0)
}

func TestMigrationPreservesLegacyMarksWithoutRetroactiveRewards(t *testing.T) {
	t.Parallel()
	dsn := postgresTestDSN(t)
	client, err := openFixtureClient(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tables, err := entschema.CopyTables(migrate.Tables)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.Create(t.Context(), client.Schema, tables); err != nil {
		t.Fatal(err)
	}
	legacy, err := client.User.Create().SetUsername("legacy").SetPasswordHash("").Save(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Mark.Create().SetUserID(legacy.ID).SetPlatform("github").SetUsername("old-target").Save(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, dsn)
	newcomer := creditUser(t, s, "newcomer", 0)
	targets := []model.Account{{Platform: "github", Username: "old-target"}}
	states, err := s.Query(t.Context(), newcomer, targets)
	if err != nil || states[0].MarkCount != 1 {
		t.Fatalf("legacy mark lost: %+v, %v", states, err)
	}
	creditMark(t, s, newcomer, model.MarkInput{Accounts: targets, Marked: true, Key: "confirm-legacy"})
	balance(t, s, legacy.ID, 0)
	balance(t, s, newcomer.UserID, 0)
	target, err := s.client.Account.Query().Where(account.UsernameEQ("old-target")).Only(t.Context())
	if err != nil || target.FirstMarkerID != nil {
		t.Fatalf("invented legacy attribution: %+v, %v", target, err)
	}
}

func TestInvitationDebitFailureRollsBack(t *testing.T) {
	t.Parallel()
	dsn := postgresTestDSN(t)
	s := testStore(t, dsn)
	a := creditUser(t, s, "alice", 0)
	witness := creditUser(t, s, "witness", 0)
	accounts := make([]model.Account, 10)
	for i := range accounts {
		accounts[i] = model.Account{Platform: "github", Username: fmt.Sprintf("target-%d", i)}
	}
	creditMark(t, s, a, model.MarkInput{Accounts: accounts, Marked: true, Key: "first"})
	creditMark(t, s, witness, model.MarkInput{Accounts: accounts, Marked: true, Key: "confirm"})
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `ALTER TABLE credit_entries ADD CONSTRAINT reject_debit
		CHECK (reason <> 'invitation_purchase')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PurchaseInvitation(t.Context(), a.UserID, "purchase"); err == nil {
		t.Fatal("expected debit failure")
	}
	balance(t, s, a.UserID, 100)
	invites, _, err := s.Invitations(t.Context(), a.UserID, model.Page{Limit: 20})
	if err != nil || len(invites) != 0 {
		t.Fatalf("failed debit left invitation: %+v, %v", invites, err)
	}
	if _, err := db.ExecContext(t.Context(), "ALTER TABLE credit_entries DROP CONSTRAINT reject_debit"); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := s.PurchaseInvitation(t.Context(), a.UserID, "purchase"); err != nil || replayed {
		t.Fatalf("failed purchase consumed key: replayed %v, error %v", replayed, err)
	}
	balance(t, s, a.UserID, 0)
}
