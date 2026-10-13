//go:build integration

package d1

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/41tair/slopless/pkg/d1/d1test"
)

func TestRESTBatchRollsBackOnConstraintFailure(t *testing.T) {
	database := d1test.New(t)
	client, err := newClient(database.Endpoint, database.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.query(t.Context(), []statement{
		{SQL: "CREATE TABLE probe (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"},
		{SQL: "INSERT INTO probe VALUES (?, ?)", Params: []any{1, "persisted"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.query(t.Context(), []statement{
		{SQL: "INSERT INTO probe VALUES (?, ?)", Params: []any{2, "must roll back"}},
		{SQL: "INSERT INTO probe VALUES (?, ?)", Params: []any{3, nil}},
	}); err == nil {
		t.Fatal("batch with a NOT NULL violation succeeded")
	}
	results, err := client.query(t.Context(), []statement{{SQL: "SELECT id FROM probe ORDER BY id"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results[0].Results) != 1 {
		t.Fatalf("failed batch retained rows: %v", results[0].Results)
	}
	var id int
	if err := json.Unmarshal(results[0].Results[0]["id"], &id); err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("persisted row ID = %d, want 1", id)
	}
}

type loseResponseTransport struct {
	transport http.RoundTripper
	lost      bool
}

func (t *loseResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.transport.RoundTrip(request)
	if err == nil && !t.lost && response.StatusCode == http.StatusOK {
		t.lost = true
		_ = response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, err
}

func TestRESTCommitResolvesLostResponse(t *testing.T) {
	database := d1test.New(t)
	client, err := newClient(database.Endpoint, database.Token)
	if err != nil {
		t.Fatal(err)
	}
	before, err := client.snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	transport := &loseResponseTransport{transport: http.DefaultTransport}
	client.http.Transport = transport
	if err := client.commit(t.Context(), commitRequest{
		Version: before.Version, TxID: "lost-response",
		DDL: []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)"},
	}); err != nil {
		t.Fatal(err)
	}
	if !transport.lost {
		t.Fatal("test did not lose the actual D1 commit response")
	}
	after, err := client.snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version+1 || len(after.Tables) != 1 {
		t.Fatal("lost response caused a duplicate or missing commit")
	}
}

func TestRESTSnapshotAndCommitPreserveScalars(t *testing.T) {
	database := d1test.New(t)
	client, err := newClient(database.Endpoint, database.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.query(t.Context(), []statement{{
		SQL: "CREATE TABLE users (id INTEGER PRIMARY KEY, text TEXT, payload BLOB, number INTEGER, optional TEXT)",
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := client.snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	text := "quote '; and embedded \x00 byte"
	payload := []byte{0, 1, 127, 255}
	number := maxSafeInteger - 1
	if err := client.commit(t.Context(), commitRequest{
		Version: before.Version,
		TxID:    "scalar-round-trip",
		Changes: []change{{
			Table:      "users",
			Columns:    []string{"id", "text", "payload", "number", "optional"},
			PrimaryKey: []string{"id"},
			Rows: [][]scalar{{
				{Value: int64(1)}, {Value: text}, {Value: payload}, {Value: number}, {Value: nil},
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := client.snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version+1 || len(after.Tables) != 1 || len(after.Tables[0].Rows) != 1 {
		t.Fatal("commit did not persist one row and advance the revision")
	}
	row := after.Tables[0].Rows[0]
	if row[0].Value != int64(1) || row[1].Value != text || row[3].Value != number || row[4].Value != nil {
		t.Fatalf("scalar values changed during REST round trip: %v", row)
	}
	gotPayload, ok := row[2].Value.([]byte)
	if !ok || !bytes.Equal(gotPayload, payload) {
		t.Fatalf("BLOB = %v, want %v", row[2].Value, payload)
	}
}
