package d1

import (
	"bytes"
	"context"
	"database/sql/driver"
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
	apiEndpoint      = "https://api.cloudflare.com/client/v4"
	bindingEndpoint  = "http://d1.internal/query"
	maxResponseBytes = 64 << 20
	requestAttempts  = 4
	maxSafeInteger   = int64(1<<53 - 1)
)

var (
	ErrConflict                = errors.New("d1 revision conflict")
	ErrUnavailable             = errors.New("d1 unavailable")
	ErrTransactionsUnsupported = errors.New("d1 interactive transactions are unsupported")
)

// Statement is one parameterized SQLite statement sent to D1.
type Statement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

// Result contains the rows and write metadata returned for one statement.
// Rows use the same order as Columns.
type Result struct {
	Columns      []string
	Rows         [][]any
	Changes      int64
	LastInsertID int64
}

type client struct {
	queryEndpoint *url.URL
	rawEndpoint   *url.URL
	token         string
	http          *http.Client

	schemaMu    sync.Mutex
	schemaReady bool
}

type apiError struct {
	Status     int
	Codes      []int
	constraint bool
	transient  bool
}

func (e *apiError) Error() string {
	// Cloudflare messages can contain statement values. Keep them out of logs.
	return fmt.Sprintf("D1 request failed with HTTP %d (codes %v)", e.Status, e.Codes)
}

type constraintError struct{ error }

func (*constraintError) Code() int       { return 19 }
func (e *constraintError) Unwrap() error { return e.error }

type requestError struct{ error }

func (e *requestError) Unwrap() error { return e.error }

type scalar struct{ Value any }

func (s scalar) MarshalJSON() ([]byte, error) {
	switch value := s.Value.(type) {
	case nil, bool, string, float64:
		return json.Marshal(value)
	case float32:
		return json.Marshal(float64(value))
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
		return marshalUnsigned(uint64(value))
	case uint8:
		return marshalUnsigned(uint64(value))
	case uint16:
		return marshalUnsigned(uint64(value))
	case uint32:
		return marshalUnsigned(uint64(value))
	case uint64:
		return marshalUnsigned(value)
	case []byte:
		return json.Marshal(struct {
			Blob string `json:"blob"`
		}{Blob: strings.ToUpper(hex.EncodeToString(value))})
	case time.Time:
		return json.Marshal(formatSQLiteTime(value))
	default:
		converted, err := driver.DefaultParameterConverter.ConvertValue(s.Value)
		if err != nil {
			// A Valuer error can include the value itself. Report only its type.
			return nil, fmt.Errorf("unsupported D1 parameter type %T", s.Value)
		}
		return scalar{Value: converted}.MarshalJSON()
	}
}

func marshalInteger(value int64) ([]byte, error) {
	if value < -maxSafeInteger || value > maxSafeInteger {
		return nil, fmt.Errorf("integer exceeds the D1 safe range")
	}
	return json.Marshal(value)
}

func marshalUnsigned(value uint64) ([]byte, error) {
	if value > uint64(maxSafeInteger) {
		return nil, fmt.Errorf("integer exceeds the D1 safe range")
	}
	return json.Marshal(value)
}

func newClient(endpoint, token string) (*client, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("parse D1 endpoint: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("D1 endpoint must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("D1 endpoint must be an absolute URL without credentials, query, or fragment")
	}
	hostname := parsed.Hostname()
	local := hostname == "localhost" || hostname == "d1.internal"
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		local = address.IsLoopback()
	}
	if parsed.Scheme != "https" && !local {
		return nil, fmt.Errorf("D1 endpoint must use HTTPS, or the local D1 binding endpoint")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/query") {
		return nil, fmt.Errorf("D1 endpoint must end with /query")
	}
	token = strings.TrimSpace(token)
	if token == "" && hostname != "d1.internal" {
		return nil, fmt.Errorf("D1 token is required")
	}
	raw := *parsed
	raw.Path = strings.TrimSuffix(raw.Path, "/query") + "/raw"
	return &client{
		queryEndpoint: parsed,
		rawEndpoint:   &raw,
		token:         token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *client) read(ctx context.Context, statement Statement) (Result, error) {
	prepared, err := prepareStatement(statement)
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}{SQL: prepared.SQL, Params: nonNilParams(prepared.Params)})
	if err != nil {
		return Result{}, fmt.Errorf("encode D1 raw query: %w", err)
	}
	var requestErr error
	for attempt := range requestAttempts {
		results, err := c.do(ctx, c.rawEndpoint, body, 1)
		if err == nil {
			return results[0], nil
		}
		requestErr = err
		if !isTransientD1Error(err) || attempt+1 == requestAttempts {
			return Result{}, err
		}
		if err := waitForRetry(ctx, attempt); err != nil {
			return Result{}, err
		}
	}
	return Result{}, requestErr
}

