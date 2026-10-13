package data

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/idempotency"
	"github.com/41tair/slopless/pkg/data/ent/mark"
	"github.com/41tair/slopless/pkg/model"
)

func (s *Store) Snapshot(ctx context.Context, userID int, accounts []model.Account) ([]model.AccountState, error) {
	result := make([]model.AccountState, len(accounts))
	for i, account := range accounts {
		result[i].Account = account
	}
	if len(accounts) == 0 {
		return result, nil
	}
	payload, err := json.Marshal(accounts)
	if err != nil {
		return nil, fmt.Errorf("encode requested accounts: %w", err)
	}
	rows, err := s.driver.Read(ctx, d1.Statement{
		SQL: `SELECT requested."ordinal", COUNT(marks."id") AS "mark_count",
			MAX(CASE WHEN marks."user_id" = ? THEN 1 ELSE 0 END) AS "marked_by_me"
		FROM (
			SELECT CAST("key" AS INTEGER) AS "ordinal",
				json_extract("value", '$.platform') AS "platform",
				json_extract("value", '$.username') AS "username"
			FROM json_each(?)
		) AS requested
		LEFT JOIN marks ON marks."platform" = requested."platform"
			AND marks."username" = requested."username"
		GROUP BY requested."ordinal"
		ORDER BY requested."ordinal"`,
		Params: []any{userID, string(payload)},
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if len(rows.Columns) != 3 {
		return nil, fmt.Errorf("snapshot query returned %d columns", len(rows.Columns))
	}
	for _, row := range rows.Rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("snapshot query returned a row with %d columns", len(row))
		}
		ordinal, ok := integerValue(row[0])
		if !ok || ordinal < 0 || ordinal >= int64(len(result)) {
			return nil, fmt.Errorf("snapshot query returned an invalid account ordinal")
		}
		count, ok := integerValue(row[1])
		if !ok || count < 0 {
			return nil, fmt.Errorf("snapshot query returned an invalid mark count")
		}
		own, ok := integerValue(row[2])
		if !ok || (own != 0 && own != 1) {
			return nil, fmt.Errorf("snapshot query returned an invalid ownership flag")
		}
		result[ordinal].MarkCount = count
		result[ordinal].MarkedByMe = own == 1
	}
	if s.plan != nil && s.plan.accounts != nil {
		for _, state := range s.plan.accounts.marks {
			if state.baseExists == state.currentExists {
				continue
			}
			delta := int64(1)
			if !state.currentExists {
				delta = -1
			}
			for i := range result {
				if result[i].Account != state.account {
					continue
				}
				result[i].MarkCount += delta
				if state.userID == userID {
					result[i].MarkedByMe = state.currentExists
				}
			}
		}
	}
	for i := range result {
		result[i].Marked = result[i].MarkCount > 0
	}
	return result, nil
}

func integerValue(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case float64:
		integer := int64(value)
		return integer, float64(integer) == value
	default:
		return 0, false
	}
}

type Replay struct {
	ID          int
	UserID      int
	Key         string
	Fingerprint string
	Response    []byte
	ExpiresAt   time.Time
}

func replayRecord(row *ent.Idempotency, err error) (*Replay, error) {
	if err != nil {
		return nil, databaseError(err)
	}
	return &Replay{
		ID:          row.ID,
		UserID:      row.UserID,
		Key:         row.Key,
		Fingerprint: row.Fingerprint,
		Response:    append([]byte(nil), row.Response...),
		ExpiresAt:   row.ExpiresAt,
	}, nil
}

func (s *Store) Replay(ctx context.Context, userID int, key string) (*Replay, error) {
	if s.plan != nil {
		return s.plan.accountState().replay(ctx, userID, key)
	}
	return replayRecord(s.client.Idempotency.Query().Where(
		idempotency.UserIDEQ(userID),
		idempotency.KeyEQ(key),
	).Only(ctx))
}

func (s *Store) SaveReplay(ctx context.Context, input Replay) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error { return tx.SaveReplay(ctx, input) })
	}
	return s.plan.accountState().saveReplay(ctx, input)
}

func (s *Store) DeleteReplay(ctx context.Context, id int) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error { return tx.DeleteReplay(ctx, id) })
	}
	return s.plan.accountState().deleteReplay(ctx, id)
}

func (s *Store) HasMark(ctx context.Context, userID int, target model.Account) (bool, error) {
	if s.plan != nil {
		return s.plan.accountState().hasMark(ctx, userID, target)
	}
	exists, err := s.client.Mark.Query().Where(
		mark.UserIDEQ(userID),
		mark.PlatformEQ(target.Platform),
		mark.UsernameEQ(target.Username),
	).Exist(ctx)
	return exists, databaseError(err)
}

func (s *Store) CreateMark(ctx context.Context, userID int, target model.Account) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error { return tx.CreateMark(ctx, userID, target) })
	}
	return s.plan.accountState().createMark(ctx, userID, target)
}

func (s *Store) DeleteMark(ctx context.Context, userID int, target model.Account) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error { return tx.DeleteMark(ctx, userID, target) })
	}
	return s.plan.accountState().deleteMark(ctx, userID, target)
}

func (s *Store) ActiveMarkers(ctx context.Context, target model.Account) ([]int, error) {
	rows, err := s.client.Mark.Query().Where(
		mark.PlatformEQ(target.Platform),
		mark.UsernameEQ(target.Username),
	).All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	active := make(map[int]bool, len(rows))
	for _, row := range rows {
		active[row.UserID] = true
	}
	if s.plan != nil && s.plan.accounts != nil {
		for _, state := range s.plan.accounts.marks {
			if state.account == target {
				active[state.userID] = state.currentExists
			}
		}
	}
	result := make([]int, 0, len(active))
	for id, exists := range active {
		if exists {
			result = append(result, id)
		}
	}
	sort.Ints(result)
	return result, nil
}
