package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Account preserves attribution even when all active marks are withdrawn.
type Account struct{ ent.Schema }

func (Account) Fields() []ent.Field {
	return []ent.Field{
		field.String("platform").NotEmpty().Immutable(),
		field.String("username").NotEmpty().Immutable(),
		// Existing marks have no trustworthy historical attribution; they stay nil.
		field.Int("first_marker_id").Optional().Nillable().Immutable(),
		field.Int("rewarded_confirmations").Default(0).NonNegative(),
		field.Int64("awarded_credits").Default(0).NonNegative(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (Account) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("first_marker", User.Type).Ref("discovered_accounts").Field("first_marker_id").Unique().Immutable(),
		edge.To("credit_entries", CreditEntry.Type),
		edge.To("confirmations", Confirmation.Type),
	}
}

func (Account) Indexes() []ent.Index {
	return []ent.Index{index.Fields("platform", "username").Unique()}
}