func (c *client) batch(ctx context.Context, statements []Statement) ([]Result, error) {
	if len(statements) == 0 {
		return []Result{}, nil
	}
	batch := make([]Statement, len(statements))
	for i, statement := range statements {
		prepared, err := prepareStatement(statement)
		if err != nil {
			return nil, fmt.Errorf("prepare D1 statement %d: %w", i, err)
		}
		prepared.Params = nonNilParams(prepared.Params)
		batch[i] = prepared
	}
	body, err := json.Marshal(struct {
		Batch []Statement `json:"batch"`
	}{Batch: batch})
	if err != nil {
		return nil, fmt.Errorf("encode D1 batch: %w", err)
	}
	return c.do(ctx, c.queryEndpoint, body, len(batch))
}

func nonNilParams(params []any) []any {
	if params == nil {
		return []any{}
	}
	return params
}

func (c *client) do(ctx context.Context, endpoint *url.URL, body []byte, expected int) ([]Result, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create D1 request: %w", err)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &requestError{error: fmt.Errorf("send D1 request: %w", err)}
	}
	defer response.Body.Close()
	payload, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return nil, &requestError{error: err}
	}
	results, failure, err := decodeEnvelope(payload, response.StatusCode)
	if err != nil {
		return nil, &requestError{error: err}
	}
	if failure != nil {
		if failure.constraint {
			return nil, &constraintError{error: failure}
		}
		return nil, failure
	}
	if len(results) != expected {
		return nil, &requestError{error: fmt.Errorf("D1 returned %d results for %d statements", len(results), expected)}
	}
	return results, nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read D1 response: %w", err)
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("D1 response exceeds %d bytes", limit)
	}
	return payload, nil
}

