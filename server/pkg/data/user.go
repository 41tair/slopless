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
	return userRecord(s.client.User.Get(ctx, id))
}

func (s *Store) UserByName(ctx context.Context, name string) (*UserRecord, error) {
	return userRecord(s.client.User.Query().Where(user.UsernameEQ(name)).Only(ctx))
}

func (s *Store) CreateUser(ctx context.Context, input UserRecord) (*model.User, error) {
	u, err := s.client.User.Create().SetUsername(input.Username).SetPasswordHash(input.PasswordHash).
		SetNillableInvitationID(input.InvitationID).SetNillableInviterID(input.InviterID).Save(ctx)
	result, err := userRecord(u, err)
	if err != nil {
		return nil, err
	}
	return &result.User, nil
}

func (s *Store) SetBalance(ctx context.Context, userID int, balance int64) error {
	return databaseError(s.client.User.UpdateOneID(userID).SetCredits(balance).Exec(ctx))
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
	return credentialRecord(s.client.Credential.Create().SetUserID(input.UserID).SetKind(credential.Kind(input.Kind)).
		SetName(input.Name).SetSecretHash(input.SecretHash).SetExpiresAt(input.ExpiresAt).Save(ctx))
}

func (s *Store) Credential(ctx context.Context, hash, kind string) (*Credential, error) {
	return credentialRecord(s.client.Credential.Query().Where(
		credential.SecretHashEQ(hash), credential.KindEQ(credential.Kind(kind)),
	).Only(ctx))
}

func (s *Store) Tokens(ctx context.Context, userID int) ([]*model.Token, error) {
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
	count, err := s.client.Credential.Delete().Where(credential.IDEQ(tokenID), credential.UserIDEQ(userID),
		credential.KindEQ(credential.KindAPI)).Exec(ctx)
	if err != nil {
		return databaseError(err)
	}
	if count == 0 {
		return model.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSession(ctx context.Context, id int) error {
	_, err := s.client.Credential.Delete().Where(credential.IDEQ(id), credential.KindEQ(credential.KindSession)).Exec(ctx)
	return databaseError(err)
}
