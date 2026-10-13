package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Confirmation records the first mark by a different user. Withdrawal never
// deletes it, so neither retries nor invitation descendants can restart rewards.
type Confirmation struct{ ent.Schema }

func (Confirmation) Fields() []ent.Field {
	return []ent.Field{
		field.Int("account_id").Immutable(),
		field.Int("user_id").Immutable(),
		field.Int("branch_id").Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("rewarded_at").Optional().Nillable(),
	}
}

func (Confirmation) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("account", Account.Type).Ref("confirmations").Field("account_id").Unique().Required().Immutable(),
		edge.From("user", User.Type).Ref("confirmations").Field("user_id").Unique().Required().Immutable(),
		edge.To("credit_entries", CreditEntry.Type),
	}
}

func (Confirmation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id", "user_id").Unique(),
		index.Fields("account_id", "branch_id", "rewarded_at"),
	}
}
