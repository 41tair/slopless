package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Mark struct{ ent.Schema }

func (Mark) Fields() []ent.Field {
	return []ent.Field{
		field.Int("user_id"),
		field.String("platform").NotEmpty(),
		field.String("username").NotEmpty(),
	}
}

func (Mark) Edges() []ent.Edge {
	return []ent.Edge{edge.From("user", User.Type).Ref("marks").Field("user_id").Unique().Required()}
}

func (Mark) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "platform", "username").Unique(),
		index.Fields("platform", "username"),
	}
}
