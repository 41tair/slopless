package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
)

const maxCreditBalance int64 = 9_007_199_254_740_991

func validatePage(page model.Page) error {
	if page.Limit < 1 || page.Limit > 100 {
		return invalid("/limit", "invalid_limit", "Limit must be between 1 and 100.")
	}
	if page.Before < 0 {
		return invalid("/before", "invalid_cursor", "Provide a positive cursor ID.")
	}
	return nil
}

func (s *Service) Credits(ctx context.Context, userID int, page model.Page) (*model.Credits, error) {
	if err := validatePage(page); err != nil {
		return nil, err
	}
	var result *model.Credits
	err := s.store.WithTx(ctx, func(tx *data.Store) error {
		var err error
		result, err = tx.Credits(ctx, userID, page)
		return err
	})
	if err != nil {
		return nil, err
	}
	result.Rules = s.credits
	result.Rules.InvitationRewards = slices.Clone(s.credits.InvitationRewards)
	return result, nil
}

type markChange struct {
	Account model.Account
	UserID  int
	Marked  bool
}

func (s *Service) setMark(ctx context.Context, store *data.Store, change markChange) error {
	target := change.Account
	if !change.Marked {
		return store.DeleteMark(ctx, change.UserID, target)
	}
	attribution, err := store.Attribution(ctx, target)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	if attribution == nil {
		attribution, err = store.CreateAttribution(ctx, target, change.UserID)
		if err != nil {
			return err
		}
	}
	exists, err := store.HasMark(ctx, change.UserID, target)
	if err != nil {
		return err
	}
	if !exists {
		if err := store.CreateMark(ctx, change.UserID, target); err != nil {
			return err
		}
	}

	if attribution.FirstMarkerID == nil {
		return nil // Legacy marks have no provable first marker and earn no retroactive rewards.
	}
	if *attribution.FirstMarkerID != change.UserID {
		if err := recordConfirmation(
			ctx,
			store,
			attribution,
			change.UserID,
		); err != nil {
			return err
		}
	}
	return s.rewardConfirmations(ctx, store, attribution)
}

// inviterLine returns direct inviter first and the seed user last. Registration
// only attaches a new user to an existing inviter, so the graph is a forest.
func inviterLine(ctx context.Context, store *data.Store, userID int) ([]int, error) {
	line := []int{}
	seen := map[int]bool{userID: true}
	for {
		u, err := store.User(ctx, userID)
		if err != nil {
			return nil, err
		}
		if u.InviterID == nil {
			return line, nil
		}
		userID = *u.InviterID
		if seen[userID] {
			return nil, fmt.Errorf("invitation graph contains a cycle")
		}
		seen[userID] = true
		line = append(line, userID)
	}
}

// A descendant's branch is the first child below the original marker. For an
// outside confirmer it is their seed root. A whole branch can confirm only once.
func confirmationBranch(firstMarkerID, confirmerID int, line []int) int {
	branch := confirmerID
	for _, inviter := range line {
		if inviter == firstMarkerID {
			return branch
		}
		branch = inviter
	}
	return branch
}

func recordConfirmation(ctx context.Context, store *data.Store, target *data.Attribution, userID int) error {
	exists, err := store.HasConfirmation(ctx, target.ID, userID)
	if err != nil || exists {
		return err
	}
	line, err := inviterLine(ctx, store, userID)
	if err != nil {
		return err
	}
	return store.CreateConfirmation(ctx, data.Confirmation{AccountID: target.ID, UserID: userID,
		BranchID: confirmationBranch(*target.FirstMarkerID, userID, line)})
}

type creditAward struct {
	UserID int
	Amount int64
	Reason string
}

func (s *Service) rewardConfirmations(ctx context.Context, store *data.Store, target *data.Attribution) error {
	if target.RewardedConfirmations >= s.credits.MaxConfirmationsPerAccount {
		return nil
	}
	marks, err := store.ActiveMarkers(ctx, target.Account)
	if err != nil {
		return err
	}
	active := make(map[int]bool, len(marks))
	for _, m := range marks {
		active[m] = true
	}
	first := *target.FirstMarkerID
	if !active[first] {
		return nil
	}
	confirmations, err := store.Confirmations(ctx, target.ID)
	if err != nil {
		return err
	}
	paidBranches := map[int]bool{}
	for _, event := range confirmations {
		if event.RewardedAt != nil {
			paidBranches[event.BranchID] = true
		}
	}
	for _, event := range confirmations {
		alreadyPaid := event.RewardedAt != nil || paidBranches[event.BranchID]
		if alreadyPaid || !active[event.UserID] {
			continue
		}
		line, err := inviterLine(ctx, store, event.UserID)
		if err != nil {
			return err
		}
		awards := []creditAward{{UserID: first, Amount: s.credits.FirstMarkReward, Reason: "first_mark"}}
		total := s.credits.FirstMarkReward
		for level, amount := range s.credits.InvitationRewards {
			if level >= len(line) {
				break
			}
			if line[level] == first {
				continue // The first marker cannot double-dip in the invitation payout.
			}
			awards = append(awards, creditAward{
				UserID: line[level], Amount: amount, Reason: "invitation_contribution",
			})
			total += amount
		}
		if total > s.credits.MaxRewardPerAccount-target.AwardedCredits {
			continue // Pay complete allocations only; never overspend the account's budget.
		}
		for _, award := range awards {
			if err := payCredit(
				ctx,
				store,
				&event,
				award,
			); err != nil {
				return err
			}
		}
		if err := store.RewardConfirmation(ctx, event.ID, time.Now()); err != nil {
			return err
		}
		target, err = store.AddAccountReward(ctx, target.ID, total)
		if err != nil {
			return err
		}
		paidBranches[event.BranchID] = true
		if target.RewardedConfirmations >= s.credits.MaxConfirmationsPerAccount {
			break
		}
	}
	return nil
}

func payCredit(ctx context.Context, store *data.Store, event *data.Confirmation, award creditAward) error {
	u, err := store.User(ctx, award.UserID)
	if err != nil {
		return err
	}
	if u.Credits > maxCreditBalance-award.Amount {
		return fmt.Errorf("credit balance exceeds the supported range")
	}
	balance := u.Credits + award.Amount
	if err := store.SetBalance(ctx, u.ID, balance); err != nil {
		return err
	}
	return store.CreateCreditEntry(ctx, data.CreditRecord{UserID: award.UserID, Amount: award.Amount,
		BalanceAfter: balance, Reason: award.Reason, AccountID: &event.AccountID, ConfirmationID: &event.ID})
}
