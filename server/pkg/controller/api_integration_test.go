//go:build integration

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/41tair/slopless/pkg/config"
	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/d1/d1test"
	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
	"github.com/41tair/slopless/pkg/service"
	"github.com/google/uuid"
)

type testRequest struct {
	method string
	path   string
	body   string
	token  string
	key    string
	cookie *http.Cookie
	origin string
}

type testHandler struct {
	http.Handler
	store      *data.Store
	service    *service.Service
	accountID  string
	databaseID string
	token      string
}

func perform(handler http.Handler, input testRequest) *httptest.ResponseRecorder {
	method := input.method
	if method == "" {
		method = "POST"
	}
	req := httptest.NewRequest(method, input.path, strings.NewReader(input.body))
	req.Header.Set("Content-Type", "application/json")
	if input.token != "" {
		req.Header.Set("Authorization", "Bearer "+input.token)
	}
	if input.key != "" {
		req.Header.Set("Idempotency-Key", input.key)
	}
	if input.cookie != nil {
		req.AddCookie(input.cookie)
	}
	if input.origin != "" {
		req.Header.Set("Origin", input.origin)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func requireStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store header")
	}
	if _, err := uuid.Parse(response.Header().Get("X-Request-Id")); err != nil {
		t.Fatalf("invalid request ID: %v", err)
	}
}

