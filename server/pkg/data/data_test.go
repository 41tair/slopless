package data

import (
	"testing"
)

func TestOpenRejectsInvalidD1Configuration(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		accountID  string
		databaseID string
		token      string
	}{
		{name: "missing account ID", databaseID: "database", token: "test-token"},
		{name: "missing database ID", accountID: "account", token: "test-token"},
		{name: "missing token", accountID: "account", databaseID: "database"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(
				t.Context(),
				tt.accountID,
				tt.databaseID,
				tt.token,
			)
			if store != nil {
				_ = store.Close()
				t.Fatal("invalid D1 configuration opened a store")
			}
			if err == nil {
				t.Fatal("Open returned nil error for invalid D1 configuration")
			}
		})
	}
}
