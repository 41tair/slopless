package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// CreditEntry is an append-only record of a balance change and its origin.
type CreditEntry struct{ ent.Schema }

func (CreditEntry) Fields() []ent.Field {
	return []ent.Field{
		field.Int("user_id").Immutable(),
		field.Int64("amount").Immutable(),
		field.Int64("balance_after").NonNegative().Immutable(),
		field.Enum("reason").Values("first_mark", "invitation_contribution", "invitation_purchase").Immutable(),
		field.Int("account_id").Optional().Nillable().Immutable(),
		field.Int("confirmation_id").Optional().Nillable().Immutable(),
		field.Int("invitation_id").Optional().Nillable().Unique().Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (CreditEntry) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).Ref("credit_entries").Field("user_id").Unique().Required().Immutable(),
		edge.From("account", Account.Type).Ref("credit_entries").Field("account_id").Unique().Immutable(),
		edge.From("confirmation", Confirmation.Type).Ref("credit_entries").Field("confirmation_id").Unique().Immutable(),
		edge.From("invitation", Invitation.Type).Ref("credit_entry").Field("invitation_id").Unique().Immutable(),
	}
}

func (CreditEntry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "id"),
		index.Fields("confirmation_id", "user_id").Unique(),
	}
}
