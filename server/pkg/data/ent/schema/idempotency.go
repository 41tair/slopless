package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Idempotency struct{ ent.Schema }

func (Idempotency) Fields() []ent.Field {
	return []ent.Field{
		field.Int("user_id"),
		field.String("key"),
		field.String("fingerprint"),
		field.Bytes("response"),
		field.Time("expires_at"),
	}
}

func (Idempotency) Edges() []ent.Edge {
	return []ent.Edge{edge.From("user", User.Type).Ref("idempotencies").Field("user_id").Unique().Required()}
}

func (Idempotency) Indexes() []ent.Index {
	// This table is exclusively for POST /openapi/v1/accounts/mark.
	return []ent.Index{index.Fields("user_id", "key").Unique(), index.Fields("expires_at")}
}
