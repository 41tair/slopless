package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
	"golang.org/x/crypto/bcrypt"
)

var userPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,31}$`)
var invitationPattern = regexp.MustCompile(`^sli_[a-f0-9]{64}$`)

func invalid(path, code, message string) error {
	return &model.ValidationError{Path: path, Code: code, Message: message}
}

func normalizeCredentials(username, password string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !userPattern.MatchString(username) {
		return "", invalid("/username", "invalid_username", "Use 3–32 letters, digits, underscores or hyphens.")
	}
	if len(password) < 8 || len(password) > 72 {
		return "", invalid("/password", "invalid_password", "Use a password of 8–72 bytes.")
	}
	return username, nil
}

func secretHash(secret string) string {
	hash := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(hash[:])
}

func newSecret(prefix string) string {
	var secret [32]byte
	_, _ = rand.Read(secret[:]) // crypto/rand.Read never returns an error.
	return prefix + hex.EncodeToString(secret[:])
}

func (s *Service) Register(ctx context.Context, input model.Registration) (*model.User, error) {
	username, err := normalizeCredentials(input.Username, input.Password)
	if err != nil {
		return nil, err
	}
	input.InvitationCode = strings.TrimSpace(input.InvitationCode)
	if !invitationPattern.MatchString(input.InvitationCode) {
		return nil, invalid("/invitationCode", "invalid_invitation", "Provide a valid invitation code.")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	var result *model.User
	err = s.store.WithTx(ctx, func(tx *data.Store) error {
		invite, err := tx.InvitationByCode(ctx, input.InvitationCode)
		if errors.Is(err, model.ErrNotFound) {
			return model.ErrInvalidInvitation
		}
		if err != nil {
			return err
		}
		if invite.UsedAt != nil {
			return model.ErrInvalidInvitation
		}
		result, err = tx.CreateUser(ctx, data.UserRecord{
			User:         model.User{Username: username, InvitationID: &invite.ID, InviterID: invite.IssuerID},
			PasswordHash: string(hash),
		})
		if errors.Is(err, data.ErrConflict) {
			return model.ErrUsernameTaken
		}
		if err != nil {
			return err
		}
		return tx.ConsumeInvitation(ctx, invite.ID, time.Now())
	})
	return result, err
}

// CreateUser creates an account without invitation provenance for local administration only.
func (s *Service) CreateUser(ctx context.Context, username, password string) (*model.User, error) {
	username, err := normalizeCredentials(username, password)
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	var result *model.User
	err = s.store.WithTx(ctx, func(tx *data.Store) error {
		result, err = tx.CreateUser(ctx, data.UserRecord{
			User:         model.User{Username: username},
			PasswordHash: string(hash),
		})
		if errors.Is(err, data.ErrConflict) {
			return model.ErrUsernameTaken
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// A valid dummy hash keeps unknown-user logins on the same bcrypt path.
const dummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

type LoginResult struct {
	User      *model.User
	Secret    string
	ExpiresAt time.Time
}

func (s *Service) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	username, err := normalizeCredentials(username, password)
	if err != nil {
		return nil, err
	}
	u, err := s.store.UserByName(ctx, username)
	if errors.Is(err, model.ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))
		return nil, model.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, model.ErrUnauthorized
	}
	secret := newSecret("sls_")
	expires := time.Now().Add(7 * 24 * time.Hour)
	_, err = s.store.CreateCredential(ctx, data.Credential{
		UserID: u.ID, Kind: "session", SecretHash: secretHash(secret), ExpiresAt: expires,
	})
	if err != nil {
		return nil, err
	}
	return &LoginResult{User: &u.User, Secret: secret, ExpiresAt: expires}, nil
}

func (s *Service) AuthenticateSession(ctx context.Context, secret string) (*model.Identity, error) {
	return s.authenticate(ctx, secret, "session")
}

func (s *Service) AuthenticateAPI(ctx context.Context, secret string) (*model.Identity, error) {
	return s.authenticate(ctx, secret, "api")
}

func (s *Service) authenticate(ctx context.Context, secret, kind string) (*model.Identity, error) {
	if secret == "" {
		return nil, model.ErrUnauthorized
	}
	c, err := s.store.Credential(ctx, secretHash(secret), kind)
	if errors.Is(err, model.ErrNotFound) {
		return nil, model.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if !c.ExpiresAt.After(time.Now()) {
		return nil, model.ErrUnauthorized
	}
	return &model.Identity{UserID: c.UserID, TokenID: c.ID}, nil
}

func (s *Service) User(ctx context.Context, id int) (*model.User, error) {
	u, err := s.store.User(ctx, id)
	if err != nil {
		return nil, err
	}
	return &u.User, nil
}

func (s *Service) CreateToken(ctx context.Context, input model.TokenInput) (*model.Token, string, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		return nil, "", invalid("/name", "invalid_name", "Provide a token name of 1–100 bytes.")
	}
	expires := time.Now().Add(365 * 24 * time.Hour)
	if input.ExpiresAt != nil {
		expires = *input.ExpiresAt
	}
	if !expires.After(time.Now()) {
		return nil, "", invalid("/expiresAt", "invalid_expiration", "Token expiration must be in the future.")
	}
	secret := newSecret("slp_")
	c, err := s.store.CreateCredential(ctx, data.Credential{UserID: input.UserID, Kind: "api", Name: input.Name,
		SecretHash: secretHash(secret), ExpiresAt: expires})
	if err != nil {
		return nil, "", err
	}
	return &model.Token{ID: c.ID, Name: c.Name, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt}, secret, nil
}

func (s *Service) Tokens(ctx context.Context, userID int) ([]*model.Token, error) {
	return s.store.Tokens(ctx, userID)
}

func (s *Service) DeleteToken(ctx context.Context, userID, tokenID int) error {
	return s.store.DeleteToken(ctx, userID, tokenID)
}

func (s *Service) Logout(ctx context.Context, identity model.Identity) error {
	return s.store.DeleteSession(ctx, identity.TokenID)
}
