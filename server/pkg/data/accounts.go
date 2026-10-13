package data

import (
	"context"
	"time"

	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/idempotency"
	"github.com/41tair/slopless/pkg/data/ent/mark"
	"github.com/41tair/slopless/pkg/data/ent/predicate"
	"github.com/41tair/slopless/pkg/model"
)

func (s *Store) Snapshot(ctx context.Context, userID int, accounts []model.Account) ([]model.AccountState, error) {
	predicates := make([]predicate.Mark, 0, len(accounts))
	for _, account := range accounts {
		predicates = append(predicates, mark.And(mark.PlatformEQ(account.Platform), mark.UsernameEQ(account.Username)))
	}
	counts := []struct {
		Platform string `json:"platform"`
		Username string `json:"username"`
		Count    int64  `json:"count"`
	}{}
	err := s.client.Mark.Query().Where(mark.Or(predicates...)).
		GroupBy(mark.FieldPlatform, mark.FieldUsername).Aggregate(ent.Count()).Scan(ctx, &counts)
	if err != nil {
		return nil, databaseError(err)
	}
	states := make(map[model.Account]model.AccountState, len(accounts))
	for _, count := range counts {
		account := model.Account{Platform: count.Platform, Username: count.Username}
		states[account] = model.AccountState{MarkCount: count.Count}
	}
	marks, err := s.client.Mark.Query().Where(mark.UserIDEQ(userID), mark.Or(predicates...)).All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	for _, m := range marks {
		account := model.Account{Platform: m.Platform, Username: m.Username}
		state := states[account]
		state.MarkedByMe = true
		states[account] = state
	}
	result := make([]model.AccountState, 0, len(accounts))
	for _, account := range accounts {
		state := states[account]
		state.Account = account
		state.Marked = state.MarkCount > 0
		result = append(result, state)
	}
	return result, nil
}

type Replay struct {
	ID          int
	UserID      int
	Key         string
	Fingerprint string
	Response    []byte
	ExpiresAt   time.Time
}

func (s *Store) Replay(ctx context.Context, userID int, key string) (*Replay, error) {
	r, err := s.client.Idempotency.Query().Where(idempotency.UserIDEQ(userID), idempotency.KeyEQ(key)).Only(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	return &Replay{
		ID: r.ID, UserID: r.UserID, Key: r.Key, Fingerprint: r.Fingerprint,
		Response: r.Response, ExpiresAt: r.ExpiresAt,
	}, nil
}

func (s *Store) SaveReplay(ctx context.Context, r Replay) error {
	_, err := s.client.Idempotency.Create().SetUserID(r.UserID).SetKey(r.Key).SetFingerprint(r.Fingerprint).
		SetResponse(r.Response).SetExpiresAt(r.ExpiresAt).Save(ctx)
	return databaseError(err)
}

func (s *Store) DeleteReplay(ctx context.Context, id int) error {
	return databaseError(s.client.Idempotency.DeleteOneID(id).Exec(ctx))
}

func (s *Store) HasMark(ctx context.Context, userID int, target model.Account) (bool, error) {
	exists, err := s.client.Mark.Query().Where(
		mark.UserIDEQ(userID), mark.PlatformEQ(target.Platform), mark.UsernameEQ(target.Username),
	).Exist(ctx)
	return exists, databaseError(err)
}

func (s *Store) CreateMark(ctx context.Context, userID int, target model.Account) error {
	_, err := s.client.Mark.Create().SetUserID(userID).SetPlatform(target.Platform).SetUsername(target.Username).Save(ctx)
	return databaseError(err)
}

func (s *Store) DeleteMark(ctx context.Context, userID int, target model.Account) error {
	_, err := s.client.Mark.Delete().Where(
		mark.UserIDEQ(userID), mark.PlatformEQ(target.Platform), mark.UsernameEQ(target.Username),
	).Exec(ctx)
	return databaseError(err)
}

func (s *Store) ActiveMarkers(ctx context.Context, target model.Account) ([]int, error) {
	ids, err := s.client.Mark.Query().Where(mark.PlatformEQ(target.Platform), mark.UsernameEQ(target.Username)).
		Select(mark.FieldUserID).Ints(ctx)
	return ids, databaseError(err)
}
