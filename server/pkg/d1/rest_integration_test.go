//go:build integration

package d1_test

import (
	"bytes"
	"testing"

	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/d1/d1test"
)

func testDriver(t *testing.T) (*d1.Driver, d1test.Database) {
	t.Helper()
	database := d1test.New(t)
	driver, err := d1.New(database.AccountID, database.ID, database.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	return driver, database
}

func TestRESTBatchRollsBackOnConstraintFailure(t *testing.T) {
	driver, _ := testDriver(t)
	if _, err := driver.Batch(t.Context(), []d1.Statement{
		{SQL: "CREATE TABLE probe (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"},
		{SQL: "INSERT INTO probe VALUES (?, ?)", Params: []any{1, "persisted"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Batch(t.Context(), []d1.Statement{
		{SQL: "INSERT INTO probe VALUES (?, ?)", Params: []any{2, "must roll back"}},
		{SQL: "INSERT INTO probe VALUES (?, ?)", Params: []any{3, nil}},
	}); err == nil || !d1.IsConstraintError(err) {
		t.Fatalf("batch error = %v, want constraint", err)
	}
	result, err := driver.Read(t.Context(), d1.Statement{SQL: "SELECT id FROM probe ORDER BY id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(1) {
		t.Fatalf("failed batch retained rows: %+v", result.Rows)
	}
}

func TestRESTRawPreservesEmptyColumnsAndScalars(t *testing.T) {
	driver, _ := testDriver(t)
	if _, err := driver.Batch(t.Context(), []d1.Statement{{
		SQL: "CREATE TABLE raw_probe (id INTEGER PRIMARY KEY, text TEXT, payload BLOB, optional TEXT)",
	}}); err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, 1, 127, 255}
	if _, err := driver.Batch(t.Context(), []d1.Statement{{
		SQL:    "INSERT INTO raw_probe (id, text, payload, optional) VALUES (?, ?, ?, ?)",
		Params: []any{1, "quote '; and NUL \x00", payload, nil},
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := driver.Read(t.Context(), d1.Statement{
		SQL:    "SELECT id, text, payload, optional FROM raw_probe WHERE id = ?",
		Params: []any{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(result.Columns, []string{"id", "text", "payload", "optional"}) || len(result.Rows) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if got, ok := result.Rows[0][2].([]byte); !ok || !bytes.Equal(got, payload) {
		t.Fatalf("BLOB = %#v", result.Rows[0][2])
	}
	empty, err := driver.Read(t.Context(), d1.Statement{SQL: "SELECT id, text FROM raw_probe WHERE false"})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(empty.Columns, []string{"id", "text"}) || len(empty.Rows) != 0 {
		t.Fatalf("empty result = %+v", empty)
	}
}

func TestRESTNextIDIsAtomicAndDoesNotAdvanceVersion(t *testing.T) {
	driver, _ := testDriver(t)
	before, err := driver.Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, err := driver.NextID(t.Context(), "users")
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.NextID(t.Context(), "users")
	if err != nil {
		t.Fatal(err)
	}
	after, err := driver.Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 2 || after != before {
		t.Fatalf("IDs = %d,%d; versions = %d,%d", first, second, before, after)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
