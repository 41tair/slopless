package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/41tair/slopless/pkg/model"
)

func TestBind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		body        string
		contentType string
		encoding    string
		status      int
	}{
		{name: "valid", body: `{"accounts":[]}`, contentType: "application/json; charset=utf-8", status: 200},
		{name: "duplicate", body: `{"accounts":[],"accounts":[]}`, status: 400},
		{name: "escaped duplicate", body: `{"accounts":[],"\u0061ccounts":[]}`, status: 400},
		{name: "nested duplicate", body: `{"accounts":[{"username":"one","username":"two"}]}`, status: 400},
		{name: "trailing object", body: `{"accounts":[]} {}`, status: 400},
		{name: "malformed", body: `{"accounts":`, status: 400},
		{name: "empty", status: 400},
		{name: "null", body: `null`, status: 422},
		{name: "array", body: `[]`, status: 422},
		{name: "unknown", body: `{"accounts":[],"other":1}`, status: 422},
		{name: "case sensitive keys", body: `{"Accounts":[]}`, status: 422},
		{name: "wrong type", body: `{"accounts":true}`, status: 422},
		{name: "too large", body: strings.Repeat(" ", 16385), status: 413},
		{name: "wrong media", body: `{}`, contentType: "text/plain", status: 415},
		{name: "compressed", body: `{}`, encoding: "gzip", status: 415},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
			contentType := tt.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("Content-Encoding", tt.encoding)
			recorder := httptest.NewRecorder()
			c := SContext{w: recorder, req: req}
			var input struct {
				Accounts []any `json:"accounts"`
			}
			ok := c.Bind(&input, "accounts")
			if ok != (tt.status == 200) || recorder.Code != tt.status {
				t.Fatalf("Bind = %v, status %d, want %d: %s", ok, recorder.Code, tt.status, recorder.Body.String())
			}
		})
	}
}

