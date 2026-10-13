//go:build integration

package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/41tair/slopless/pkg/model"
	"golang.org/x/crypto/bcrypt"
)

func TestCreateUserForLocalAdministration(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	const password = "admin-password"
	u, err := s.CreateUser(t.Context(), " Admin-User ", password)
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "admin-user" || u.Credits != 0 || u.InviterID != nil || u.InvitationID != nil {
		t.Fatalf("unexpected local user: %+v", u)
	}
	record, err := s.store.UserByName(t.Context(), "admin-user")
	if err != nil {
		t.Fatal(err)
	}
	if record.PasswordHash == password || bcrypt.CompareHashAndPassword([]byte(record.PasswordHash), []byte(password)) != nil {
		t.Fatal("password was not stored as a valid bcrypt hash")
	}
	login, err := s.Login(t.Context(), " ADMIN-USER ", password)
	if err != nil || login == nil || login.User == nil || login.User.ID != u.ID {
		t.Fatalf("login = %+v, error = %v", login, err)
	}
	invites, err := s.client.Invitation.Query().Count(t.Context())
	if err != nil || invites != 0 {
		t.Fatalf("local user created invitation provenance: %d, %v", invites, err)
	}
}

func TestCreateUserDuplicatePreservesPassword(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	const originalPassword = "original-password"
	if _, err := s.CreateUser(t.Context(), "operator", originalPassword); err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.CreateUser(t.Context(), " OPERATOR ", "replacement-password")
	if duplicate != nil || !errors.Is(err, model.ErrUsernameTaken) {
		t.Fatalf("duplicate = %+v, error = %v", duplicate, err)
	}
	if _, err := s.Login(t.Context(), "operator", originalPassword); err != nil {
		t.Fatalf("original password stopped working: %v", err)
	}
	login, err := s.Login(t.Context(), "operator", "replacement-password")
	if login != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("replacement password was accepted: %+v, %v", login, err)
	}
	count, err := s.client.User.Query().Count(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("users = %d, error = %v", count, err)
	}
}

func TestCreateUserInvalidCredentialsDoNotPersist(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	tests := []struct {
		name     string
		username string
		password string
		path     string
	}{
		{name: "username", username: "x", password: "valid-password", path: "/username"},
		{name: "password", username: "valid-user", password: "short", path: "/password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := s.CreateUser(t.Context(), tt.username, tt.password)
			var validation *model.ValidationError
			if u != nil || !errors.As(err, &validation) || validation.Path != tt.path {
				t.Fatalf("user = %+v, error = %v, want validation at %s", u, err, tt.path)
			}
			count, err := s.client.User.Query().Count(t.Context())
			if err != nil || count != 0 {
				t.Fatalf("invalid input persisted %d users: %v", count, err)
			}
		})
	}
}

func TestSessionAndTokenRulesWithoutHTTP(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	identity := testIdentity(t, s)
	login, err := s.Login(t.Context(), " ALICE ", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if login.User.ID != identity.UserID || time.Until(login.ExpiresAt) < 6*24*time.Hour {
		t.Fatalf("invalid login result: %+v", login)
	}
	session, err := s.AuthenticateSession(t.Context(), login.Secret)
	if err != nil || session.UserID != identity.UserID {
		t.Fatalf("session = %+v, error = %v", session, err)
	}
	if _, err := s.AuthenticateAPI(t.Context(), login.Secret); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("session accepted as API token: %v", err)
	}
	token, secret, err := s.CreateToken(t.Context(), model.TokenInput{
		UserID: identity.UserID, Name: " extension ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if token.Name != "extension" {
		t.Fatalf("token was not normalized: %+v", token)
	}
	if time.Until(token.ExpiresAt) < 364*24*time.Hour {
		t.Fatal("missing default token lifetime")
	}
	apiIdentity, err := s.AuthenticateAPI(t.Context(), secret)
	if err != nil {
		t.Fatal(err)
	}
	accounts := []model.Account{{Platform: "github", Username: "example"}}
	states, err := s.Query(t.Context(), *apiIdentity, accounts)
	if err != nil || len(states) != 1 || states[0].Marked {
		t.Fatalf("unscoped token query = %+v, error = %v", states, err)
	}
	states, replayed, err := s.Mark(t.Context(), *apiIdentity, model.MarkInput{
		Accounts: accounts, Marked: true, Key: "unscoped-token-mark",
	})
	if err != nil || replayed || len(states) != 1 || !states[0].MarkedByMe {
		t.Fatalf("unscoped token mark = %+v, replayed = %v, error = %v", states, replayed, err)
	}
	if _, err := s.AuthenticateSession(t.Context(), secret); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("API token accepted as session: %v", err)
	}
	if err := s.Logout(t.Context(), *session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateSession(t.Context(), login.Secret); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("logged-out session remained valid: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.User(ctx, identity.UserID); !errors.Is(err, context.Canceled) {
		t.Fatalf("request cancellation was lost: %v", err)
	}
}

func TestAccountNormalizationPrecedesIdempotencyAndCharging(t *testing.T) {
	t.Parallel()
	s := testStore(t, postgresTestDSN(t))
	identity := testIdentity(t, s)
	input := model.MarkInput{Key: "normalized-operation", Marked: true, Accounts: []model.Account{
		{Platform: "github", Username: " Example "},
		{Platform: "github", Username: "example"},
		{Platform: "github", Username: "Build[bot]"},
	}}
	states, replayed, err := s.Mark(t.Context(), identity, input)
	if err != nil || replayed {
		t.Fatalf("mark: replayed = %v, error = %v", replayed, err)
	}
	if len(states) != 2 || states[0].Username != "example" || states[1].Username != "build[bot]" {
		t.Fatalf("incorrect canonical accounts: %+v", states)
	}
	if input.Accounts[0].Username != " Example " {
		t.Fatal("mark mutated caller's accounts")
	}
	input.Accounts = []model.Account{states[0].Account, states[1].Account}
	replay, replayed, err := s.Mark(t.Context(), identity, input)
	if err != nil || !replayed || !reflect.DeepEqual(states, replay) {
		t.Fatalf("canonical replay = %+v, replayed = %v, error = %v", replay, replayed, err)
	}
	for _, policy := range accountPolicies("mark", identity, len(states)) {
		if policy.Unit != "accounts" {
			continue
		}
		events, err := s.store.RateEvents(t.Context(), policy.Policy, policy.Subject, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		units := 0
		for _, event := range events {
			units += event.Units
		}
		if units != 4 {
			t.Fatalf("%s charged %d units, want 2 distinct accounts per request", policy.Policy, units)
		}
	}
}
