package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
)

// BootstrapInvitations is exposed only through local administration.
func (s *Service) BootstrapInvitations(ctx context.Context, count int) ([]*model.Invitation, error) {
	if count < 1 || count > 1000 {
		return nil, fmt.Errorf("bootstrap invitation count must be between 1 and 1000")
	}
	invites := make([]*model.Invitation, 0, count)
	err := s.store.WithTx(ctx, func(tx *data.Store) error {
		invites = invites[:0]
		for range count {
			invite, err := tx.CreateInvitation(ctx, model.Invitation{Code: newSecret("sli_")}, nil)
			if err != nil {
				return err
			}
			invites = append(invites, invite)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return invites, nil
}

func (s *Service) PurchaseInvitation(ctx context.Context, userID int, key string) (*model.Invitation, bool, error) {
	if key == "" {
		return nil, false, fmt.Errorf("invitation purchase key is required")
	}
	var result *model.Invitation
	replayed := false
	err := s.store.WithTx(ctx, func(tx *data.Store) error {
		result = nil
		replayed = false
		previous, err := tx.InvitationByPurchase(ctx, userID, key)
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			return err
		}
		if previous != nil {
			result = previous
			replayed = true
			return nil
		}
		u, err := tx.User(ctx, userID)
		if err != nil {
			return err
		}
		if u.Credits < s.credits.InvitationCost {
			return model.ErrInsufficientCredits
		}
		balance := u.Credits - s.credits.InvitationCost
		result, err = tx.CreateInvitation(ctx, model.Invitation{
			Code: newSecret("sli_"), IssuerID: &userID, Cost: s.credits.InvitationCost,
		}, &key)
		if err != nil {
			return err
		}
		if err := tx.SetBalance(ctx, userID, balance); err != nil {
			return err
		}
		return tx.CreateCreditEntry(ctx, data.CreditRecord{UserID: userID, Amount: -result.Cost, BalanceAfter: balance,
			Reason: "invitation_purchase", InvitationID: &result.ID})
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}

func (s *Service) Invitations(ctx context.Context, userID int, page model.Page) ([]*model.Invitation, int, error) {
	if err := validatePage(page); err != nil {
		return nil, 0, err
	}
	return s.store.Invitations(ctx, userID, page)
}
