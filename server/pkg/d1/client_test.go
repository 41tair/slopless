package d1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
)

func TestClientBatchPacksParameters(t *testing.T) {
	t.Parallel()

	type requestBody struct {
		Batch []struct {
			SQL    string `json:"sql"`
			Params []any  `json:"params"`
		} `json:"batch"`
	}
	captured := make(chan requestBody, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/query" {
			t.Errorf("path = %q, want /query", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		var body requestBody
		decoder := json.NewDecoder(request.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil {
			t.Error(err)
		}
		captured <- body
		_, _ = writer.Write([]byte(`{"result":[{"results":[],"success":true,"meta":{"changes":1,"last_row_id":7}}],"success":true,"errors":[]}`))
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	results, err := client.batch(context.Background(), []Statement{{
		SQL:    `INSERT INTO t(a, b, c) VALUES (?, ?, ?)`,
		Params: []any{"value", []byte{0, 255}, int64(42)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Changes != 1 || results[0].LastInsertID != 7 {
		t.Fatalf("result = %+v", results[0])
	}
	body := <-captured
	if len(body.Batch) != 1 || len(body.Batch[0].Params) != 1 {
		t.Fatalf("request = %#v", body)
	}
	if strings.Count(body.Batch[0].SQL, "json_extract(?1") != 6 {
		t.Fatalf("SQL was not JSON-packed: %s", body.Batch[0].SQL)
	}
	packed, ok := body.Batch[0].Params[0].(string)
	if !ok || packed != `["value",{"blob":"00FF"},42]` {
		t.Fatalf("packed params = %#v", body.Batch[0].Params)
	}
}

func TestClientReadPreservesColumnsForEmptyResult(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/raw" {
			t.Errorf("path = %q, want /raw", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{
			"result":[{"results":{"columns":["id","name"],"rows":[]},"success":true,"meta":{}}],
			"success":true,"errors":[]
		}`))
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.read(context.Background(), Statement{SQL: "SELECT id, name FROM users WHERE id = ?", Params: []any{1}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(result.Columns, []string{"id", "name"}) || len(result.Rows) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestDriverQueryUsesOneTargetedRawRequest(t *testing.T) {
	t.Parallel()

	const targetQuery = `SELECT "id", "username" FROM "users" ORDER BY "id" LIMIT 10`
	type requestBody struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}
	var requests atomic.Int32
	captured := make(chan requestBody, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/raw" {
			t.Errorf("path = %q, want /raw", request.URL.Path)
		}
		var body requestBody
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		captured <- body
		_, _ = writer.Write([]byte(`{
			"result":[{"results":{"columns":["id","username"],"rows":[]},"success":true,"meta":{}}],
			"success":true,"errors":[]
		}`))
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	driver := &Driver{client: client}
	var rows entsql.Rows
	if err := driver.Query(context.Background(), targetQuery, nil, &rows); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
	request := <-captured
	if request.SQL != targetQuery {
		t.Fatalf("SQL = %q, want unchanged %q", request.SQL, targetQuery)
	}
	if request.Params == nil || len(request.Params) != 0 {
		t.Fatalf("params = %#v, want []", request.Params)
	}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(columns, []string{"id", "username"}) {
		t.Fatalf("columns = %v", columns)
	}
	if rows.Next() {
		t.Fatal("empty targeted query returned a row")
	}
}

func TestClientErrorsAreRedactedAndClassified(t *testing.T) {
	t.Parallel()

	const secret = "secret-value-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{
			"result":[],"success":false,
			"errors":[{"code":7500,"message":"UNIQUE constraint failed: ` + secret + `"}]
		}`))
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "token-should-not-leak")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.batch(context.Background(), []Statement{{SQL: "INSERT INTO t VALUES (1)"}})
	if err == nil || !IsConstraintError(err) {
		t.Fatalf("error = %v, want constraint error", err)
	}
	for _, sensitive := range []string{secret, "token-should-not-leak", "UNIQUE constraint"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error leaks %q: %v", sensitive, err)
		}
	}
	var failure *apiError
	if !errors.As(err, &failure) || failure.Status != http.StatusBadRequest {
		t.Fatalf("error does not preserve API status: %v", err)
	}
}

func TestClientClassifiesNativeSQLiteConstraintCode(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"result":[],"success":false,"errors":[{"code":19,"message":"statement failed"}]}`))
	}))
	defer server.Close()
	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.batch(context.Background(), []Statement{{SQL: "INSERT INTO t VALUES (1)"}})
	if !IsConstraintError(err) {
		t.Fatalf("error = %v, want SQLite constraint", err)
	}
}

func TestDriverClassifiesOnlyTransientD1FailuresAsUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		status          int
		response        string
		wantUnavailable bool
		wantConstraint  bool
		wantAttempts    int32
	}{
		{name: "busy", status: http.StatusBadRequest, response: `{"result":[],"success":false,"errors":[{"code":5,"message":"failed"}]}`, wantUnavailable: true, wantAttempts: requestAttempts},
		{name: "locked", status: http.StatusBadRequest, response: `{"result":[],"success":false,"errors":[{"code":6,"message":"failed"}]}`, wantUnavailable: true, wantAttempts: requestAttempts},
		{name: "rate limited", status: http.StatusTooManyRequests, response: `{"result":[],"success":false,"errors":[]}`, wantUnavailable: true, wantAttempts: requestAttempts},
		{name: "service unavailable", status: http.StatusServiceUnavailable, response: `{"result":[],"success":false,"errors":[]}`, wantUnavailable: true, wantAttempts: requestAttempts},
		{name: "invalid server JSON", status: http.StatusOK, response: `{`, wantUnavailable: true, wantAttempts: requestAttempts},
		{name: "SQL syntax", status: http.StatusBadRequest, response: `{"result":[],"success":false,"errors":[{"code":1,"message":"syntax error"}]}`, wantAttempts: 1},
		{name: "constraint", status: http.StatusBadRequest, response: `{"result":[],"success":false,"errors":[{"code":19,"message":"failed"}]}`, wantConstraint: true, wantAttempts: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()
			client, err := newClient(server.URL+"/query", "test-token")
			if err != nil {
				t.Fatal(err)
			}
			driver := &Driver{client: client}
			_, err = driver.Read(context.Background(), Statement{SQL: "SELECT 1"})
			if got := errors.Is(err, ErrUnavailable); got != test.wantUnavailable {
				t.Fatalf("ErrUnavailable = %v, want %v; error: %v", got, test.wantUnavailable, err)
			}
			if got := IsConstraintError(err); got != test.wantConstraint {
				t.Fatalf("constraint = %v, want %v; error: %v", got, test.wantConstraint, err)
			}
			if got := attempts.Load(); got != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", got, test.wantAttempts)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestDriverClassifiesNetworkFailureAsUnavailable(t *testing.T) {
	t.Parallel()

	client, err := newClient("http://localhost/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	driver := &Driver{client: client}
	_, err = driver.Read(context.Background(), Statement{SQL: "SELECT 1"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

func TestReadRetriesOnlyTransientFailures(t *testing.T) {
	t.Parallel()

	const success = `{
		"result":[{"results":{"columns":["value"],"rows":[[1]]},"success":true,"meta":{}}],
		"success":true,"errors":[]
	}`
	t.Run("connection reset", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(success))
		}))
		defer server.Close()
		client, err := newClient(server.URL+"/query", "test-token")
		if err != nil {
			t.Fatal(err)
		}
		base := http.DefaultTransport
		var attempts atomic.Int32
		client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return base.RoundTrip(request)
		})
		result, err := (&Driver{client: client}).Read(context.Background(), Statement{SQL: "SELECT 1 AS value"})
		if err != nil {
			t.Fatal(err)
		}
		if attempts.Load() != 2 || len(result.Rows) != 1 || result.Rows[0][0] != int64(1) {
			t.Fatalf("attempts=%d result=%+v", attempts.Load(), result)
		}
	})
	t.Run("service unavailable", func(t *testing.T) {
		t.Parallel()
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			if attempts.Add(1) == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = writer.Write([]byte(`{"result":[],"success":false,"errors":[]}`))
				return
			}
			_, _ = writer.Write([]byte(success))
		}))
		defer server.Close()
		client, err := newClient(server.URL+"/query", "test-token")
		if err != nil {
			t.Fatal(err)
		}
		result, err := (&Driver{client: client}).Read(context.Background(), Statement{SQL: "SELECT 1 AS value"})
		if err != nil {
			t.Fatal(err)
		}
		if attempts.Load() != 2 || len(result.Rows) != 1 || result.Rows[0][0] != int64(1) {
			t.Fatalf("attempts=%d result=%+v", attempts.Load(), result)
		}
	})
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var destinationReached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationReached.Store(true)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL+"/query", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := newClient(origin.URL+"/query", "redirect-secret-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.batch(context.Background(), []Statement{{SQL: "SELECT 1"}})
	if err == nil {
		t.Fatal("redirect response was accepted")
	}
	if destinationReached.Load() {
		t.Fatal("redirect destination received the bearer token")
	}
}

func TestPrepareStatementSupportsMoreThanOneHundredParameters(t *testing.T) {
	t.Parallel()

	params := make([]any, 125)
	placeholders := make([]string, len(params))
	for i := range params {
		params[i] = i
		placeholders[i] = "?"
	}
	prepared, err := prepareStatement(Statement{
		SQL:    "SELECT 'literal ?', " + strings.Join(placeholders, ", ") + " -- comment ?",
		Params: params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Params) != 1 || strings.Count(prepared.SQL, "json_type(?1") != len(params) {
		t.Fatalf("packed statement uses %d expressions and %d params", strings.Count(prepared.SQL, "json_type(?1"), len(prepared.Params))
	}
	if !strings.Contains(prepared.SQL, "'literal ?'") || !strings.Contains(prepared.SQL, "-- comment ?") {
		t.Fatalf("quoted/comment question marks were rewritten: %s", prepared.SQL)
	}
}

func TestPrepareStatementSupportsNamedBasicTypes(t *testing.T) {
	t.Parallel()

	type credentialKind string
	type creditReason string
	type namedInt int
	type namedBytes []byte
	prepared, err := prepareStatement(Statement{
		SQL: "SELECT ?, ?, ?, ?",
		Params: []any{
			credentialKind("token"),
			creditReason("confirmation"),
			namedInt(17),
			namedBytes{0, 127, 255},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Params) != 1 {
		t.Fatalf("params = %#v", prepared.Params)
	}
	if got, want := prepared.Params[0], `["token","confirmation",17,{"blob":"007FFF"}]`; got != want {
		t.Fatalf("packed params = %v, want %s", got, want)
	}
}

func TestBufferedRowsScansTimeAndNullTime(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, time.October, 13, 4, 5, 6, 7, time.UTC)
	rows := newBufferedRows(Result{
		Columns: []string{"created_at", "deleted_at"},
		Rows:    [][]any{{formatSQLiteTime(want), nil}},
	})
	if !rows.Next() {
		t.Fatal("missing row")
	}
	var created time.Time
	var deleted sql.NullTime
	if err := rows.Scan(&created, &deleted); err != nil {
		t.Fatal(err)
	}
	if !created.Equal(want) || deleted.Valid {
		t.Fatalf("created = %v, deleted = %+v", created, deleted)
	}
}

func TestD1ValuesPreserveSafeIntegerPrecision(t *testing.T) {
	t.Parallel()

	value, err := decodeValue(json.RawMessage(`9007199254740991`))
	if err != nil || value != maxSafeInteger {
		t.Fatalf("decode max safe integer = %#v, %v", value, err)
	}
	if _, err := decodeValue(json.RawMessage(`9007199254740992`)); err == nil {
		t.Fatal("unsafe integer was accepted")
	}
	if _, err := prepareStatement(Statement{SQL: "SELECT ?", Params: []any{maxSafeInteger + 1}}); err == nil {
		t.Fatal("unsafe integer parameter was accepted")
	}
}

func TestTimeParameterAndRowRoundTrip(t *testing.T) {
	t.Parallel()

	input := time.Date(2026, time.October, 13, 12, 34, 56, 987654321, time.FixedZone("offset", 8*60*60))
	prepared, err := prepareStatement(Statement{SQL: "SELECT ?", Params: []any{input}})
	if err != nil {
		t.Fatal(err)
	}
	var packed []string
	if err := json.Unmarshal([]byte(prepared.Params[0].(string)), &packed); err != nil {
		t.Fatal(err)
	}
	if len(packed) != 1 {
		t.Fatalf("packed time = %#v", packed)
	}
	var output time.Time
	rows := newBufferedRows(Result{Columns: []string{"value"}, Rows: [][]any{{packed[0]}}})
	if !rows.Next() {
		t.Fatal("missing row")
	}
	if err := rows.Scan(&output); err != nil {
		t.Fatal(err)
	}
	if !output.Equal(input) || output.Location() != time.UTC {
		t.Fatalf("round-trip time = %v, want instant %v in UTC", output, input)
	}
}

func TestNewBindingUsesPrivateEndpointWithoutToken(t *testing.T) {
	t.Parallel()

	driver, err := NewBinding()
	if err != nil {
		t.Fatal(err)
	}
	if got := driver.client.queryEndpoint.String(); got != bindingEndpoint {
		t.Fatalf("endpoint = %q", got)
	}
}

type loseQueryResponseTransport struct {
	base http.RoundTripper
	lost atomic.Bool
}

func (t *loseQueryResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && request.URL.Path == "/query" && t.lost.CompareAndSwap(false, true) {
		_ = response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, err
}

func TestWriteResolvesLostResponseWithStoredResult(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/query":
			_, _ = writer.Write([]byte(`{
				"result":[
					{"results":[],"success":true,"meta":{"changes":1,"last_row_id":12}},
					{"results":[],"success":true,"meta":{"changes":1}},
					{"results":[],"success":true,"meta":{"changes":1}},
					{"results":[],"success":true,"meta":{"changes":0}}
				],"success":true,"errors":[]
			}`))
		case "/raw":
			_, _ = writer.Write([]byte(`{
				"result":[{"results":{"columns":["version","changes","last_insert_id","returning_column"],"rows":[[1,1,12,""]]},"success":true,"meta":{}}],
				"success":true,"errors":[]
			}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	transport := &loseQueryResponseTransport{base: http.DefaultTransport}
	client.http.Transport = transport
	client.schemaReady = true
	driver := &Driver{client: client}
	var got sql.Result
	if err := driver.Exec(context.Background(), "INSERT INTO users(name) VALUES (?)", []any{"alice"}, &got); err != nil {
		t.Fatal(err)
	}
	lastID, _ := got.LastInsertId()
	changes, _ := got.RowsAffected()
	if !transport.lost.Load() || lastID != 12 || changes != 1 {
		t.Fatalf("lost=%v, last ID=%d, changes=%d", transport.lost.Load(), lastID, changes)
	}
}

func TestBatchDoesNotRetryUnknownManagementWrite(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"result":[],"success":false,"errors":[{"code":5,"message":"SQLITE_BUSY"}]}`))
	}))
	defer server.Close()
	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	driver := &Driver{client: client}
	_, err = driver.Batch(context.Background(), []Statement{{SQL: "UPDATE users SET name = 'x'"}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("management batch sent %d times, want exactly once", got)
	}
}

func TestWriteRetryUsesOutcomeToAvoidDuplicateMutation(t *testing.T) {
	t.Parallel()

	var queryRequests atomic.Int32
	var outcomeRequests atomic.Int32
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/query":
			attempt := queryRequests.Add(1)
			if attempt == 1 {
				mutations.Add(1)
				_, _ = writer.Write([]byte(`{
					"result":[
						{"results":[],"success":true,"meta":{"changes":1,"last_row_id":21}},
						{"results":[],"success":true,"meta":{"changes":1}},
						{"results":[],"success":true,"meta":{"changes":1}},
						{"results":[],"success":true,"meta":{"changes":0}}
					],"success":true,"errors":[]
				}`))
				return
			}
			// Replaying the same txid makes the outcome primary key fail, so D1
			// rolls back the replayed mutation atomically.
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"result":[],"success":false,"errors":[{"code":19,"message":"constraint"}]}`))
		case "/raw":
			if outcomeRequests.Add(1) <= requestAttempts {
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = writer.Write([]byte(`{"result":[],"success":false,"errors":[{"code":5,"message":"SQLITE_BUSY"}]}`))
				return
			}
			_, _ = writer.Write([]byte(`{
				"result":[{"results":{"columns":["version","changes","last_insert_id","returning_column"],"rows":[[1,1,21,""]]},"success":true,"meta":{}}],
				"success":true,"errors":[]
			}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	transport := &loseQueryResponseTransport{base: http.DefaultTransport}
	client.http.Transport = transport
	client.schemaReady = true
	driver := &Driver{client: client}
	var got sql.Result
	if err := driver.Exec(context.Background(), "INSERT INTO users(name) VALUES (?)", []any{"alice"}, &got); err != nil {
		t.Fatal(err)
	}
	if mutations.Load() != 1 || queryRequests.Load() != 2 || outcomeRequests.Load() != requestAttempts+1 {
		t.Fatalf("mutations=%d query requests=%d outcome requests=%d", mutations.Load(), queryRequests.Load(), outcomeRequests.Load())
	}
	lastID, _ := got.LastInsertId()
	if lastID != 21 {
		t.Fatalf("recovered last ID = %d, want 21", lastID)
	}
}

func TestCommitRetryUsesOutcomeToAvoidDuplicateCASMutation(t *testing.T) {
	t.Parallel()

	var queryRequests atomic.Int32
	var outcomeRequests atomic.Int32
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/query":
			attempt := queryRequests.Add(1)
			if attempt == 1 {
				mutations.Add(1)
				_, _ = writer.Write([]byte(`{
					"result":[
						{"results":[],"success":true,"meta":{}},
						{"results":[],"success":true,"meta":{"changes":1}},
						{"results":[],"success":true,"meta":{"changes":1}},
						{"results":[],"success":true,"meta":{"changes":1}},
						{"results":[],"success":true,"meta":{"changes":0}}
					],"success":true,"errors":[]
				}`))
				return
			}
			// The replayed CAS sees version 8 rather than 7 and aborts the
			// entire D1 batch before the business mutation can commit again.
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"result":[],"success":false,"errors":[{"code":19,"message":"constraint"}]}`))
		case "/raw":
			if outcomeRequests.Add(1) <= requestAttempts {
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = writer.Write([]byte(`{"result":[],"success":false,"errors":[{"code":5,"message":"SQLITE_BUSY"}]}`))
				return
			}
			_, _ = writer.Write([]byte(`{
				"result":[{"results":{"columns":["version","changes","last_insert_id","returning_column"],"rows":[[8,0,0,""]]},"success":true,"meta":{}}],
				"success":true,"errors":[]
			}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	transport := &loseQueryResponseTransport{base: http.DefaultTransport}
	client.http.Transport = transport
	client.schemaReady = true
	driver := &Driver{client: client}
	if err := driver.Commit(context.Background(), 7, []Statement{{SQL: "UPDATE users SET credits = credits + 1 WHERE id = 1"}}); err != nil {
		t.Fatal(err)
	}
	if mutations.Load() != 1 || queryRequests.Load() != 2 || outcomeRequests.Load() != requestAttempts+1 {
		t.Fatalf("mutations=%d query requests=%d outcome requests=%d", mutations.Load(), queryRequests.Load(), outcomeRequests.Load())
	}
}
