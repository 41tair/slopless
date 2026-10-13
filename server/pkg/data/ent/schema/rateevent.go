package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type RateEvent struct{ ent.Schema }

func (RateEvent) Fields() []ent.Field {
	return []ent.Field{
		field.String("policy"),
		field.String("subject"),
		field.Int("units").Positive(),
		field.Time("expires_at"),
	}
}

func (RateEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("policy", "subject", "expires_at"),
		index.Fields("expires_at"),
	}
}
