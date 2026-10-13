package d1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const sqliteTimeFormat = "2006-01-02 15:04:05.000000000-07:00"

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	txIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	applicationTables = map[string]struct{}{
		"accounts": {}, "confirmations": {}, "credentials": {},
		"credit_entries": {}, "idempotencies": {}, "invitations": {},
		"marks": {}, "rate_events": {}, "users": {},
	}
)

func (d *Driver) Read(ctx context.Context, statement Statement) (Result, error) {
	if err := d.checkOpen(); err != nil {
		return Result{}, err
	}
	result, err := d.client.read(ctx, statement)
	return result, unavailableIfTransient(err)
}

// Batch executes the statements in one D1 batch. It does not change the
// application revision or add compare-and-swap behavior.
func (d *Driver) Batch(ctx context.Context, statements []Statement) ([]Result, error) {
	if err := d.checkOpen(); err != nil {
		return nil, err
	}
	results, err := d.client.batch(ctx, statements)
	return results, unavailableIfTransient(err)
}

func (d *Driver) Version(ctx context.Context) (int64, error) {
	if err := d.checkOpen(); err != nil {
		return 0, err
	}
	if err := d.client.ensureControlSchema(ctx); err != nil {
		return 0, fmt.Errorf("check D1 metadata: %w", unavailableIfTransient(err))
	}
	result, err := d.Read(ctx, Statement{SQL: `SELECT "version" FROM "__slopless_meta" WHERE "id" = 1`})
	if err != nil {
		return 0, err
	}
	value, err := singleInteger(result, "version")
	if err != nil || value < 0 || value > maxSafeInteger {
		return 0, fmt.Errorf("%w: invalid D1 revision", ErrUnavailable)
	}
	return value, nil
}

// Commit atomically applies statements if version is still current.
func (d *Driver) Commit(ctx context.Context, version int64, mutations []Statement) error {
	if err := d.checkOpen(); err != nil {
		return err
	}
	if version < 0 || version >= maxSafeInteger {
		return fmt.Errorf("D1 revision is out of range")
	}
	if err := d.client.ensureControlSchema(ctx); err != nil {
		return fmt.Errorf("check D1 metadata: %w", unavailableIfTransient(err))
	}
	txID := uuid.NewString()
	now := time.Now().Unix()
	statements := make([]Statement, 0, len(mutations)+5)
	statements = append(statements,
		Statement{SQL: `PRAGMA defer_foreign_keys = ON`},
		Statement{
			SQL: `UPDATE "__slopless_meta"
				SET "version" = CASE WHEN "version" = ? THEN "version" + 1 ELSE NULL END
				WHERE "id" = 1`,
			Params: []any{version},
		},
	)
	statements = append(statements, mutations...)
	statements = append(statements,
		Statement{
			SQL: `INSERT INTO "__slopless_commit_outcomes"
				("txid", "version", "changes", "last_insert_id", "returning_column", "created_at")
				VALUES (?, ?, 0, 0, '', ?)`,
			Params: []any{txID, version + 1, now},
		},
		pruneCommitsStatement(now),
	)

	var requestErr error
	for attempt := range requestAttempts {
		_, requestErr = d.client.batch(ctx, statements)
		if requestErr == nil {
			return nil
		}
		outcome, found, resolveErr := d.client.resolveOutcome(ctx, txID)
		if resolveErr == nil && found && outcome.Version == version+1 {
			return nil
		}
		if isRESTClientFailure(requestErr) {
			current, versionErr := d.Version(ctx)
			if versionErr == nil && current != version {
				return fmt.Errorf("%w: base revision changed", ErrConflict)
			}
			return requestErr
		}
		if attempt+1 < requestAttempts {
			if err := waitForRetry(ctx, attempt); err != nil {
				return fmt.Errorf("%w: commit outcome is unknown: %w", ErrUnavailable, err)
			}
		}
	}
	return fmt.Errorf("%w: commit outcome is unknown: %w", ErrUnavailable, requestErr)
}

// NextID atomically reserves the next integer ID for table. ID allocation is
// independent from the application revision.
func (d *Driver) NextID(ctx context.Context, table string) (int, error) {
	return d.ReserveIDs(ctx, table, 1)
}

