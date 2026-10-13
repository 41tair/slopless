package data

import (
	"context"
	"sort"
	"time"

	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/account"
	"github.com/41tair/slopless/pkg/data/ent/confirmation"
	"github.com/41tair/slopless/pkg/data/ent/creditentry"
	"github.com/41tair/slopless/pkg/model"
)

func (s *Store) Credits(ctx context.Context, userID int, page model.Page) (*model.Credits, error) {
	var balance int64
	if s.plan == nil {
		u, err := s.client.User.Get(ctx, userID)
		if err != nil {
			return nil, databaseError(err)
		}
		balance = u.Credits
	} else {
		u, err := s.User(ctx, userID)
		if err != nil {
			return nil, err
		}
		balance = u.Credits
	}
	query := s.client.CreditEntry.Query().Where(creditentry.UserIDEQ(userID)).WithAccount().WithConfirmation().
		Order(ent.Desc(creditentry.FieldID)).Limit(page.Limit + 1)
	if page.Before > 0 {
		query.Where(creditentry.IDLT(page.Before))
	}
	rows, err := query.All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	entries := make([]model.CreditEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, creditEntryValue(row))
	}
	if s.plan != nil && s.plan.accounts != nil {
		for _, pending := range s.plan.accounts.creditEntries {
			if pending.input.UserID != userID || page.Before > 0 && pending.id >= page.Before {
				continue
			}
			entry, err := s.plan.accounts.creditEntry(ctx, pending)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID > entries[j].ID })
	result := &model.Credits{Balance: balance, Entries: entries}
	if len(result.Entries) > page.Limit {
		result.Entries = result.Entries[:page.Limit]
		result.NextCursor = result.Entries[len(result.Entries)-1].ID
	}
	return result, nil
}

func creditEntryValue(row *ent.CreditEntry) model.CreditEntry {
	entry := model.CreditEntry{
		ID:             row.ID,
		Amount:         row.Amount,
		BalanceAfter:   row.BalanceAfter,
		Reason:         string(row.Reason),
		ConfirmationID: cloneInt(row.ConfirmationID),
		InvitationID:   cloneInt(row.InvitationID),
		CreatedAt:      row.CreatedAt,
	}
	if row.Edges.Account != nil {
		entry.Account = &model.Account{
			Platform: row.Edges.Account.Platform,
			Username: row.Edges.Account.Username,
		}
	}
	if row.Edges.Confirmation != nil {
		confirmedByID := row.Edges.Confirmation.UserID
		entry.ConfirmedByID = &confirmedByID
	}
	return entry
}

type Attribution struct {
	ID int
	model.Account
	FirstMarkerID         *int
	RewardedConfirmations int
	AwardedCredits        int64
}

func attributionRecord(value *ent.Account, err error) (*Attribution, error) {
	if err != nil {
		return nil, databaseError(err)
	}
	return &Attribution{
		ID:                    value.ID,
		Account:               model.Account{Platform: value.Platform, Username: value.Username},
		FirstMarkerID:         cloneInt(value.FirstMarkerID),
		RewardedConfirmations: value.RewardedConfirmations,
		AwardedCredits:        value.AwardedCredits,
	}, nil
}

func (s *Store) Attribution(ctx context.Context, target model.Account) (*Attribution, error) {
	if s.plan != nil {
		return s.plan.accountState().attribution(ctx, target)
	}
	return attributionRecord(s.client.Account.Query().Where(
		account.PlatformEQ(target.Platform),
		account.UsernameEQ(target.Username),
	).Only(ctx))
}

func (s *Store) CreateAttribution(
	ctx context.Context,
	target model.Account,
	userID int,
) (*Attribution, error) {
	if s.plan == nil {
		var result *Attribution
		err := s.WithTx(ctx, func(tx *Store) error {
			var err error
			result, err = tx.CreateAttribution(ctx, target, userID)
			return err
		})
		return result, err
	}
	return s.plan.accountState().createAttribution(ctx, target, userID)
}

func (s *Store) AddAccountReward(ctx context.Context, id int, amount int64) (*Attribution, error) {
	if s.plan == nil {
		var result *Attribution
		err := s.WithTx(ctx, func(tx *Store) error {
			var err error
			result, err = tx.AddAccountReward(ctx, id, amount)
			return err
		})
		return result, err
	}
	return s.plan.accountState().addAccountReward(ctx, id, amount)
}

type Confirmation struct {
	ID         int
	AccountID  int
	UserID     int
	BranchID   int
	RewardedAt *time.Time
}

func (s *Store) HasConfirmation(ctx context.Context, accountID, userID int) (bool, error) {
	if s.plan != nil {
		return s.plan.accountState().hasConfirmation(ctx, accountID, userID)
	}
	exists, err := s.client.Confirmation.Query().Where(
		confirmation.AccountIDEQ(accountID),
		confirmation.UserIDEQ(userID),
	).Exist(ctx)
	return exists, databaseError(err)
}

func (s *Store) CreateConfirmation(ctx context.Context, input Confirmation) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error { return tx.CreateConfirmation(ctx, input) })
	}
	return s.plan.accountState().createConfirmation(ctx, input)
}

func (s *Store) Confirmations(ctx context.Context, accountID int) ([]Confirmation, error) {
	if s.plan != nil {
		return s.plan.accountState().confirmationsFor(ctx, accountID)
	}
	rows, err := s.client.Confirmation.Query().Where(confirmation.AccountIDEQ(accountID)).
		Order(ent.Asc(confirmation.FieldID)).All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	result := make([]Confirmation, 0, len(rows))
	for _, row := range rows {
		result = append(result, confirmationValue(row))
	}
	return result, nil
}

func (s *Store) RewardConfirmation(ctx context.Context, id int, at time.Time) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error { return tx.RewardConfirmation(ctx, id, at) })
	}
	return s.plan.accountState().rewardConfirmation(ctx, id, at)
}

type CreditRecord struct {
	UserID         int
	Amount         int64
	BalanceAfter   int64
	Reason         string
	AccountID      *int
	ConfirmationID *int
	InvitationID   *int
}

func (s *Store) CreateCreditEntry(ctx context.Context, input CreditRecord) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error { return tx.CreateCreditEntry(ctx, input) })
	}
	if err := creditentry.BalanceAfterValidator(input.BalanceAfter); err != nil {
		return err
	}
	if err := creditentry.ReasonValidator(creditentry.Reason(input.Reason)); err != nil {
		return err
	}
	id, err := s.plan.nextID(ctx, "credit_entries")
	if err != nil {
		return databaseError(err)
	}
	input.AccountID = cloneInt(input.AccountID)
	input.ConfirmationID = cloneInt(input.ConfirmationID)
	input.InvitationID = cloneInt(input.InvitationID)
	s.plan.accountState().creditEntries[id] = &plannedCreditEntry{
		id: id, input: input, createdAt: s.plan.now,
	}
	s.plan.insert("credit_entries", id, map[string]any{
		"user_id":         input.UserID,
		"amount":          input.Amount,
		"balance_after":   input.BalanceAfter,
		"reason":          input.Reason,
		"account_id":      nullableAccountInt(input.AccountID),
		"confirmation_id": nullableAccountInt(input.ConfirmationID),
		"invitation_id":   nullableAccountInt(input.InvitationID),
		"created_at":      s.plan.now,
	})
	return nil
}

func nullableAccountInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
