package data

import (
	"strings"
	"testing"
)

func TestOpenRejectsUnsupportedDSN(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		dsn  string
	}{
		{name: "empty"},
		{name: "file", dsn: "file:database.db"},
		{name: "other database", dsn: "mysql://localhost/slopless"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(t.Context(), tt.dsn)
			if store != nil {
				_ = store.Close()
				t.Fatal("unsupported DSN opened a store")
			}
			if err == nil || !strings.Contains(err.Error(), "postgres:// or postgresql://") {
				t.Fatalf("Open returned %v, want PostgreSQL URL validation error", err)
			}
		})
	}
}
