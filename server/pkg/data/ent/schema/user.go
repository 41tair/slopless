package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// User holds the schema definition for the User entity.
type User struct {
	ent.Schema
}

// Fields of the User.
func (User) Fields() []ent.Field {
	return []ent.Field{
		field.String("username").NotEmpty().Unique(),
		field.String("password_hash").Sensitive(),
		field.Int64("credits").Default(0).NonNegative(),
		field.Int("inviter_id").Optional().Nillable().Immutable(),
		field.Int("invitation_id").Optional().Nillable().Unique().Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Edges of the User.
func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("credentials", Credential.Type),
		edge.To("marks", Mark.Type),
		edge.To("idempotencies", Idempotency.Type),
		edge.To("issued_invitations", Invitation.Type),
		edge.From("invitation", Invitation.Type).Ref("redeemer").Field("invitation_id").Unique().Immutable(),
		edge.To("invitees", User.Type),
		edge.From("inviter", User.Type).Ref("invitees").Field("inviter_id").Unique().Immutable(),
		edge.To("discovered_accounts", Account.Type),
		edge.To("confirmations", Confirmation.Type),
		edge.To("credit_entries", CreditEntry.Type),
	}
}