func fixture(t *testing.T) (*service.Service, http.Handler) {
	t.Helper()
	database := d1test.New(t)

	store, err := data.Open(
		t.Context(), database.AccountID, database.ID, database.Token,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := service.New(store, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	return svc, &testHandler{
		Handler: NewRouter(svc, true), store: store, service: svc,
		accountID: database.AccountID, databaseID: database.ID, token: database.Token,
	}
}

func seedInvitation(t *testing.T, handler http.Handler) string {
	t.Helper()
	fixture, ok := handler.(*testHandler)
	if !ok {
		t.Fatal("test handler has no seed store")
	}
	invites, err := fixture.service.BootstrapInvitations(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return invites[0].Code
}

func registerLogin(t *testing.T, handler http.Handler, username string) *http.Cookie {
	t.Helper()
	body := fmt.Sprintf(
		`{"username":%q,"password":"a-test-password","invitationCode":%q}`,
		username, seedInvitation(t, handler),
	)
	return registerWithInvitation(t, handler, body)
}

func registerWithInvitation(t *testing.T, handler http.Handler, body string) *http.Cookie {
	t.Helper()
	response := perform(handler, testRequest{path: "/api/v1/register", body: body})
	requireStatus(t, response, 201)
	var input struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		t.Fatal(err)
	}
	loginBody := fmt.Sprintf(`{"username":%q,"password":"a-test-password"}`, input.Username)
	response = perform(handler, testRequest{path: "/api/v1/login", body: loginBody})
	requireStatus(t, response, 200)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("invalid session cookie: %v", cookies)
	}
	return cookies[0]
}

type issuedToken struct {
	ID     int    `json:"id"`
	Secret string `json:"token"`
}

func issueToken(t *testing.T, handler http.Handler, cookie *http.Cookie) issuedToken {
	t.Helper()
	response := perform(handler, testRequest{
		path: "/api/v1/tokens", cookie: cookie,
		body: `{"name":"extension"}`,
	})
	requireStatus(t, response, 201)
	if strings.Contains(response.Body.String(), `"scopes"`) {
		t.Fatalf("token creation returned deleted scopes field: %s", response.Body.String())
	}
	var result struct {
		Data issuedToken `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Secret == "" {
		t.Fatal("missing token secret")
	}
	return result.Data
}

func rateUnits(t *testing.T, store *data.Store, policy string, subject int) int {
	t.Helper()
	// These assertions measure persisted charges, including expired events. Real
	// REST round trips may cross a rolling window while checking replay behavior.
	// Admission and expiry are verified separately with fresh budget fixtures.
	events, err := store.RateEvents(t.Context(), policy, fmt.Sprint(subject), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	units := 0
	for _, event := range events {
		units += event.Units
	}
	return units
}

func requireRateLimited(
	t *testing.T,
	response *httptest.ResponseRecorder,
	policy, unit string,
	limit int,
) {
	t.Helper()
	requireStatus(t, response, http.StatusTooManyRequests)
	var body struct {
		Code              string           `json:"code"`
		RetryAfterSeconds int              `json:"retryAfterSeconds"`
		RateLimit         model.RatePolicy `json:"rateLimit"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "rate_limited" || body.RetryAfterSeconds < 1 ||
		response.Header().Get("Retry-After") != fmt.Sprint(body.RetryAfterSeconds) ||
		body.RateLimit.Policy != policy || body.RateLimit.Unit != unit ||
		body.RateLimit.Limit != limit || body.RateLimit.WindowSeconds != 60 {
		t.Fatalf("incorrect rate-limit problem: %v %s", response.Header(), response.Body.String())
	}
}

func TestUserAndTokenLifecycle(t *testing.T) {
	t.Parallel()
	_, handler := fixture(t)
	cookie := registerLogin(t, handler, "Alice")
	token := issueToken(t, handler, cookie)
	other := registerLogin(t, handler, "bob")
	queryBody := `{"accounts":[{"platform":"github","username":"example"}]}`
	duplicate := fmt.Sprintf(
		`{"username":" ALICE ","password":"another-password","invitationCode":%q}`,
		seedInvitation(t, handler),
	)
	tests := []struct {
		name     string
		request  testRequest
		status   int
		contains string
	}{
		{name: "duplicate registration", request: testRequest{path: "/api/v1/register", body: duplicate}, status: 409},
		{name: "wrong password", request: testRequest{path: "/api/v1/login", body: `{"username":"alice","password":"wrong-password"}`}, status: 401},
		{name: "unknown user", request: testRequest{path: "/api/v1/login", body: `{"username":"missing","password":"wrong-password"}`}, status: 401},
		{name: "user info", request: testRequest{method: "GET", path: "/api/v1/user", cookie: cookie}, status: 200},
		{name: "tokens require session", request: testRequest{path: "/api/v1/tokens", token: token.Secret, body: `{}`}, status: 401},
		{name: "bearer cannot manage user", request: testRequest{method: "GET", path: "/api/v1/user", token: token.Secret}, status: 401},
		{name: "session cannot query", request: testRequest{path: "/openapi/v1/accounts/query", cookie: cookie, body: queryBody}, status: 401},
		{name: "session secret is not API token", request: testRequest{path: "/openapi/v1/accounts/query", token: cookie.Value, body: queryBody}, status: 401},
		{name: "other user cannot delete", request: testRequest{method: "DELETE", path: fmt.Sprintf("/api/v1/tokens/%d", token.ID), cookie: other}, status: 404},
		{name: "cross origin mutation", request: testRequest{method: "DELETE", path: fmt.Sprintf("/api/v1/tokens/%d", token.ID), cookie: cookie, origin: "https://evil.example"}, status: 403},
		{name: "deleted scopes field", request: testRequest{path: "/api/v1/tokens", cookie: cookie, body: `{"name":"bad","scopes":["accounts:read"]}`}, status: 422, contains: "unknown_field"},
		{name: "zero token expiration", request: testRequest{path: "/api/v1/tokens", cookie: cookie, body: `{"name":"old","expiresAt":"0001-01-01T00:00:00Z"}`}, status: 422},
		{name: "expired token creation", request: testRequest{path: "/api/v1/tokens", cookie: cookie, body: `{"name":"old","expiresAt":"2000-01-01T00:00:00Z"}`}, status: 422},
		{name: "query succeeds", request: testRequest{path: "/openapi/v1/accounts/query", token: token.Secret, body: queryBody}, status: 200},
		{name: "same token marks", request: testRequest{path: "/openapi/v1/accounts/mark", token: token.Secret, key: "lifecycle-mark-key", body: `{"accounts":[{"platform":"github","username":"example"}],"marked":true}`}, status: 200},
		{name: "method not allowed", request: testRequest{method: "PUT", path: "/api/v1/tokens", cookie: cookie}, status: 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := perform(handler, tt.request)
			requireStatus(t, response, tt.status)
			if tt.contains != "" && !strings.Contains(response.Body.String(), tt.contains) {
				t.Fatalf("response does not contain %q: %s", tt.contains, response.Body.String())
			}
		})
	}
	response := perform(handler, testRequest{method: "GET", path: "/api/v1/tokens", cookie: cookie})
	requireStatus(t, response, 200)
	if strings.Contains(response.Body.String(), token.Secret) || strings.Contains(response.Body.String(), "hash") ||
		strings.Contains(response.Body.String(), `"scopes"`) {
		t.Fatalf("token list disclosed removed or secret fields: %s", response.Body.String())
	}
	response = perform(handler, testRequest{method: "DELETE", path: fmt.Sprintf("/api/v1/tokens/%d", token.ID), cookie: cookie})
	requireStatus(t, response, 204)
	response = perform(handler, testRequest{path: "/openapi/v1/accounts/query", token: token.Secret, body: queryBody})
	requireStatus(t, response, 401)
	if !strings.Contains(response.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Fatal("missing bearer challenge")
	}
	requireStatus(t, perform(handler, testRequest{path: "/api/v1/logout", cookie: cookie}), 204)
	requireStatus(t, perform(handler, testRequest{method: "GET", path: "/api/v1/user", cookie: cookie}), 401)
}

func TestOpenAPIRequestAdmissionOrderingAndSharing(t *testing.T) {
	t.Parallel()
	svc, handler := fixture(t)
	cookie := registerLogin(t, handler, "alice")
	first := issueToken(t, handler, cookie)
	second := issueToken(t, handler, cookie)
	third := issueToken(t, handler, cookie)
	identity, err := svc.AuthenticateAPI(t.Context(), first.Secret)
	if err != nil {
		t.Fatal(err)
	}
	testServer := handler.(*testHandler)
	requireStatus(t, perform(handler, testRequest{
		path: "/openapi/v1/accounts/query", token: first.Secret, body: "{",
	}), http.StatusBadRequest)
	if got := rateUnits(t, testServer.store, "query.token.requests.minute", first.ID); got != 1 {
		t.Fatalf("invalid query charged %d token requests, want 1", got)
	}
	if got := rateUnits(t, testServer.store, "query.user.requests.minute", identity.UserID); got != 1 {
		t.Fatalf("invalid query charged %d user requests, want 1", got)
	}
	if got := rateUnits(t, testServer.store, "query.user.accounts.minute", identity.UserID); got != 0 {
		t.Fatalf("invalid query charged %d account units", got)
	}
	invalid := testRequest{
		path: "/openapi/v1/accounts/mark", key: "invalid-request-key", body: "{",
	}
	unauthenticated := perform(handler, invalid)
	requireStatus(t, unauthenticated, http.StatusUnauthorized)
	if unauthenticated.Header().Get("WWW-Authenticate") != `Bearer realm="slopless"` {
		t.Fatal("missing bearer authentication challenge")
	}
	if got := rateUnits(t, testServer.store, "mark.user.requests.minute", identity.UserID); got != 0 {
		t.Fatalf("failed authentication charged user requests: %d", got)
	}
	if got := rateUnits(t, testServer.store, "mark.token.requests.minute", first.ID); got != 0 {
		t.Fatalf("failed authentication charged token requests: %d", got)
	}

	chargedInvalid := []struct {
		name    string
		request testRequest
		status  int
	}{
		{name: "JSON", request: testRequest{
			path: invalid.path, token: first.Secret, key: invalid.key, body: invalid.body,
		}, status: http.StatusBadRequest},
		{name: "idempotency key", request: testRequest{
			path: invalid.path, token: first.Secret, key: "short",
			body: `{"accounts":[{"platform":"github","username":"example"}],"marked":true}`,
		}, status: http.StatusBadRequest},
		{name: "body", request: testRequest{
			path: invalid.path, token: first.Secret, key: invalid.key,
			body: `{"accounts":[],"marked":true}`,
		}, status: http.StatusUnprocessableEntity},
	}
	for _, tt := range chargedInvalid {
		t.Run(tt.name, func(t *testing.T) {
			requireStatus(t, perform(handler, tt.request), tt.status)
		})
	}
	invalid.token = first.Secret
	if got := rateUnits(t, testServer.store, "mark.token.requests.minute", first.ID); got != 3 {
		t.Fatalf("invalid authenticated requests charged %d token units, want 3", got)
	}
	if got := rateUnits(t, testServer.store, "mark.user.requests.minute", identity.UserID); got != 3 {
		t.Fatalf("invalid authenticated requests charged %d user units, want 3", got)
	}
	if got := rateUnits(t, testServer.store, "mark.user.accounts.minute", identity.UserID); got != 0 {
		t.Fatalf("invalid requests charged %d account units", got)
	}
	// The HTTP requests above prove charging and ordering. Seed the rest of the
	// boundary in real D1 so REST latency cannot outlast the rolling window.
	driver, err := d1.New(testServer.accountID, testServer.databaseID, testServer.token)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	if err := driver.Exec(t.Context(), `UPDATE "rate_events" SET "expires_at" = ?
		WHERE "policy" IN ('mark.token.requests.minute', 'mark.user.requests.minute')`,
		[]any{time.Now().Add(time.Minute)}, nil); err != nil {
		t.Fatal(err)
	}
	seedBudget := func(store *data.Store, tokenID, units int) {
		t.Helper()
		err := store.WithTx(t.Context(), func(tx *data.Store) error {
			for _, event := range []data.RateEvent{
				{Policy: "mark.token.requests.minute", Subject: fmt.Sprint(tokenID), Units: units, ExpiresAt: time.Now().Add(time.Minute)},
				{Policy: "mark.user.requests.minute", Subject: fmt.Sprint(identity.UserID), Units: units, ExpiresAt: time.Now().Add(time.Minute)},
			} {
				if err := tx.CreateRateEvent(t.Context(), event); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	seedBudget(testServer.store, first.ID, 7)
	blocked := perform(handler, testRequest{
		path: "/openapi/v1/accounts/mark", token: first.Secret, key: "blocked-request-key",
		body: `{"accounts":[{"platform":"github","username":"blocked"}],"marked":true}`,
	})
	requireRateLimited(t, blocked, "mark.token.requests.minute", "requests", 10)
	if got := rateUnits(t, testServer.store, "mark.user.accounts.minute", identity.UserID); got != 0 {
		t.Fatalf("request rejection charged %d account units", got)
	}

	secondStore, err := data.Open(
		t.Context(), testServer.accountID, testServer.databaseID, testServer.token,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondStore.Close() })
	secondService, err := service.New(secondStore, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	secondHandler := NewRouter(secondService, true)
	invalid.token = second.Secret
	requireStatus(t, perform(secondHandler, invalid), http.StatusBadRequest)
	seedBudget(secondStore, second.ID, 9)
	if err := secondStore.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := data.Open(
		t.Context(), testServer.accountID, testServer.databaseID, testServer.token,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedStore.Close() })
	reopenedService, err := service.New(reopenedStore, config.DefaultCredits())
	if err != nil {
		t.Fatal(err)
	}
	blocked = perform(NewRouter(reopenedService, true), testRequest{
		path: "/openapi/v1/accounts/mark", token: third.Secret, key: "shared-user-limit-key",
		body: `{"accounts":[{"platform":"github","username":"blocked"}],"marked":true}`,
	})
	requireRateLimited(t, blocked, "mark.user.requests.minute", "requests", 20)
	if got := rateUnits(t, reopenedStore, "mark.user.requests.minute", identity.UserID); got != 20 {
		t.Fatalf("shared user request units = %d, want 20", got)
	}
	if got := rateUnits(t, reopenedStore, "mark.token.requests.minute", third.ID); got != 0 {
		t.Fatalf("rejected shared request partially charged token: %d", got)
	}
}

func TestOpenAPIAccountRejectionCommitsRequestOnly(t *testing.T) {
	t.Parallel()
	svc, handler := fixture(t)
	token := issueToken(t, handler, registerLogin(t, handler, "alice"))
	identity, err := svc.AuthenticateAPI(t.Context(), token.Secret)
	if err != nil {
		t.Fatal(err)
	}
	testServer := handler.(*testHandler)
	if err := testServer.store.CreateRateEvent(t.Context(), data.RateEvent{
		Policy: "mark.user.accounts.minute", Subject: fmt.Sprint(identity.UserID),
		Units: 200, ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	account := model.Account{Platform: "github", Username: "example"}
	response := perform(handler, testRequest{
		path: "/openapi/v1/accounts/mark", token: token.Secret, key: "account-limit-key",
		body: `{"accounts":[{"platform":"github","username":"example"}],"marked":true}`,
	})
	requireRateLimited(t, response, "mark.user.accounts.minute", "accounts", 200)
	if got := rateUnits(t, testServer.store, "mark.token.requests.minute", token.ID); got != 1 {
		t.Fatalf("account rejection charged %d token requests, want 1", got)
	}
	if got := rateUnits(t, testServer.store, "mark.user.requests.minute", identity.UserID); got != 1 {
		t.Fatalf("account rejection charged %d user requests, want 1", got)
	}
	if got := rateUnits(t, testServer.store, "mark.user.accounts.minute", identity.UserID); got != 200 {
		t.Fatalf("account rejection partially charged minute units: %d", got)
	}
	if got := rateUnits(t, testServer.store, "mark.user.accounts.day", identity.UserID); got != 0 {
		t.Fatalf("account rejection partially charged day units: %d", got)
	}
	states, err := testServer.store.Snapshot(t.Context(), identity.UserID, []model.Account{account})
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Marked || states[0].MarkedByMe || states[0].MarkCount != 0 {
		t.Fatalf("account rejection partially applied mark: %+v", states[0])
	}
}

func accountStates(t *testing.T, response *httptest.ResponseRecorder) []model.AccountState {
	t.Helper()
	requireStatus(t, response, 200)
	var body struct {
		Data []model.AccountState `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

func TestAccountMarkingAndReplay(t *testing.T) {
	t.Parallel()
	svc, handler := fixture(t)
	alice := registerLogin(t, handler, "alice")
	token := issueToken(t, handler, alice)
	secondToken := issueToken(t, handler, alice)
	bob := issueToken(t, handler, registerLogin(t, handler, "bob"))
	identity, err := svc.AuthenticateAPI(t.Context(), secondToken.Secret)
	if err != nil {
		t.Fatal(err)
	}
	testServer := handler.(*testHandler)
	accounts := `[{"platform":"github","username":" Example "},{"platform":"github","username":"example"},{"platform":"github","username":"Build[bot]"}]`
	queryBody := `{"accounts":` + accounts + `}`
	markBody := `{"accounts":` + accounts + `,"marked":true}`
	response := perform(handler, testRequest{path: "/openapi/v1/accounts/query", token: token.Secret, body: queryBody})
	states := accountStates(t, response)
	if len(states) != 2 || states[0].Username != "example" || states[1].Username != "build[bot]" || states[0].Marked {
		t.Fatalf("bad normalized query: %+v", states)
	}
	response = perform(handler, testRequest{path: "/openapi/v1/accounts/mark", token: token.Secret, key: "first-mark-key-123", body: markBody})
	original := accountStates(t, response)
	if original[0].MarkCount != 1 || !original[0].MarkedByMe || response.Header().Get("Idempotency-Replayed") != "false" {
		t.Fatalf("bad mark: %+v", original)
	}
	requestID := response.Header().Get("X-Request-Id")
	response = perform(handler, testRequest{path: "/openapi/v1/accounts/mark", token: secondToken.Secret, key: "second-mark-key-123", body: markBody})
	if states := accountStates(t, response); states[0].MarkCount != 1 {
		t.Fatalf("counted tokens: %+v", states)
	}
	response = perform(handler, testRequest{path: "/openapi/v1/accounts/mark", token: bob.Secret, key: "first-mark-key-123", body: markBody})
	if states := accountStates(t, response); states[0].MarkCount != 2 {
		t.Fatalf("did not count users: %+v", states)
	}
	response = perform(handler, testRequest{
		path: "/openapi/v1/accounts/mark", token: token.Secret, key: "remove-mark-key-123",
		body: `{"accounts":` + accounts + `,"marked":false}`,
	})
	states = accountStates(t, response)
	if states[0].MarkCount != 1 || !states[0].Marked || states[0].MarkedByMe {
		t.Fatalf("bad unmark: %+v", states)
	}
	minuteUnits := rateUnits(t, testServer.store, "mark.user.accounts.minute", identity.UserID)
	dayUnits := rateUnits(t, testServer.store, "mark.user.accounts.day", identity.UserID)
	response = perform(handler, testRequest{
		path: "/openapi/v1/accounts/mark", token: secondToken.Secret, key: "first-mark-key-123",
		body: `{"accounts":[{"platform":"github","username":"example"},{"platform":"github","username":"build[bot]"}],"marked":true}`,
	})
	if replay := accountStates(t, response); !reflect.DeepEqual(replay, original) {
		t.Fatalf("changed replay: %+v", replay)
	}
	if response.Header().Get("Idempotency-Replayed") != "true" || response.Header().Get("X-Request-Id") == requestID {
		t.Fatal("incorrect replay headers")
	}
	if got := rateUnits(t, testServer.store, "mark.user.accounts.minute", identity.UserID); got != minuteUnits+2 {
		t.Fatalf("matching replay charged %d minute account units, want %d", got-minuteUnits, 2)
	}
	if got := rateUnits(t, testServer.store, "mark.user.accounts.day", identity.UserID); got != dayUnits+2 {
		t.Fatalf("matching replay charged %d daily account units, want %d", got-dayUnits, 2)
	}
	response = perform(handler, testRequest{path: "/openapi/v1/accounts/query", token: token.Secret, body: queryBody})
	if states := accountStates(t, response); states[0].MarkedByMe {
		t.Fatal("replay re-applied mark")
	}
	response = perform(handler, testRequest{
		path: "/openapi/v1/accounts/mark", token: token.Secret, key: "first-mark-key-123",
		body: `{"accounts":` + accounts + `,"marked":false}`,
	})
	requireStatus(t, response, 409)
	if !strings.Contains(response.Body.String(), "idempotency_conflict") {
		t.Fatal(response.Body.String())
	}
	if got := rateUnits(t, testServer.store, "mark.user.accounts.minute", identity.UserID); got != minuteUnits+2 {
		t.Fatalf("conflicting replay charged account units: %d", got-minuteUnits)
	}
}

func TestAccountValidation(t *testing.T) {
	t.Parallel()
	store, handler := fixture(t)
	cookie := registerLogin(t, handler, "alice")
	token := issueToken(t, handler, cookie)
	batch := make([]string, 101)
	for i := range batch {
		batch[i] = `{"platform":"github","username":"example"}`
	}
	tests := []struct {
		name, body string
		status     int
	}{
		{name: "empty batch", body: `{"accounts":[],"marked":true}`, status: 422},
		{name: "null accounts", body: `{"accounts":null,"marked":true}`, status: 422},
		{name: "missing marked", body: `{"accounts":[{"platform":"github","username":"example"}]}`, status: 422},
		{name: "null marked", body: `{"accounts":[{"platform":"github","username":"example"}],"marked":null}`, status: 422},
		{name: "unsupported platform is atomic", body: `{"accounts":[{"platform":"github","username":"example"},{"platform":"gitlab","username":"user"}],"marked":true}`, status: 422},
		{name: "profile URL", body: `{"accounts":[{"platform":"github","username":"https://github.com/user"}],"marked":true}`, status: 422},
		{name: "unknown nested field", body: `{"accounts":[{"platform":"github","username":"user","extra":true}],"marked":true}`, status: 422},
		{name: "case sensitive nested field", body: `{"accounts":[{"Platform":"github","username":"user"}],"marked":true}`, status: 422},
		{name: "duplicate nested key", body: `{"accounts":[{"platform":"github","username":"user","username":"other"}],"marked":true}`, status: 400},
		{name: "raw mark limit", body: `{"accounts":[` + strings.Join(batch[:51], ",") + `],"marked":true}`, status: 422},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := perform(handler, testRequest{path: "/openapi/v1/accounts/mark", token: token.Secret, key: "validation-key-123", body: tt.body})
			requireStatus(t, response, tt.status)
			if response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatal("bad error media type")
			}
		})
	}
	queryBody := `{"accounts":[{"platform":"github","username":"example"}]}`
	states := accountStates(t, perform(handler, testRequest{path: "/openapi/v1/accounts/query", token: token.Secret, body: queryBody}))
	if states[0].Marked {
		t.Fatal("invalid batch partially committed")
	}
	requireStatus(t, perform(handler, testRequest{path: "/openapi/v1/accounts/query", token: token.Secret, body: `{"accounts":[` + strings.Join(batch, ",") + `]}`}), 422)
	_, invalidHandler := fixture(t)
	invalidToken := issueToken(t, invalidHandler, registerLogin(t, invalidHandler, "invalid-mark"))
	requireStatus(t, perform(invalidHandler, testRequest{
		path: "/openapi/v1/accounts/mark", token: invalidToken.Secret, body: `{}`,
	}), 400)
	identity, err := store.AuthenticateSession(t.Context(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	expired := "expired-api-token"
	hash := sha256.Sum256([]byte(expired))
	testServer, ok := handler.(*testHandler)
	if !ok {
		t.Fatal("missing fixture store")
	}
	_, err = testServer.store.CreateCredential(t.Context(), data.Credential{
		UserID: identity.UserID, Kind: "api", Name: "expired", SecretHash: hex.EncodeToString(hash[:]),
		ExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	response := perform(handler, testRequest{path: "/openapi/v1/accounts/query", token: expired, body: queryBody})
	requireStatus(t, response, 401)
	if !strings.Contains(response.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Fatal("missing expired-token bearer challenge")
	}
}