func TestRetryProblems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
		retry  string
	}{
		{name: "rate limited", err: &model.RateLimitError{
			RetryAfterSeconds: 42,
			RateLimit:         model.RatePolicy{Policy: "mark.user.accounts.minute", Limit: 200, WindowSeconds: 60, Unit: "accounts"},
		}, status: 429, retry: "42"},
		{name: "rate store unavailable", err: model.ErrUnavailable, status: 503, retry: "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			SContext{w: w, req: httptest.NewRequest("POST", "/", nil)}.failure(tt.err)
			var body struct {
				RetryAfterSeconds json.Number `json:"retryAfterSeconds"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tt.status || w.Header().Get("Retry-After") != tt.retry || string(body.RetryAfterSeconds) != tt.retry {
				t.Fatalf("incorrect retry problem: %d %v %s", w.Code, w.Header(), w.Body.String())
			}
		})
	}
}

func TestRequestFailureLogging(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const requestID = "request-id-123"
	newRequest := func(body string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "https://example.test/sensitive?token=query-secret", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer header-secret")
		req.Header.Set("Cookie", "session=cookie-secret")
		return req
	}
	checkLog := func(t *testing.T, level, code, path, cause string, status int) {
		t.Helper()
		text := output.String()
		if strings.Count(strings.TrimSpace(text), "\n") != 0 {
			t.Fatalf("expected one log event: %s", text)
		}
		var record map[string]any
		if err := json.Unmarshal(output.Bytes(), &record); err != nil {
			t.Fatalf("decode log: %v: %s", err, text)
		}
		loggedError, ok := record["error"].(string)
		if record["msg"] != "request failed" || record["level"] != level ||
			record["request_id"] != requestID || record["method"] != http.MethodPost ||
			record["path"] != path || record["status"] != float64(status) || record["code"] != code ||
			!ok || !strings.Contains(loggedError, cause) {
			t.Fatalf("incorrect log event: %s", text)
		}
		for _, secret := range []string{"query-secret", "header-secret", "cookie-secret", "body-password", "body-token", "body-invitation"} {
			if strings.Contains(text, secret) {
				t.Fatalf("log disclosed %q: %s", secret, text)
			}
		}
	}

	root := errors.New("connection reset by peer")
	tests := []struct {
		name, level, code, cause string
		err                      error
		status                   int
	}{
		{name: "authentication", level: "WARN", code: "unauthorized", cause: model.ErrUnauthorized.Error(), err: model.ErrUnauthorized, status: 401},
		{name: "validation", level: "WARN", code: "validation_failed", cause: "invalid account", err: &model.ValidationError{Path: "/accounts", Code: "invalid", Message: "invalid account"}, status: 422},
		{name: "rate limit", level: "WARN", code: "rate_limited", cause: "mark.user.accounts.minute", err: &model.RateLimitError{RetryAfterSeconds: 42, RateLimit: model.RatePolicy{Policy: "mark.user.accounts.minute", Limit: 200}}, status: 429},
		{name: "unavailable", level: "ERROR", code: "service_unavailable", cause: root.Error(), err: errors.Join(model.ErrUnavailable, root), status: 503},
		{name: "unknown", level: "ERROR", code: "internal_error", cause: "unexpected invariant", err: errors.New("unexpected invariant"), status: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output.Reset()
			w := httptest.NewRecorder()
			w.Header().Set("X-Request-Id", requestID)
			SContext{w: w, req: newRequest(`{"password":"body-password","token":"body-token","invitationCode":"body-invitation"}`)}.failure(tt.err)
			var problem struct {
				Code   string `json:"code"`
				Status int    `json:"status"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if w.Code != tt.status || problem.Code != tt.code || problem.Status != tt.status ||
				strings.Contains(problem.Detail, tt.cause) || tt.status >= 500 && strings.Contains(w.Body.String(), tt.cause) {
				t.Fatalf("incorrect sanitized problem: %d %s", w.Code, w.Body.String())
			}
			checkLog(t, tt.level, tt.code, "/sensitive", tt.cause, tt.status)
		})
	}

	t.Run("malformed bind", func(t *testing.T) {
		output.Reset()
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-Id", requestID)
		req := newRequest(`{"password":"body-password","token":"body-token","invitationCode":"body-invitation"`)
		var input map[string]any
		if (SContext{w: w, req: req}).Bind(&input, "password", "token", "invitationCode") {
			t.Fatal("malformed body was accepted")
		}
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_json"`) {
			t.Fatalf("incorrect bind problem: %d %s", w.Code, w.Body.String())
		}
		checkLog(t, "WARN", "invalid_json", "/sensitive", "one valid JSON object", http.StatusBadRequest)
	})

	t.Run("invalid field", func(t *testing.T) {
		output.Reset()
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-Id", requestID)
		(SContext{w: w, req: newRequest(`{}`)}).invalid("/username", "required", "Provide a username.")
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("incorrect validation problem: %d %s", w.Code, w.Body.String())
		}
		checkLog(t, "WARN", "validation_failed", "/sensitive", "Provide a username.", http.StatusUnprocessableEntity)
	})

	t.Run("cross origin", func(t *testing.T) {
		output.Reset()
		mux := http.NewServeMux()
		NewGroup("api/v1", mux).NewHandler("POST", "/protected", func(c SContext) { c.Resp(map[string]any{"ok": true}) })
		req := httptest.NewRequest(http.MethodPost, "https://example.test/api/v1/protected?token=query-secret", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Authorization", "Bearer header-secret")
		req.Header.Set("Cookie", "session=cookie-secret")
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-Id", requestID)
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"cross_origin_request"`) {
			t.Fatalf("incorrect cross-origin problem: %d %s", w.Code, w.Body.String())
		}
		checkLog(t, "WARN", "cross_origin_request", "/api/v1/protected", "Cross-origin request rejected.", http.StatusForbidden)
	})
}
