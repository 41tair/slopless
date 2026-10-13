// Package schema owns the versioned D1 schema used by deployment and tests.
package schema

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"strings"

	"github.com/41tair/slopless/pkg/d1"
)

const (
	Version        = 1
	expectedTables = 13
)

//go:embed schema.sql
var migrationSQL string

// Check verifies that the database has the application schema expected by this binary.
func Check(ctx context.Context, driver *d1.Driver) error {
	result, err := driver.Read(ctx, d1.Statement{SQL: `
		SELECT "version",
			(SELECT COUNT(*) FROM "sqlite_schema"
			 WHERE "type" = 'table' AND "name" IN (
				'accounts', 'confirmations', 'credentials', 'credit_entries',
				'idempotencies', 'invitations', 'marks', 'rate_events', 'users',
				'__slopless_meta', '__slopless_commit_outcomes',
				'__slopless_ids', '__slopless_schema'
			)) AS "table_count"
		FROM "__slopless_schema" WHERE "id" = 1`,
	})
	if err != nil {
		return fmt.Errorf("read D1 schema metadata: %w", err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 2 {
		return fmt.Errorf("D1 schema metadata is missing")
	}
	version, ok := integer(result.Rows[0][0])
	if !ok || version != Version {
		return fmt.Errorf("D1 schema version is %v, want %d", result.Rows[0][0], Version)
	}
	tables, ok := integer(result.Rows[0][1])
	if !ok || tables != expectedTables {
		return fmt.Errorf("D1 schema has %v of %d required tables", result.Rows[0][1], expectedTables)
	}
	return nil
}

// Apply installs or upgrades the schema in one D1 batch. Production deploys
// apply the matching migration file with Wrangler; this entry point is used by
// isolated integration databases.
func Apply(ctx context.Context, driver *d1.Driver) error {
	if err := Check(ctx, driver); err == nil {
		return nil
	}
	statements, err := statements()
	if err != nil {
		return err
	}
	if _, err := driver.Batch(ctx, statements); err != nil {
		return fmt.Errorf("apply D1 schema version %d: %w", Version, err)
	}
	if err := Check(ctx, driver); err != nil {
		return fmt.Errorf("verify D1 schema version %d: %w", Version, err)
	}
	return nil
}

func statements() ([]d1.Statement, error) {
	parts := strings.Split(migrationSQL, ";")
	result := make([]d1.Statement, 0, len(parts))
	for _, part := range parts {
		query := strings.TrimSpace(part)
		if query == "" {
			continue
		}
		result = append(result, d1.Statement{SQL: query})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("embedded D1 schema is empty")
	}
	return result, nil
}

func integer(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int64:
		if int64(int(value)) == value {
			return int(value), true
		}
	case float64:
		if value >= math.MinInt && value <= math.MaxInt && math.Trunc(value) == value {
			return int(value), true
		}
	}
	return 0, false
}
