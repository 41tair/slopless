package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/41tair/slopless/pkg/model"
)

func TestServiceRejectsInvalidInputBeforePersistence(t *testing.T) {
	t.Parallel()
	// Invalid business input must be rejected even without an HTTP controller or
	// an available database. A store access here would panic and fail the test.
	s := &Service{}
	zeroTime := time.Time{}
	tests := []struct {
		name string
		path string
		run  func() error
	}{
		{name: "login username", path: "/username", run: func() error {
			_, err := s.Login(t.Context(), "x", "valid-password")
			return err
		}},
		{name: "login password", path: "/password", run: func() error {
			_, err := s.Login(t.Context(), "alice", "short")
			return err
		}},
		{name: "registration invitation", path: "/invitationCode", run: func() error {
			_, err := s.Register(t.Context(), model.Registration{Username: "alice", Password: "valid-password"})
			return err
		}},
		{name: "token name", path: "/name", run: func() error {
			_, _, err := s.CreateToken(t.Context(), model.TokenInput{Name: "  "})
			return err
		}},
		{name: "explicit zero expiration", path: "/expiresAt", run: func() error {
			_, _, err := s.CreateToken(t.Context(), model.TokenInput{
				Name: "test", ExpiresAt: &zeroTime,
			})
			return err
		}},
		{name: "query size", path: "/accounts", run: func() error {
			_, err := s.Query(t.Context(), model.Identity{}, make([]model.Account, 101))
			return err
		}},
		{name: "mark size before deduplication", path: "/accounts", run: func() error {
			accounts := make([]model.Account, 51)
			for i := range accounts {
				accounts[i] = model.Account{Platform: "github", Username: "alice"}
			}
			_, _, err := s.Mark(t.Context(), model.Identity{}, model.MarkInput{Accounts: accounts, Key: "test"})
			return err
		}},
		{name: "unsupported platform", path: "/accounts/1/platform", run: func() error {
			_, _, err := s.Mark(t.Context(), model.Identity{}, model.MarkInput{Key: "test", Accounts: []model.Account{
				{Platform: "github", Username: "alice"}, {Platform: "gitlab", Username: "bob"},
			}})
			return err
		}},
		{name: "account profile URL", path: "/accounts/0/username", run: func() error {
			_, err := s.Query(t.Context(), model.Identity{}, []model.Account{
				{Platform: "github", Username: "https://github.com/alice"},
			})
			return err
		}},
		{name: "credit page", path: "/limit", run: func() error {
			_, err := s.Credits(t.Context(), 1, model.Page{Limit: 101})
			return err
		}},
		{name: "invitation cursor", path: "/before", run: func() error {
			_, _, err := s.Invitations(t.Context(), 1, model.Page{Limit: 20, Before: -1})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			var validation *model.ValidationError
			if !errors.As(err, &validation) || validation.Path != tt.path {
				t.Fatalf("error = %v, want validation at %s", err, tt.path)
			}
		})
	}
}

func TestCredentialNormalization(t *testing.T) {
	t.Parallel()
	username, err := normalizeCredentials(" ALICE ", "valid-password")
	if err != nil || username != "alice" {
		t.Fatalf("normalized username = %q, error = %v", username, err)
	}
	// The password limit is measured in bytes, including non-ASCII passwords.
	_, err = normalizeCredentials("alice", strings.Repeat("密", 25))
	var validation *model.ValidationError
	if !errors.As(err, &validation) || validation.Path != "/password" {
		t.Fatalf("oversize password error = %v", err)
	}
}