// ReserveIDs atomically reserves count consecutive positive integer IDs and
// returns the first. It consults the target table on every allocation, so rows
// inserted outside the allocator cannot cause it to issue a duplicate ID.
func (d *Driver) ReserveIDs(ctx context.Context, table string, count int) (int, error) {
	if err := d.checkOpen(); err != nil {
		return 0, err
	}
	if !identifierPattern.MatchString(table) {
		return 0, fmt.Errorf("invalid D1 table name")
	}
	if _, ok := applicationTables[table]; !ok {
		return 0, fmt.Errorf("D1 ID allocation is not allowed for table %q", table)
	}
	if count <= 0 || int64(count) > maxSafeInteger {
		return 0, fmt.Errorf("D1 ID allocation count is out of range")
	}
	if err := d.client.ensureControlSchema(ctx); err != nil {
		return 0, fmt.Errorf("check D1 metadata: %w", unavailableIfTransient(err))
	}
	quoted := quoteIdentifier(table)
	result, err := d.client.batch(ctx, []Statement{{
		SQL: `INSERT INTO "__slopless_ids" ("table_name", "next_id")
			SELECT ?, COALESCE((SELECT MAX("id") FROM ` + quoted + `), 0) + ?
			ON CONFLICT ("table_name") DO UPDATE SET "next_id" = MAX(
				"__slopless_ids"."next_id",
				COALESCE((SELECT MAX("id") FROM ` + quoted + `), 0)
			) + ?
			RETURNING "next_id" - ? + 1 AS "id"`,
		Params: []any{table, count, count, count},
	}})
	if err != nil {
		return 0, unavailableIfTransient(err)
	}
	value, err := singleInteger(result[0], "id")
	last := value + int64(count) - 1
	if err != nil || value <= 0 || last > maxSafeInteger || int64(int(value)) != value {
		return 0, fmt.Errorf("%w: invalid allocated ID", ErrUnavailable)
	}
	return int(value), nil
}

type writeOutcome struct {
	Version         int64
	Changes         int64
	LastInsertID    int64
	ReturningColumn string
}

func (d *Driver) write(ctx context.Context, statement Statement) (Result, error) {
	if err := d.client.ensureControlSchema(ctx); err != nil {
		return Result{}, fmt.Errorf("check D1 metadata: %w", unavailableIfTransient(err))
	}
	txID := uuid.NewString()
	now := time.Now().Unix()
	returningColumn := returningIDColumn(statement.SQL)
	statements := []Statement{
		statement,
		{
			SQL: `INSERT INTO "__slopless_commit_outcomes"
				("txid", "version", "changes", "last_insert_id", "returning_column", "created_at")
				SELECT ?, "version" + 1, changes(), last_insert_rowid(), ?, ?
				FROM "__slopless_meta" WHERE "id" = 1`,
			Params: []any{txID, returningColumn, now},
		},
		{SQL: `UPDATE "__slopless_meta" SET "version" = "version" + 1 WHERE "id" = 1`},
		pruneCommitsStatement(now),
	}
	var requestErr error
	for attempt := range requestAttempts {
		results, err := d.client.batch(ctx, statements)
		if err == nil {
			return results[0], nil
		}
		requestErr = err
		outcome, found, resolveErr := d.client.resolveOutcome(ctx, txID)
		if resolveErr == nil && found {
			result := Result{Changes: outcome.Changes, LastInsertID: outcome.LastInsertID}
			if outcome.ReturningColumn != "" {
				result.Columns = []string{outcome.ReturningColumn}
				result.Rows = [][]any{{outcome.LastInsertID}}
			}
			return result, nil
		}
		if isRESTClientFailure(err) {
			return Result{}, err
		}
		if attempt+1 < requestAttempts {
			if err := waitForRetry(ctx, attempt); err != nil {
				return Result{}, fmt.Errorf("%w: write outcome is unknown: %w", ErrUnavailable, err)
			}
		}
	}
	return Result{}, fmt.Errorf("%w: write outcome is unknown: %w", ErrUnavailable, requestErr)
}

func pruneCommitsStatement(now int64) Statement {
	return Statement{
		SQL:    `DELETE FROM "__slopless_commit_outcomes" WHERE "created_at" < ?`,
		Params: []any{now - int64((72*time.Hour)/time.Second)},
	}
}

