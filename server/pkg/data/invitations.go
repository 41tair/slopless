package data

import (
	"context"
	"time"

	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/invitation"
	"github.com/41tair/slopless/pkg/model"
)

func invitationInfo(invite *ent.Invitation) *model.Invitation {
	result := &model.Invitation{
		ID: invite.ID, Code: invite.Code, IssuerID: cloneInt(invite.IssuerID), Cost: invite.Cost,
		CreatedAt: invite.CreatedAt, UsedAt: cloneTime(invite.UsedAt),
	}
	if invite.Edges.Redeemer != nil {
		result.RedeemerID = cloneInt(&invite.Edges.Redeemer.ID)
	}
	return result
}

func (s *Store) Invitations(ctx context.Context, userID int, page model.Page) ([]*model.Invitation, int, error) {
	if s.plan != nil {
		return s.plan.invitations(ctx, userID, page)
	}
	query := s.client.Invitation.Query().Where(invitation.IssuerIDEQ(userID)).WithRedeemer().
		Order(ent.Desc(invitation.FieldID)).Limit(page.Limit + 1)
	if page.Before > 0 {
		query.Where(invitation.IDLT(page.Before))
	}
	rows, err := query.All(ctx)
	if err != nil {
		return nil, 0, databaseError(err)
	}
	var next int
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		next = rows[len(rows)-1].ID
	}
	invites := make([]*model.Invitation, 0, len(rows))
	for _, row := range rows {
		invites = append(invites, invitationInfo(row))
	}
	return invites, next, nil
}

func (s *Store) InvitationByCode(ctx context.Context, code string) (*model.Invitation, error) {
	if s.plan != nil {
		return s.plan.invitationByCode(ctx, code)
	}
	row, err := s.client.Invitation.Query().Where(invitation.CodeEQ(code)).WithRedeemer().Only(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	return invitationInfo(row), nil
}

func (s *Store) InvitationByPurchase(ctx context.Context, userID int, key string) (*model.Invitation, error) {
	if s.plan != nil {
		return s.plan.invitationByPurchase(ctx, userID, key)
	}
	row, err := s.client.Invitation.Query().Where(invitation.IssuerIDEQ(userID), invitation.PurchaseKeyEQ(key)).
		WithRedeemer().Only(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	return invitationInfo(row), nil
}

func (s *Store) CreateInvitation(ctx context.Context, input model.Invitation, key *string) (*model.Invitation, error) {
	if s.plan != nil {
		return s.plan.createInvitation(ctx, input, key)
	}
	var result *model.Invitation
	err := s.WithTx(ctx, func(tx *Store) error {
		result = nil
		var err error
		result, err = tx.CreateInvitation(ctx, input, key)
		return err
	})
	return result, err
}

// CreateInvitations creates invitations with the default empty purchase key.
func (s *Store) CreateInvitations(ctx context.Context, inputs []model.Invitation) ([]*model.Invitation, error) {
	if s.plan != nil {
		return s.plan.createInvitations(ctx, inputs)
	}
	if len(inputs) == 0 {
		return []*model.Invitation{}, nil
	}
	var result []*model.Invitation
	err := s.WithTx(ctx, func(tx *Store) error {
		result = nil
		var err error
		result, err = tx.CreateInvitations(ctx, inputs)
		return err
	})
	return result, err
}

func (s *Store) ConsumeInvitation(ctx context.Context, id int, at time.Time) error {
	if s.plan != nil {
		return s.plan.consumeInvitation(ctx, id, at)
	}
	return s.WithTx(ctx, func(tx *Store) error { return tx.ConsumeInvitation(ctx, id, at) })
}