type wireEnvelope struct {
	Result  []json.RawMessage `json:"result"`
	Success bool              `json:"success"`
	Errors  []wireError       `json:"errors"`
}

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type wireResult struct {
	Results json.RawMessage `json:"results"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Meta    struct {
		Changes      json.Number `json:"changes"`
		LastInsertID json.Number `json:"last_row_id"`
	} `json:"meta"`
}

func decodeEnvelope(payload []byte, status int) ([]Result, *apiError, error) {
	var envelope wireEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			return nil, &apiError{Status: status}, nil
		}
		return nil, nil, fmt.Errorf("D1 returned an invalid response (HTTP %d)", status)
	}
	failure := &apiError{Status: status, Codes: []int{}}
	for _, item := range envelope.Errors {
		failure.Codes = append(failure.Codes, item.Code)
		failure.constraint = failure.constraint || item.Code&0xff == 19 || looksLikeConstraint(item.Message)
		nativeCode := item.Code & 0xff
		failure.transient = failure.transient || nativeCode == 5 || nativeCode == 6 || looksLikeTransient(item.Message)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || !envelope.Success {
		return nil, failure, nil
	}
	results := make([]Result, len(envelope.Result))
	for i, raw := range envelope.Result {
		var item wireResult
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, nil, fmt.Errorf("decode D1 result %d: %w", i, err)
		}
		if !item.Success {
			failure.constraint = failure.constraint || looksLikeConstraint(item.Error)
			failure.transient = failure.transient || looksLikeTransient(item.Error)
			return nil, failure, nil
		}
		result, err := decodeResult(item)
		if err != nil {
			return nil, nil, fmt.Errorf("decode D1 result %d: %w", i, err)
		}
		results[i] = result
	}
	return results, nil, nil
}

func decodeResult(item wireResult) (Result, error) {
	result := Result{Changes: numberInt64(item.Meta.Changes), LastInsertID: numberInt64(item.Meta.LastInsertID)}
	trimmed := bytes.TrimSpace(item.Results)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return result, nil
	}
	if trimmed[0] == '{' {
		var raw struct {
			Columns []string            `json:"columns"`
			Rows    [][]json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return Result{}, err
		}
		result.Columns = append([]string(nil), raw.Columns...)
		result.Rows = make([][]any, len(raw.Rows))
		for i, row := range raw.Rows {
			if len(row) != len(result.Columns) {
				return Result{}, fmt.Errorf("row width %d does not match %d columns", len(row), len(result.Columns))
			}
			result.Rows[i] = make([]any, len(row))
			for j, value := range row {
				decoded, err := decodeValue(value)
				if err != nil {
					return Result{}, err
				}
				result.Rows[i][j] = decoded
			}
		}
		return result, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return Result{}, err
	}
	for i, row := range rows {
		columns, values, err := decodeObjectRow(row)
		if err != nil {
			return Result{}, err
		}
		if i == 0 {
			result.Columns = columns
		} else if !equalStrings(result.Columns, columns) {
			return Result{}, fmt.Errorf("D1 result column order changed between rows")
		}
		result.Rows = append(result.Rows, values)
	}
	return result, nil
}

func decodeObjectRow(raw json.RawMessage) ([]string, []any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, nil, fmt.Errorf("D1 row is not an object")
	}
	var columns []string
	var values []any
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		var encoded json.RawMessage
		if err := decoder.Decode(&encoded); err != nil {
			return nil, nil, err
		}
		value, err := decodeValue(encoded)
		if err != nil {
			return nil, nil, err
		}
		columns = append(columns, name.(string))
		values = append(values, value)
	}
	return columns, values, nil
}

func decodeValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	switch value := value.(type) {
	case nil, bool, string:
		return value, nil
	case json.Number:
		if strings.ContainsAny(value.String(), ".eE") {
			float, err := value.Float64()
			if err != nil || math.IsInf(float, 0) || math.IsNaN(float) {
				return nil, fmt.Errorf("invalid D1 number")
			}
			return float, nil
		}
		integer, err := value.Int64()
		if err != nil || integer < -maxSafeInteger || integer > maxSafeInteger {
			return nil, fmt.Errorf("D1 integer exceeds the safe range")
		}
		return integer, nil
	case []any:
		blob := make([]byte, len(value))
		for i, entry := range value {
			number, ok := entry.(json.Number)
			if !ok {
				return nil, fmt.Errorf("invalid D1 BLOB")
			}
			integer, err := number.Int64()
			if err != nil || integer < 0 || integer > math.MaxUint8 {
				return nil, fmt.Errorf("invalid D1 BLOB")
			}
			blob[i] = byte(integer)
		}
		return blob, nil
	case map[string]any:
		encoded, ok := value["blob"].(string)
		if !ok || len(value) != 1 || len(encoded)%2 != 0 {
			return nil, fmt.Errorf("invalid D1 BLOB")
		}
		blob, err := hex.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid D1 BLOB")
		}
		return blob, nil
	default:
		return nil, fmt.Errorf("unsupported D1 value %T", value)
	}
}

func numberInt64(number json.Number) int64 {
	value, _ := number.Int64()
	return value
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

func looksLikeConstraint(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "constraint") ||
		strings.Contains(message, "not null") ||
		strings.Contains(message, "unique failed") ||
		strings.Contains(message, "foreign key")
}

func looksLikeTransient(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database is busy") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "sqlite_locked") ||
		strings.Contains(message, "temporarily unavailable")
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
