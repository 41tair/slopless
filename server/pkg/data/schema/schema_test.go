package schema

import (
	"os"
	"testing"
)

func TestDeploymentMigrationMatchesEmbeddedSchema(t *testing.T) {
	deployed, err := os.ReadFile("../../../deploy/cloudflare/migrations/0001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(deployed) != migrationSQL {
		t.Fatal("deployment migration and integration schema differ")
	}
}
