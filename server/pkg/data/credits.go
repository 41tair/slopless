package data

import (
	"context"
	"time"

	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/account"
	"github.com/41tair/slopless/pkg/data/ent/confirmation"
	"github.com/41tair/slopless/pkg/data/ent/creditentry"
	"github.com/41tair/slopless/pkg/data/ent/mark"
	"github.com/41tair/slopless/pkg/model"
)

func (s *Store) Credits(ctx context.Context, userID int, page model.Page) (*model.Credits, error) {
	u, err := s.client.User.Get(ctx, userID)
	if err != nil {
		return nil, databaseError(err)
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
	result := &model.Credits{Balance: u.Credits, Entries: make([]model.CreditEntry, 0, len(rows))}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		result.NextCursor = rows[len(rows)-1].ID
	}
	for _, row := range rows {
		entry := model.CreditEntry{
			ID: row.ID, Amount: row.Amount, BalanceAfter: row.BalanceAfter, Reason: string(row.Reason),
			ConfirmationID: row.ConfirmationID, InvitationID: row.InvitationID, CreatedAt: row.CreatedAt,
		}
		if row.Edges.Account != nil {
			entry.Account = &model.Account{Platform: row.Edges.Account.Platform, Username: row.Edges.Account.Username}
		}
		if row.Edges.Confirmation != nil {
			entry.ConfirmedByID = &row.Edges.Confirmation.UserID
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

type Attribution struct {
	ID int
	model.Account
	FirstMarkerID         *int
	RewardedConfirmations int
	AwardedCredits        int64
}

func attributionRecord(a *ent.Account, err error) (*Attribution, error) {
	if err != nil {
		return nil, databaseError(err)
	}
	return &Attribution{ID: a.ID, Account: model.Account{Platform: a.Platform, Username: a.Username},
		FirstMarkerID: a.FirstMarkerID, RewardedConfirmations: a.RewardedConfirmations, AwardedCredits: a.AwardedCredits}, nil
}

func (s *Store) Attribution(ctx context.Context, target model.Account) (*Attribution, error) {
	return attributionRecord(s.client.Account.Query().Where(
		account.PlatformEQ(target.Platform), account.UsernameEQ(target.Username),
	).Only(ctx))
}

func (s *Store) CreateAttribution(ctx context.Context, target model.Account, userID int) (*Attribution, error) {
	return attributionRecord(s.client.Account.Create().SetPlatform(target.Platform).
		SetUsername(target.Username).SetFirstMarkerID(userID).Save(ctx))
}

func (s *Store) AddAccountReward(ctx context.Context, id int, amount int64) (*Attribution, error) {
	return attributionRecord(s.client.Account.UpdateOneID(id).AddRewardedConfirmations(1).AddAwardedCredits(amount).Save(ctx))
}

type Confirmation struct {
	ID         int
	AccountID  int
	UserID     int
	BranchID   int
	RewardedAt *time.Time
}

func (s *Store) HasConfirmation(ctx context.Context, accountID, userID int) (bool, error) {
	exists, err := s.client.Confirmation.Query().Where(
		confirmation.AccountIDEQ(accountID), confirmation.UserIDEQ(userID),
	).Exist(ctx)
	return exists, databaseError(err)
}

func (s *Store) CreateConfirmation(ctx context.Context, input Confirmation) error {
	_, err := s.client.Confirmation.Create().SetAccountID(input.AccountID).
		SetUserID(input.UserID).SetBranchID(input.BranchID).Save(ctx)
	return databaseError(err)
}

func (s *Store) Confirmations(ctx context.Context, accountID int) ([]Confirmation, error) {
	rows, err := s.client.Confirmation.Query().Where(confirmation.AccountIDEQ(accountID)).
		Order(ent.Asc(confirmation.FieldID)).All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	result := make([]Confirmation, 0, len(rows))
	for _, r := range rows {
		result = append(result, Confirmation{
			ID: r.ID, AccountID: r.AccountID, UserID: r.UserID, BranchID: r.BranchID, RewardedAt: r.RewardedAt,
		})
	}
	return result, nil
}

func (s *Store) RewardConfirmation(ctx context.Context, id int, at time.Time) error {
	return databaseError(s.client.Confirmation.UpdateOneID(id).SetRewardedAt(at).Exec(ctx))
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
	_, err := s.client.CreditEntry.Create().SetUserID(input.UserID).SetAmount(input.Amount).SetBalanceAfter(input.BalanceAfter).
		SetReason(creditentry.Reason(input.Reason)).SetNillableAccountID(input.AccountID).
		SetNillableConfirmationID(input.ConfirmationID).SetNillableInvitationID(input.InvitationID).Save(ctx)
	return databaseError(err)
}

// Backfill only records the identity of pre-credit accounts. Reconstructing an
// original marker from active rows would be incorrect after earlier withdrawals.
func preserveLegacyAccounts(ctx context.Context, client *ent.Client) error {
	tx, err := client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	accounts := []model.Account{}
	if err := tx.Mark.Query().GroupBy(mark.FieldPlatform, mark.FieldUsername).Scan(ctx, &accounts); err != nil {
		return err
	}
	for _, legacy := range accounts {
		exists, err := tx.Account.Query().Where(
			account.PlatformEQ(legacy.Platform), account.UsernameEQ(legacy.Username),
		).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.Account.Create().SetPlatform(legacy.Platform).SetUsername(legacy.Username).Save(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}
