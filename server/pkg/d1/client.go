package d1

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	apiEndpoint         = "https://api.cloudflare.com/client/v4"
	maxSnapshotResponse = 64 << 20
	commitAttempts      = 4
	maxSafeInteger      = int64(1<<53 - 1)
)

var (
	ErrConflict    = errors.New("d1 revision conflict")
	ErrUnavailable = errors.New("d1 unavailable")
)

type client struct {
	endpoint    *url.URL
	token       string
	http        *http.Client
	schemaMu    sync.Mutex
	schemaReady bool
}

type snapshot struct {
	Version   int64         `json:"version"`
	Schema    []schemaEntry `json:"schema"`
	Tables    []tableData   `json:"tables"`
	Sequences []sequence    `json:"sequences"`
}

type schemaEntry struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Table string `json:"table"`
	SQL   string `json:"sql"`
}

type tableData struct {
	Name    string       `json:"name"`
	Columns []columnInfo `json:"columns"`
	Rows    [][]scalar   `json:"rows"`
}

type columnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PK   int    `json:"pk"`
}

type sequence struct {
	Name string `json:"name"`
	Seq  int64  `json:"seq"`
}

type change struct {
	Table      string     `json:"table"`
	Columns    []string   `json:"columns"`
	PrimaryKey []string   `json:"primaryKey"`
	Rows       [][]scalar `json:"rows"`
	Deleted    [][]scalar `json:"deleted"`
}

type commitRequest struct {
	Version   int64      `json:"version"`
	TxID      string     `json:"txid"`
	DDL       []string   `json:"ddl"`
	Changes   []change   `json:"changes"`
	Sequences []sequence `json:"sequences"`
}

// statement and queryResult describe Cloudflare's D1 REST contract.
type statement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

type queryResult struct {
	Results []map[string]json.RawMessage `json:"results"`
	Success bool                         `json:"success"`
}

type apiError struct {
	Status int
	Codes  []int
}

func (e *apiError) Error() string {
	// D1 error messages can contain SQL values. Keep them out of application logs.
	return fmt.Sprintf("D1 REST request failed with HTTP %d (codes %v)", e.Status, e.Codes)
}

type blob struct{ Bytes []byte }

type scalar struct{ Value any }

func (s scalar) MarshalJSON() ([]byte, error) {
	switch value := s.Value.(type) {
	case nil, bool, string, float64:
		return json.Marshal(value)
	case int:
		return marshalInteger(int64(value))
	case int8:
		return marshalInteger(int64(value))
	case int16:
		return marshalInteger(int64(value))
	case int32:
		return marshalInteger(int64(value))
	case int64:
		return marshalInteger(value)
	case uint:
		if uint64(value) > uint64(maxSafeInteger) {
			return nil, fmt.Errorf("integer exceeds the D1 safe range")
		}
		return json.Marshal(value)
	case uint8:
		return json.Marshal(value)
	case uint16:
		return json.Marshal(value)
	case uint32:
		return json.Marshal(value)
	case uint64:
		if value > uint64(maxSafeInteger) {
			return nil, fmt.Errorf("integer exceeds the D1 safe range")
		}
		return json.Marshal(value)
	case []byte:
		return json.Marshal(struct {
			Blob string `json:"blob"`
		}{Blob: strings.ToUpper(hex.EncodeToString(value))})
	case blob:
		return json.Marshal(struct {
			Blob string `json:"blob"`
		}{Blob: strings.ToUpper(hex.EncodeToString(value.Bytes))})
	case time.Time:
		return json.Marshal(value.UTC().Format(time.RFC3339Nano))
	default:
		return nil, fmt.Errorf("unsupported D1 scalar type %T", s.Value)
	}
}

func marshalInteger(value int64) ([]byte, error) {
	if value < -maxSafeInteger || value > maxSafeInteger {
		return nil, fmt.Errorf("integer exceeds the D1 safe range")
	}
	return json.Marshal(value)
}

func (s *scalar) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	switch value := value.(type) {
	case nil, bool, string:
		s.Value = value
		return nil
	case json.Number:
		if strings.ContainsAny(value.String(), ".eE") {
			f, err := value.Float64()
			if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return fmt.Errorf("invalid D1 number")
			}
			s.Value = f
			return nil
		}
		i, err := value.Int64()
		if err != nil || i < -maxSafeInteger || i > maxSafeInteger {
			return fmt.Errorf("D1 integer exceeds the safe range")
		}
		s.Value = i
		return nil
	case map[string]any:
		if len(value) != 1 {
			return fmt.Errorf("invalid D1 scalar object")
		}
		hexValue, ok := value["blob"].(string)
		if !ok || hexValue != strings.ToUpper(hexValue) || len(hexValue)%2 != 0 {
			return fmt.Errorf("invalid D1 blob")
		}
		decoded, err := hex.DecodeString(hexValue)
		if err != nil {
			return fmt.Errorf("invalid D1 blob: %w", err)
		}
		s.Value = decoded
		return nil
	default:
		return fmt.Errorf("invalid D1 scalar type %T", value)
	}
}