func (c *client) ensureControlSchema(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if c.schemaReady {
		return nil
	}
	result, err := c.read(ctx, Statement{SQL: `
		SELECT m."version" AS "meta_version",
			o."txid" AS "outcome_txid", o."version" AS "outcome_version",
			o."changes" AS "outcome_changes", o."last_insert_id" AS "outcome_last_insert_id",
			o."returning_column" AS "outcome_returning_column",
			o."created_at" AS "outcome_created_at",
			i."table_name" AS "ids_table_name", i."next_id" AS "ids_next_id"
		FROM "__slopless_meta" AS m
		LEFT JOIN "__slopless_commit_outcomes" AS o ON FALSE
		LEFT JOIN "__slopless_ids" AS i ON FALSE
		WHERE m."id" = 1`})
	if err != nil {
		return err
	}
	if len(result.Columns) != 9 || len(result.Rows) != 1 || len(result.Rows[0]) != 9 {
		return fmt.Errorf("D1 metadata schema is incomplete")
	}
	if version, ok := asInt64(result.Rows[0][0]); !ok || version < 0 || version > maxSafeInteger {
		return fmt.Errorf("D1 metadata revision is invalid")
	}
	c.schemaReady = true
	return nil
}

func (c *client) resolveOutcome(ctx context.Context, txID string) (writeOutcome, bool, error) {
	if !txIDPattern.MatchString(txID) {
		return writeOutcome{}, false, nil
	}
	resolve := func(resolveCtx context.Context) (writeOutcome, bool, error) {
		result, err := c.read(resolveCtx, Statement{
			SQL: `SELECT "version", "changes", "last_insert_id", "returning_column"
				FROM "__slopless_commit_outcomes" WHERE "txid" = ?`,
			Params: []any{txID},
		})
		if err != nil {
			return writeOutcome{}, false, err
		}
		if len(result.Rows) == 0 {
			return writeOutcome{}, false, nil
		}
		if len(result.Rows) != 1 || len(result.Rows[0]) != 4 {
			return writeOutcome{}, false, fmt.Errorf("invalid D1 commit outcome")
		}
		version, okVersion := asInt64(result.Rows[0][0])
		changes, okChanges := asInt64(result.Rows[0][1])
		lastID, okLastID := asInt64(result.Rows[0][2])
		column, okColumn := result.Rows[0][3].(string)
		if !okVersion || !okChanges || !okLastID || !okColumn {
			return writeOutcome{}, false, fmt.Errorf("invalid D1 commit outcome")
		}
		return writeOutcome{Version: version, Changes: changes, LastInsertID: lastID, ReturningColumn: column}, true, nil
	}
	outcome, found, err := resolve(ctx)
	if err == nil || ctx.Err() == nil {
		return outcome, found, err
	}
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return resolve(resolveCtx)
}

func singleInteger(result Result, column string) (int64, error) {
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
		return 0, fmt.Errorf("D1 result is not one integer")
	}
	if len(result.Columns) == 1 && result.Columns[0] != column {
		return 0, fmt.Errorf("D1 result column is %q, want %q", result.Columns[0], column)
	}
	value, ok := asInt64(result.Rows[0][0])
	if !ok {
		return 0, fmt.Errorf("D1 result is not an integer")
	}
	return value, nil
}

func asInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case float64:
		integer := int64(value)
		return integer, float64(integer) == value
	default:
		return 0, false
	}
}

func returningIDColumn(query string) string {
	upper := strings.ToUpper(query)
	index := strings.LastIndex(upper, "RETURNING")
	if index < 0 {
		return ""
	}
	value := strings.TrimSpace(strings.TrimSuffix(query[index+len("RETURNING"):], ";"))
	value = strings.Trim(value, "`\"")
	if identifierPattern.MatchString(value) && strings.EqualFold(value, "id") {
		return value
	}
	return ""
}

func isRESTClientFailure(err error) bool {
	var failure *apiError
	if !errors.As(err, &failure) || failure.transient || failure.Status == http.StatusTooManyRequests {
		return false
	}
	statementFailure := failure.Status >= http.StatusOK && failure.Status < http.StatusMultipleChoices
	clientFailure := failure.Status >= http.StatusBadRequest && failure.Status < http.StatusInternalServerError
	return statementFailure || clientFailure
}

