package d1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientQuerySendsD1Batch(t *testing.T) {
	t.Parallel()

	type capturedRequest struct {
		method        string
		path          string
		authorization string
		contentType   string
		accept        string
		body          struct {
			Batch []struct {
				SQL    string `json:"sql"`
				Params []any  `json:"params"`
			} `json:"batch"`
		}
		err error
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got := capturedRequest{
			method:        request.Method,
			path:          request.URL.Path,
			authorization: request.Header.Get("Authorization"),
			contentType:   request.Header.Get("Content-Type"),
			accept:        request.Header.Get("Accept"),
		}
		decoder := json.NewDecoder(request.Body)
		decoder.UseNumber()
		got.err = decoder.Decode(&got.body)
		captured <- got

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"result": [
				{"results": [{"value": 1}], "success": true},
				{"results": [], "success": true}
			],
			"success": true,
			"errors": []
		}`))
	}))
	defer server.Close()

	const token = "d1-test-token"
	client, err := newClient(server.URL+"/client/v4/accounts/account/d1/database/database/query", token)
	if err != nil {
		t.Fatal(err)
	}
	statements := []statement{
		{SQL: "SELECT ?, ?, ?, ?", Params: []any{"value", int64(42), true, nil}},
		{SQL: "SELECT 2"},
	}
	results, err := client.query(context.Background(), statements)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("query returned %d results, want 2", len(results))
	}

	request := <-captured
	if request.err != nil {
		t.Fatalf("decode request: %v", request.err)
	}
	if request.method != http.MethodPost {
		t.Errorf("method = %q, want POST", request.method)
	}
	if request.path != "/client/v4/accounts/account/d1/database/database/query" {
		t.Errorf("path = %q", request.path)
	}
	if request.authorization != "Bearer "+token {
		t.Errorf("Authorization = %q", request.authorization)
	}
	if request.contentType != "application/json" {
		t.Errorf("Content-Type = %q", request.contentType)
	}
	if request.accept != "application/json" {
		t.Errorf("Accept = %q", request.accept)
	}
	if len(request.body.Batch) != 2 {
		t.Fatalf("batch contains %d statements, want 2", len(request.body.Batch))
	}
	if request.body.Batch[0].SQL != statements[0].SQL {
		t.Errorf("first SQL = %q", request.body.Batch[0].SQL)
	}
	params := request.body.Batch[0].Params
	if len(params) != 4 {
		t.Fatalf("first statement contains %d params, want 4", len(params))
	}
	if value, ok := params[0].(string); !ok || value != "value" {
		t.Errorf("string param = %#v (%T)", params[0], params[0])
	}
	if value, ok := params[1].(json.Number); !ok || value.String() != "42" {
		t.Errorf("integer param = %#v (%T)", params[1], params[1])
	}
	if value, ok := params[2].(bool); !ok || !value {
		t.Errorf("boolean param = %#v (%T)", params[2], params[2])
	}
	if params[3] != nil {
		t.Errorf("null param = %#v (%T)", params[3], params[3])
	}
	if request.body.Batch[1].Params == nil || len(request.body.Batch[1].Params) != 0 {
		t.Errorf("nil params encoded as %#v, want []", request.body.Batch[1].Params)
	}
	if statements[1].Params != nil {
		t.Fatal("query mutated the caller's nil Params slice")
	}
}

func TestClientQueryRedactsD1Errors(t *testing.T) {
	t.Parallel()

	const (
		token  = "token-must-not-leak"
		secret = "SELECT secret_value FROM vault"
	)
	tests := []struct {
		name       string
		statusCode int
		response   string
		wantStatus int
		wantCode   int
	}{
		{
			name:       "envelope failure",
			statusCode: http.StatusForbidden,
			response: `{
				"result": [],
				"success": false,
				"errors": [{"code": 10000, "message": "` + secret + `; ` + token + `"}]
			}`,
			wantStatus: http.StatusForbidden,
			wantCode:   10000,
		},
		{
			name:       "statement failure",
			statusCode: http.StatusOK,
			response: `{
				"result": [{"results": [], "success": false, "error": "` + secret + `; ` + token + `"}],
				"success": true,
				"errors": []
			}`,
			wantStatus: http.StatusOK,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()

			client, err := newClient(server.URL+"/query", token)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.query(context.Background(), []statement{{SQL: secret}})
			if err == nil {
				t.Fatal("query succeeded, want D1 API error")
			}
			var failure *apiError
			if !errors.As(err, &failure) {
				t.Fatalf("error type = %T, want *apiError", err)
			}
			if failure.Status != test.wantStatus {
				t.Errorf("status = %d, want %d", failure.Status, test.wantStatus)
			}
			if test.wantCode != 0 && (len(failure.Codes) != 1 || failure.Codes[0] != test.wantCode) {
				t.Errorf("codes = %v, want [%d]", failure.Codes, test.wantCode)
			}
			for _, sensitive := range []string{secret, token, "secret_value"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Errorf("error exposes sensitive value %q: %v", sensitive, err)
				}
			}
		})
	}
}

func TestClientQueryRejectsUnexpectedResultCount(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"result": [], "success": true, "errors": []}`))
	}))
	defer server.Close()

	client, err := newClient(server.URL+"/query", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.query(context.Background(), []statement{{SQL: "SELECT 1"}})
	if err == nil || !strings.Contains(err.Error(), "returned 0 results for 1 statements") {
		t.Fatalf("query error = %v", err)
	}
}

func TestClientQueryDoesNotFollowRedirects(t *testing.T) {
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
	_, err = client.query(context.Background(), []statement{{SQL: "SELECT 1"}})
	if err == nil {
		t.Fatal("query followed redirect or accepted the redirect response")
	}
	if destinationReached.Load() {
		t.Fatal("redirect destination received the request and its bearer token")
	}
}
