package data

import (
	"context"
	"time"

	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/credential"
	"github.com/41tair/slopless/pkg/data/ent/user"
	"github.com/41tair/slopless/pkg/model"
)

type UserRecord struct {
	model.User
	PasswordHash string
}

func userRecord(u *ent.User, err error) (*UserRecord, error) {
	if err != nil {
		return nil, databaseError(err)
	}
	return &UserRecord{User: model.User{
		ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt, Credits: u.Credits,
		InviterID: u.InviterID, InvitationID: u.InvitationID,
	}, PasswordHash: u.PasswordHash}, nil
}

func (s *Store) User(ctx context.Context, id int) (*UserRecord, error) {
	if s.plan != nil {
		return s.plan.user(ctx, id)
	}
	return userRecord(s.client.User.Get(ctx, id))
}

func (s *Store) UserByName(ctx context.Context, name string) (*UserRecord, error) {
	if s.plan != nil {
		return s.plan.userByName(ctx, name)
	}
	return userRecord(s.client.User.Query().Where(user.UsernameEQ(name)).Only(ctx))
}

func (s *Store) CreateUser(ctx context.Context, input UserRecord) (*model.User, error) {
	if s.plan != nil {
		return s.plan.createUser(ctx, input)
	}
	var result *model.User
	err := s.WithTx(ctx, func(tx *Store) error {
		result = nil
		var err error
		result, err = tx.CreateUser(ctx, input)
		return err
	})
	return result, err
}

func (s *Store) SetBalance(ctx context.Context, userID int, balance int64) error {
	if s.plan != nil {
		return s.plan.setBalance(ctx, userID, balance)
	}
	return s.WithTx(ctx, func(tx *Store) error { return tx.SetBalance(ctx, userID, balance) })
}

type Credential struct {
	ID         int
	UserID     int
	Kind       string
	Name       string
	SecretHash string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

func credentialRecord(c *ent.Credential, err error) (*Credential, error) {
	if err != nil {
		return nil, databaseError(err)
	}
	return &Credential{ID: c.ID, UserID: c.UserID, Kind: string(c.Kind), Name: c.Name,
		SecretHash: c.SecretHash, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt}, nil
}

func (s *Store) CreateCredential(ctx context.Context, input Credential) (*Credential, error) {
	if s.plan != nil {
		return s.plan.createCredential(ctx, input)
	}
	var result *Credential
	err := s.WithTx(ctx, func(tx *Store) error {
		result = nil
		var err error
		result, err = tx.CreateCredential(ctx, input)
		return err
	})
	return result, err
}

func (s *Store) Credential(ctx context.Context, hash, kind string) (*Credential, error) {
	if s.plan != nil {
		return s.plan.credential(ctx, hash, kind)
	}
	return credentialRecord(s.client.Credential.Query().Where(
		credential.SecretHashEQ(hash), credential.KindEQ(credential.Kind(kind)),
	).Only(ctx))
}

func (s *Store) Tokens(ctx context.Context, userID int) ([]*model.Token, error) {
	if s.plan != nil {
		return s.plan.tokens(ctx, userID)
	}
	rows, err := s.client.Credential.Query().Where(credential.UserIDEQ(userID), credential.KindEQ(credential.KindAPI)).
		Order(ent.Asc(credential.FieldID)).All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	result := make([]*model.Token, 0, len(rows))
	for _, c := range rows {
		result = append(result, &model.Token{ID: c.ID, Name: c.Name,
			CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt})
	}
	return result, nil
}

func (s *Store) DeleteToken(ctx context.Context, userID, tokenID int) error {
	if s.plan != nil {
		return s.plan.deleteToken(ctx, userID, tokenID)
	}
	return s.WithTx(ctx, func(tx *Store) error { return tx.DeleteToken(ctx, userID, tokenID) })
}

func (s *Store) DeleteSession(ctx context.Context, id int) error {
	if s.plan != nil {
		return s.plan.deleteSession(ctx, id)
	}
	return s.WithTx(ctx, func(tx *Store) error { return tx.DeleteSession(ctx, id) })
}