func unavailableIfTransient(err error) error {
	if err == nil || errors.Is(err, ErrUnavailable) {
		return err
	}
	if isTransientD1Error(err) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

func isTransientD1Error(err error) bool {
	var requestFailure *requestError
	if errors.As(err, &requestFailure) {
		return true
	}
	var apiFailure *apiError
	if errors.As(err, &apiFailure) && (apiFailure.transient || apiFailure.Status == http.StatusTooManyRequests || apiFailure.Status >= http.StatusInternalServerError) {
		return true
	}
	return false
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func formatSQLiteTime(value time.Time) string {
	return value.Round(0).UTC().Format(sqliteTimeFormat)
}

// prepareStatement packs parameters into one JSON value. This avoids D1's
// variable-count ceiling and carries BLOBs without converting them to text.
func prepareStatement(statement Statement) (Statement, error) {
	if len(statement.Params) == 0 {
		return Statement{SQL: statement.SQL}, nil
	}
	values := make([]scalar, len(statement.Params))
	for i, value := range statement.Params {
		values[i] = scalar{Value: value}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return Statement{}, fmt.Errorf("encode D1 parameters: %w", err)
	}
	rewritten, used, err := rewriteParameters(statement.SQL, len(statement.Params))
	if err != nil {
		return Statement{}, err
	}
	if used != len(statement.Params) {
		return Statement{}, fmt.Errorf("SQL uses %d parameters, got %d", used, len(statement.Params))
	}
	return Statement{SQL: rewritten, Params: []any{string(encoded)}}, nil
}

func rewriteParameters(query string, count int) (string, int, error) {
	var result strings.Builder
	result.Grow(len(query) + count*80)
	sequential := 0
	maxUsed := 0
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"', '`':
			end := quotedSQL(query, i, query[i])
			result.WriteString(query[i:end])
			i = end
		case '[':
			end := strings.IndexByte(query[i+1:], ']')
			if end < 0 {
				result.WriteString(query[i:])
				i = len(query)
			} else {
				end += i + 2
				result.WriteString(query[i:end])
				i = end
			}
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				end := strings.IndexByte(query[i+2:], '\n')
				if end < 0 {
					result.WriteString(query[i:])
					i = len(query)
				} else {
					end += i + 3
					result.WriteString(query[i:end])
					i = end
				}
				continue
			}
			result.WriteByte(query[i])
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				end := strings.Index(query[i+2:], "*/")
				if end < 0 {
					return "", 0, fmt.Errorf("unterminated SQL comment")
				}
				end += i + 4
				result.WriteString(query[i:end])
				i = end
				continue
			}
			result.WriteByte(query[i])
			i++
		case '?':
			end := i + 1
			for end < len(query) && query[end] >= '0' && query[end] <= '9' {
				end++
			}
			parameter := sequential
			if end > i+1 {
				number, err := strconv.Atoi(query[i+1 : end])
				if err != nil || number <= 0 {
					return "", 0, fmt.Errorf("invalid numbered SQL parameter")
				}
				parameter = number - 1
				if number > maxUsed {
					maxUsed = number
				}
			} else {
				sequential++
				if sequential > maxUsed {
					maxUsed = sequential
				}
			}
			if parameter < 0 || parameter >= count {
				return "", 0, fmt.Errorf("SQL parameter index is out of range")
			}
			path := "$[" + strconv.Itoa(parameter) + "]"
			result.WriteString("CASE WHEN json_type(?1, '")
			result.WriteString(path)
			result.WriteString("') = 'object' THEN unhex(json_extract(?1, '")
			result.WriteString(path)
			result.WriteString(".blob')) ELSE json_extract(?1, '")
			result.WriteString(path)
			result.WriteString("') END")
			i = end
		default:
			result.WriteByte(query[i])
			i++
		}
	}
	return result.String(), maxUsed, nil
}

func quotedSQL(query string, start int, quote byte) int {
	for i := start + 1; i < len(query); i++ {
		if query[i] != quote {
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(query)
}
