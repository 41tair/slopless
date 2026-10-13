package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Credential struct{ ent.Schema }

func (Credential) Fields() []ent.Field {
	return []ent.Field{
		field.Int("user_id"),
		field.Enum("kind").Values("session", "api"),
		field.String("name").Default(""),
		field.String("secret_hash").Unique().Sensitive(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("expires_at"),
	}
}

func (Credential) Edges() []ent.Edge {
	return []ent.Edge{edge.From("user", User.Type).Ref("credentials").Field("user_id").Unique().Required()}
}
