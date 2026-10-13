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
		ID: invite.ID, Code: invite.Code, IssuerID: invite.IssuerID, Cost: invite.Cost,
		CreatedAt: invite.CreatedAt, UsedAt: invite.UsedAt,
	}
	if invite.Edges.Redeemer != nil {
		result.RedeemerID = &invite.Edges.Redeemer.ID
	}
	return result
}

func (s *Store) Invitations(ctx context.Context, userID int, page model.Page) ([]*model.Invitation, int, error) {
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
	row, err := s.client.Invitation.Query().Where(invitation.CodeEQ(code)).Only(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	return invitationInfo(row), nil
}

func (s *Store) InvitationByPurchase(ctx context.Context, userID int, key string) (*model.Invitation, error) {
	row, err := s.client.Invitation.Query().Where(invitation.IssuerIDEQ(userID), invitation.PurchaseKeyEQ(key)).
		WithRedeemer().Only(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	return invitationInfo(row), nil
}

func (s *Store) CreateInvitation(ctx context.Context, input model.Invitation, key *string) (*model.Invitation, error) {
	row, err := s.client.Invitation.Create().SetCode(input.Code).SetNillableIssuerID(input.IssuerID).
		SetNillablePurchaseKey(key).SetCost(input.Cost).Save(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	return invitationInfo(row), nil
}

func (s *Store) ConsumeInvitation(ctx context.Context, id int, at time.Time) error {
	return databaseError(s.client.Invitation.UpdateOneID(id).SetUsedAt(at).Exec(ctx))
}