func newClient(endpoint, token string) (*client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("D1 token is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("parse D1 endpoint: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("D1 endpoint must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("D1 endpoint must be an absolute REST URL without credentials, query, or fragment")
	}
	local := parsed.Hostname() == "localhost"
	if address, err := netip.ParseAddr(parsed.Hostname()); err == nil {
		local = address.IsLoopback()
	}
	if parsed.Scheme != "https" && !local {
		return nil, fmt.Errorf("D1 endpoint must use HTTPS, or HTTP on loopback for development")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/query") {
		return nil, fmt.Errorf("D1 endpoint must end with /query")
	}
	return &client{
		endpoint: parsed,
		token:    token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *client) query(ctx context.Context, statements []statement) ([]queryResult, error) {
	if len(statements) == 0 {
		return []queryResult{}, nil
	}
	batch := append([]statement(nil), statements...)
	for i := range batch {
		if batch[i].Params == nil {
			batch[i].Params = []any{}
		}
	}
	body, err := json.Marshal(struct {
		Batch []statement `json:"batch"`
	}{Batch: batch})
	if err != nil {
		return nil, fmt.Errorf("encode D1 query: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.endpoint.String(),
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create D1 query request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send D1 query: %w", err)
	}
	defer response.Body.Close()
	payload, err := readLimited(response.Body, maxSnapshotResponse)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Result  []queryResult `json:"result"`
		Success bool          `json:"success"`
		Errors  []struct {
			Code int `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, &apiError{Status: response.StatusCode}
		}
		return nil, fmt.Errorf("D1 REST returned an invalid response (HTTP %d)", response.StatusCode)
	}
	failure := &apiError{Status: response.StatusCode, Codes: []int{}}
	for _, item := range envelope.Errors {
		failure.Codes = append(failure.Codes, item.Code)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		return nil, failure
	}
	for _, result := range envelope.Result {
		if !result.Success {
			return nil, failure
		}
	}
	if len(envelope.Result) != len(batch) {
		return nil, fmt.Errorf("D1 REST returned %d results for %d statements", len(envelope.Result), len(batch))
	}
	return envelope.Result, nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("D1 response exceeds %d bytes", limit)
	}
	return payload, nil
}

func waitForRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(1<<min(attempt, 5)) * 5 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateSnapshot(value *snapshot) error {
	if value.Version < 0 || value.Version > maxSafeInteger {
		return fmt.Errorf("invalid revision")
	}
	schemaTables := make(map[string]bool)
	for _, entry := range value.Schema {
		if entry.Name == "" || entry.SQL == "" {
			return fmt.Errorf("empty schema entry")
		}
		switch entry.Type {
		case "table":
			if schemaTables[entry.Name] {
				return fmt.Errorf("duplicate table schema %q", entry.Name)
			}
			schemaTables[entry.Name] = true
		case "index", "trigger":
		default:
			return fmt.Errorf("unsupported schema entry type %q", entry.Type)
		}
	}
	seenTables := make(map[string]bool, len(value.Tables))
	for _, table := range value.Tables {
		if table.Name == "" || !schemaTables[table.Name] || seenTables[table.Name] {
			return fmt.Errorf("invalid table %q", table.Name)
		}
		seenTables[table.Name] = true
		seenColumns := make(map[string]bool, len(table.Columns))
		primaryKeys := 0
		for _, column := range table.Columns {
			if column.Name == "" || seenColumns[column.Name] || column.PK < 0 {
				return fmt.Errorf("invalid column in table %q", table.Name)
			}
			seenColumns[column.Name] = true
			if column.PK > 0 {
				primaryKeys++
			}
		}
		if primaryKeys == 0 {
			return fmt.Errorf("table %q has no primary key", table.Name)
		}
		for _, row := range table.Rows {
			if len(row) != len(table.Columns) {
				return fmt.Errorf("invalid row width in table %q", table.Name)
			}
		}
	}
	seenSequences := make(map[string]bool, len(value.Sequences))
	for _, sequence := range value.Sequences {
		if sequence.Name == "" || sequence.Seq < 0 || sequence.Seq > maxSafeInteger || seenSequences[sequence.Name] {
			return fmt.Errorf("invalid sequence")
		}
		seenSequences[sequence.Name] = true
	}
	return nil
}
