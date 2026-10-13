//go:build integration

// Package d1test provisions isolated Cloudflare D1 databases for integration tests.
package d1test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	cloudflareAPI    = "https://api.cloudflare.com/client/v4"
	maxResponseBytes = 1 << 20
)

// Database contains the connection details for an isolated test database.
type Database struct {
	ID        string
	AccountID string
	Endpoint  string
	Token     string
}

type apiResponse struct {
	Result struct {
		UUID string `json:"uuid"`
	} `json:"result"`
	Success bool `json:"success"`
	Errors  []struct {
		Code int `json:"code"`
	} `json:"errors"`
}

// New creates an isolated Cloudflare D1 database and deletes it during cleanup.
func New(t *testing.T) Database {
	t.Helper()

	accountID := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if strings.TrimSpace(token) == "" {
		token = os.Getenv("CLOUDFLARE_API_KEY")
	}
	hasAccount := accountID != ""
	hasToken := strings.TrimSpace(token) != ""
	if !hasAccount && !hasToken {
		t.Skip("Cloudflare D1 credentials are not set")
	}
	if !hasAccount {
		t.Fatal("CLOUDFLARE_ACCOUNT_ID is required when a Cloudflare API credential is set")
	}
	if !hasToken {
		t.Fatal("CLOUDFLARE_API_TOKEN or CLOUDFLARE_API_KEY is required when CLOUDFLARE_ACCOUNT_ID is set")
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	collectionURL := cloudflareAPI + "/accounts/" + url.PathEscape(accountID) + "/d1/database"
	body, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: "slopless-test-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("encode D1 database request: %v", err)
	}
	request, err := newRequest(t.Context(), http.MethodPost, collectionURL, token, body)
	if err != nil {
		t.Fatalf("create D1 database request: %v", err)
	}
	created, err := do(client, request)
	if err != nil {
		t.Fatalf("create D1 database: %v", err)
	}
	if created.Result.UUID == "" {
		t.Fatalf("create D1 database: HTTP success response is missing result.uuid")
	}

	databaseURL := collectionURL + "/" + url.PathEscape(created.Result.UUID)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		request, err := newRequest(ctx, http.MethodDelete, databaseURL, token, nil)
		if err != nil {
			t.Errorf("delete D1 database request: %v", err)
			return
		}
		if _, err := do(client, request); err != nil {
			t.Errorf("delete D1 database: %v", err)
		}
	})

	return Database{
		ID:        created.Result.UUID,
		AccountID: accountID,
		Endpoint:  databaseURL + "/query",
		Token:     token,
	}
}

func newRequest(ctx context.Context, method, endpoint, token string, body []byte) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func do(client *http.Client, request *http.Request) (apiResponse, error) {
	response, err := client.Do(request)
	if err != nil {
		return apiResponse{}, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return apiResponse{}, fmt.Errorf("read HTTP %s response: %w", response.Status, err)
	}
	if len(body) > maxResponseBytes {
		return apiResponse{}, fmt.Errorf("HTTP %s response exceeds %d bytes", response.Status, maxResponseBytes)
	}

	var result apiResponse
	if err := json.Unmarshal(body, &result); err != nil {
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return apiResponse{}, fmt.Errorf("HTTP %s", response.Status)
		}
		return apiResponse{}, fmt.Errorf("decode HTTP %s response: %w", response.Status, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || !result.Success {
		codes := make([]int, 0, len(result.Errors))
		for _, apiError := range result.Errors {
			codes = append(codes, apiError.Code)
		}
		return apiResponse{}, fmt.Errorf("HTTP %s, Cloudflare error codes %v", response.Status, codes)
	}
	return result, nil
}
