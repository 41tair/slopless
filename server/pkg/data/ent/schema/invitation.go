package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Invitation struct{ ent.Schema }

func (Invitation) Fields() []ent.Field {
	return []ent.Field{
		field.String("code").NotEmpty().Unique().Sensitive().Immutable(),
		field.Int("issuer_id").Optional().Nillable().Immutable(),
		field.String("purchase_key").Default("").Immutable(),
		field.Int64("cost").Default(0).NonNegative().Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("used_at").Optional().Nillable(),
	}
}

func (Invitation) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("issuer", User.Type).Ref("issued_invitations").Field("issuer_id").Unique().Immutable(),
		edge.To("redeemer", User.Type).Unique(),
		edge.To("credit_entry", CreditEntry.Type).Unique(),
	}
}

func (Invitation) Indexes() []ent.Index {
	return []ent.Index{index.Fields("issuer_id", "purchase_key").Unique()}
}
