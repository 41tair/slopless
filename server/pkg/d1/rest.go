package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	commitRetentionSeconds = int64(72 * 60 * 60)
	maxSnapshotAttempts    = 3

	schemaSQL = `SELECT type, name, tbl_name AS "table", sql
		FROM sqlite_schema
		WHERE type IN ('table', 'index', 'trigger')
			AND sql IS NOT NULL
			AND tbl_name IN (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END, name`
)

var (
	applicationTables = []string{
		"accounts",
		"confirmations",
		"credentials",
		"credit_entries",
		"idempotencies",
		"invitations",
		"marks",
		"rate_events",
		"users",
	}
	applicationTableSet = map[string]bool{
		"accounts":       true,
		"confirmations":  true,
		"credentials":    true,
		"credit_entries": true,
		"idempotencies":  true,
		"invitations":    true,
		"marks":          true,
		"rate_events":    true,
		"users":          true,
	}

	identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	txIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

func (c *client) snapshot(ctx context.Context) (*snapshot, error) {
	if err := c.ensureControlSchema(ctx); err != nil {
		return nil, fmt.Errorf("%w: initialize D1 metadata: %w", ErrUnavailable, err)
	}

	var lastErr error
	for range maxSnapshotAttempts {
		value, consistent, err := c.snapshotAttempt(ctx)
		if err == nil && consistent {
			if err := validateSnapshot(value); err != nil {
				return nil, fmt.Errorf("%w: validate snapshot: %w", ErrUnavailable, err)
			}
			return value, nil
		}
		if err != nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: load snapshot: %w", ErrUnavailable, ctx.Err())
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: load consistent snapshot: %w", ErrUnavailable, lastErr)
	}
	return nil, fmt.Errorf("%w: D1 changed while loading the snapshot", ErrUnavailable)
}

func (c *client) snapshotAttempt(ctx context.Context) (*snapshot, bool, error) {
	beforeResults, err := c.query(ctx, []statement{schemaStatement()})
	if err != nil {
		return nil, false, err
	}
	before, err := decodeSchema(beforeResults[0])
	if err != nil {
		return nil, false, err
	}

	statements, tableNames, includesSequences := snapshotStatements(before)
	results, err := c.query(ctx, statements)
	if err != nil {
		return nil, false, err
	}
	value, err := decodeSnapshot(results, tableNames, includesSequences)
	if err != nil {
		return nil, false, err
	}
	if !sameSchema(before, value.Schema) {
		return nil, false, nil
	}
	return value, true, nil
}

func schemaStatement() statement {
	return statement{SQL: schemaSQL, Params: applicationTableParams()}
}

func applicationTableParams() []any {
	params := make([]any, len(applicationTables))
	for i, table := range applicationTables {
		params[i] = table
	}
	return params
}

func snapshotStatements(schema []schemaEntry) ([]statement, []string, bool) {
	tableNames := make([]string, 0, len(schema))
	statements := []statement{
		{SQL: `SELECT "version" FROM "__slopless_meta" WHERE "id" = 1`},
		schemaStatement(),
	}
	for _, entry := range schema {
		if entry.Type != "table" {
			continue
		}
		tableNames = append(tableNames, entry.Name)
		quoted := quoteIdentifier(entry.Name)
		statements = append(statements,
			statement{SQL: "PRAGMA table_info(" + quoted + ")"},
			statement{SQL: "SELECT * FROM " + quoted + ` ORDER BY "id"`},
		)
	}
	includesSequences := schemaUsesAutoIncrement(schema)
	if includesSequences {
		statements = append(statements, statement{
			SQL: `SELECT name, seq FROM sqlite_sequence
				WHERE name IN (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ORDER BY name`,
			Params: applicationTableParams(),
		})
	}
	return statements, tableNames, includesSequences
}

func decodeSnapshot(results []queryResult, tableNames []string, includesSequences bool) (*snapshot, error) {
	expectedResults := 2 + 2*len(tableNames)
	if includesSequences {
		expectedResults++
	}
	if len(results) != expectedResults {
		return nil, fmt.Errorf("invalid snapshot result count")
	}
	version, err := resultInt64(results[0], 0, "version")
	if err != nil {
		return nil, err
	}
	schema, err := decodeSchema(results[1])
	if err != nil {
		return nil, err
	}

	tables := make([]tableData, 0, len(tableNames))
	resultIndex := 2
	for _, tableName := range tableNames {
		table, err := decodeTable(tableName, results[resultIndex], results[resultIndex+1])
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
		resultIndex += 2
	}
	sequences := []sequence{}
	if includesSequences {
		sequences, err = decodeSequences(results[resultIndex])
		if err != nil {
			return nil, err
		}
	}
	return &snapshot{
		Version:   version,
		Schema:    schema,
		Tables:    tables,
		Sequences: sequences,
	}, nil
}

func schemaUsesAutoIncrement(schema []schemaEntry) bool {
	for _, entry := range schema {
		if entry.Type == "table" && strings.Contains(strings.ToUpper(entry.SQL), "AUTOINCREMENT") {
			return true
		}
	}
	return false
}

func decodeSchema(result queryResult) ([]schemaEntry, error) {
	schema := make([]schemaEntry, 0, len(result.Results))
	for _, row := range result.Results {
		typeName, err := rowString(row, "type")
		if err != nil {
			return nil, err
		}
		name, err := rowString(row, "name")
		if err != nil {
			return nil, err
		}
		table, err := rowString(row, "table")
		if err != nil {
			return nil, err
		}
		sql, err := rowString(row, "sql")
		if err != nil {
			return nil, err
		}
		if !applicationTableSet[table] || (typeName == "table" && !applicationTableSet[name]) {
			return nil, fmt.Errorf("invalid application schema entry")
		}
		schema = append(schema, schemaEntry{Type: typeName, Name: name, Table: table, SQL: sql})
	}
	return schema, nil
}

func decodeTable(name string, metadata, data queryResult) (tableData, error) {
	if !applicationTableSet[name] {
		return tableData{}, fmt.Errorf("invalid application table")
	}
	columns := make([]columnInfo, 0, len(metadata.Results))
	for _, row := range metadata.Results {
		columnName, err := rowString(row, "name")
		if err != nil {
			return tableData{}, err
		}
		declaredType, err := rowString(row, "type")
		if err != nil {
			return tableData{}, err
		}
		primaryKey, err := rowInt64(row, "pk")
		if err != nil || primaryKey < 0 || primaryKey > int64(len(metadata.Results)) {
			return tableData{}, fmt.Errorf("invalid table metadata")
		}
		if !identifierPattern.MatchString(columnName) {
			return tableData{}, fmt.Errorf("invalid table column")
		}
		columns = append(columns, columnInfo{Name: columnName, Type: declaredType, PK: int(primaryKey)})
	}

	rows := make([][]scalar, 0, len(data.Results))
	for _, rawRow := range data.Results {
		row := make([]scalar, len(columns))
		for i, column := range columns {
			raw, ok := rawRow[column.Name]
			if !ok {
				return tableData{}, fmt.Errorf("D1 omitted a table column")
			}
			value, err := decodeD1Scalar(raw)
			if err != nil {
				return tableData{}, fmt.Errorf("decode D1 table value: %w", err)
			}
			row[i] = value
		}
		rows = append(rows, row)
	}
	return tableData{Name: name, Columns: columns, Rows: rows}, nil
}

func decodeSequences(result queryResult) ([]sequence, error) {
	sequences := make([]sequence, 0, len(result.Results))
	for _, row := range result.Results {
		name, err := rowString(row, "name")
		if err != nil || !applicationTableSet[name] {
			return nil, fmt.Errorf("invalid SQLite sequence")
		}
		value, err := rowInt64(row, "seq")
		if err != nil || value < 0 || value > maxSafeInteger {
			return nil, fmt.Errorf("invalid SQLite sequence")
		}
		sequences = append(sequences, sequence{Name: name, Seq: value})
	}
	return sequences, nil
}

func decodeD1Scalar(raw json.RawMessage) (scalar, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return scalar{}, fmt.Errorf("empty D1 scalar")
	}
	if trimmed[0] != '[' {
		var value scalar
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return scalar{}, err
		}
		return value, nil
	}

	var rawBytes []json.RawMessage
	if err := json.Unmarshal(trimmed, &rawBytes); err != nil {
		return scalar{}, fmt.Errorf("decode D1 blob: %w", err)
	}
	value := make([]byte, len(rawBytes))
	for i, rawByte := range rawBytes {
		integer, err := rawJSONInt64(rawByte)
		if err != nil || integer < 0 || integer > math.MaxUint8 {
			return scalar{}, fmt.Errorf("invalid D1 blob")
		}
		value[i] = byte(integer)
	}
	return scalar{Value: value}, nil
}

func resultInt64(result queryResult, rowIndex int, key string) (int64, error) {
	if rowIndex < 0 || rowIndex >= len(result.Results) {
		return 0, fmt.Errorf("D1 result is missing row %d", rowIndex)
	}
	return rowInt64(result.Results[rowIndex], key)
}

func rowInt64(row map[string]json.RawMessage, key string) (int64, error) {
	raw, ok := row[key]
	if !ok {
		return 0, fmt.Errorf("D1 result is missing column %q", key)
	}
	return rawJSONInt64(raw)
}

func rawJSONInt64(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return 0, fmt.Errorf("decode D1 integer: %w", err)
	}
	value, err := number.Int64()
	if err != nil || value < -maxSafeInteger || value > maxSafeInteger {
		return 0, fmt.Errorf("invalid D1 integer")
	}
	return value, nil
}

func rowString(row map[string]json.RawMessage, key string) (string, error) {
	raw, ok := row[key]
	if !ok {
		return "", fmt.Errorf("D1 result is missing column %q", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decode D1 text column %q: %w", key, err)
	}
	return value, nil
}

func sameSchema(left, right []schemaEntry) bool {
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

func (c *client) commit(ctx context.Context, request commitRequest) error {
	normalizeCommit(&request)
	if err := validateCommit(&request); err != nil {
		return fmt.Errorf("%w: validate commit: %w", ErrUnavailable, err)
	}
	if err := c.ensureControlSchema(ctx); err != nil {
		return fmt.Errorf("%w: initialize D1 metadata: %w", ErrUnavailable, err)
	}
	statements, err := commitStatements(request, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("%w: build D1 commit: %w", ErrUnavailable, err)
	}

	var commitErr error
	for attempt := range commitAttempts {
		_, commitErr = c.query(ctx, statements)
		if commitErr == nil {
			return nil
		}

		committed, statusErr := c.resolveCommit(ctx, request.TxID)
		if statusErr == nil && committed {
			return nil
		}
		if isRESTClientFailure(commitErr) {
			conflict, revisionErr := c.commitRevisionChanged(ctx, request.Version)
			if revisionErr == nil && conflict {
				return fmt.Errorf("%w: base revision changed", ErrConflict)
			}
			return fmt.Errorf("%w: commit rejected: %w", ErrUnavailable, commitErr)
		}
		if attempt+1 < commitAttempts {
			if waitErr := waitForRetry(ctx, attempt); waitErr != nil {
				return fmt.Errorf("%w: commit outcome is unknown: %w", ErrUnavailable, waitErr)
			}
		}
	}
	return fmt.Errorf("%w: commit outcome is unknown: %w", ErrUnavailable, commitErr)
}

func normalizeCommit(request *commitRequest) {
	if request.DDL == nil {
		request.DDL = []string{}
	}
	if request.Changes == nil {
		request.Changes = []change{}
	}
	if request.Sequences == nil {
		request.Sequences = []sequence{}
	}
	for i := range request.Changes {
		if request.Changes[i].Rows == nil {
			request.Changes[i].Rows = [][]scalar{}
		}
		if request.Changes[i].Deleted == nil {
			request.Changes[i].Deleted = [][]scalar{}
		}
	}
}

func validateCommit(request *commitRequest) error {
	if request.Version < 0 || request.Version >= maxSafeInteger {
		return fmt.Errorf("commit revision is out of range")
	}
	if !txIDPattern.MatchString(request.TxID) {
		return fmt.Errorf("invalid transaction ID")
	}

	seenTables := make(map[string]bool, len(request.Changes))
	for _, value := range request.Changes {
		if !applicationTableSet[value.Table] || seenTables[value.Table] {
			return fmt.Errorf("each application table may appear once per commit")
		}
		seenTables[value.Table] = true
		if err := validateChange(value); err != nil {
			return err
		}
	}

	seenSequences := make(map[string]bool, len(request.Sequences))
	for _, value := range request.Sequences {
		if !applicationTableSet[value.Name] || seenSequences[value.Name] {
			return fmt.Errorf("invalid SQLite sequence table")
		}
		if value.Seq < 0 || value.Seq > maxSafeInteger {
			return fmt.Errorf("SQLite sequence is out of range")
		}
		seenSequences[value.Name] = true
	}
	return nil
}

func validateChange(value change) error {
	if len(value.Columns) == 0 || !uniqueIdentifiers(value.Columns) {
		return fmt.Errorf("invalid changed-table columns")
	}
	if len(value.PrimaryKey) == 0 || !uniqueIdentifiers(value.PrimaryKey) {
		return fmt.Errorf("invalid changed-table primary key")
	}
	columnSet := make(map[string]bool, len(value.Columns))
	for _, column := range value.Columns {
		columnSet[column] = true
	}
	for _, column := range value.PrimaryKey {
		if !columnSet[column] {
			return fmt.Errorf("changed-table primary key is not a table column")
		}
	}
	for _, row := range value.Rows {
		if len(row) != len(value.Columns) {
			return fmt.Errorf("changed row does not contain every table column")
		}
		if err := validateScalars(row); err != nil {
			return err
		}
	}
	for _, key := range value.Deleted {
		if len(key) != len(value.PrimaryKey) {
			return fmt.Errorf("deleted row does not contain every primary-key column")
		}
		if err := validateScalars(key); err != nil {
			return err
		}
	}
	return nil
}

func uniqueIdentifiers(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !identifierPattern.MatchString(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validateScalars(values []scalar) error {
	for _, value := range values {
		if _, err := json.Marshal(value); err != nil {
			return fmt.Errorf("invalid database value: %w", err)
		}
	}
	return nil
}

func commitStatements(request commitRequest, now int64) ([]statement, error) {
	statements := []statement{
		{SQL: `PRAGMA defer_foreign_keys = ON`},
		{
			SQL: `UPDATE "__slopless_meta"
				SET "version" = CASE WHEN "version" = ? THEN "version" + 1 ELSE NULL END
				WHERE "id" = 1`,
			Params: []any{request.Version},
		},
	}
	for _, sql := range request.DDL {
		statements = append(statements, statement{SQL: sql})
	}

	changes := make(map[string]change, len(request.Changes))
	for _, value := range request.Changes {
		changes[value.Table] = value
	}
	deleteOrder := []string{
		"credit_entries", "confirmations", "idempotencies", "credentials", "marks",
		"rate_events", "accounts", "users", "invitations",
	}
	for _, table := range deleteOrder {
		value, ok := changes[table]
		if !ok || len(value.Deleted) == 0 {
			continue
		}
		entry, err := deleteStatement(value)
		if err != nil {
			return nil, err
		}
		statements = append(statements, entry)
	}

	upsertOrder := []string{
		"users", "invitations", "accounts", "credentials", "marks", "idempotencies",
		"confirmations", "credit_entries", "rate_events",
	}
	for _, table := range upsertOrder {
		value, ok := changes[table]
		if !ok || len(value.Rows) == 0 {
			continue
		}
		entry, err := upsertStatement(value)
		if err != nil {
			return nil, err
		}
		statements = append(statements, entry)
	}

	if len(request.Sequences) > 0 || ddlCreatesAutoIncrement(request.DDL) {
		statements = append(statements, statement{
			SQL:    `DELETE FROM sqlite_sequence WHERE name IN (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			Params: applicationTableParams(),
		})
		if len(request.Sequences) > 0 {
			encoded, err := encodeSequences(request.Sequences)
			if err != nil {
				return nil, err
			}
			statements = append(statements, statement{
				SQL: `INSERT INTO sqlite_sequence(name, seq)
					SELECT json_extract(sequence_row.value, '$[0]'),
						json_extract(sequence_row.value, '$[1]')
					FROM json_each(?) AS sequence_row`,
				Params: []any{encoded},
			})
		}
	}

	statements = append(statements,
		statement{
			SQL: `INSERT INTO "__slopless_commits" ("txid", "version", "created_at")
				VALUES (?, ?, ?)`,
			Params: []any{request.TxID, request.Version + 1, now},
		},
		statement{
			SQL:    `DELETE FROM "__slopless_commits" WHERE "created_at" < ?`,
			Params: []any{now - commitRetentionSeconds},
		},
	)
	return statements, nil
}

func deleteStatement(value change) (statement, error) {
	encoded, err := json.Marshal(value.Deleted)
	if err != nil {
		return statement{}, fmt.Errorf("encode deleted rows: %w", err)
	}
	table := quoteIdentifier(value.Table)
	predicates := make([]string, len(value.PrimaryKey))
	for i, column := range value.PrimaryKey {
		predicates[i] = table + "." + quoteIdentifier(column) + " IS " + jsonValueExpression("deleted_row", i)
	}
	return statement{
		SQL: `DELETE FROM ` + table + `
			WHERE EXISTS (
				SELECT 1 FROM json_each(?) AS deleted_row
				WHERE ` + strings.Join(predicates, " AND ") + `
			)`,
		Params: []any{string(encoded)},
	}, nil
}

func upsertStatement(value change) (statement, error) {
	encoded, err := json.Marshal(value.Rows)
	if err != nil {
		return statement{}, fmt.Errorf("encode changed rows: %w", err)
	}
	table := quoteIdentifier(value.Table)
	columns := make([]string, len(value.Columns))
	values := make([]string, len(value.Columns))
	for i, column := range value.Columns {
		columns[i] = quoteIdentifier(column)
		values[i] = jsonValueExpression("changed_row", i)
	}
	primaryKey := make([]string, len(value.PrimaryKey))
	primarySet := make(map[string]bool, len(value.PrimaryKey))
	for i, column := range value.PrimaryKey {
		primaryKey[i] = quoteIdentifier(column)
		primarySet[column] = true
	}
	updates := make([]string, 0, len(value.Columns)-len(value.PrimaryKey))
	changed := make([]string, 0, len(value.Columns)-len(value.PrimaryKey))
	for _, column := range value.Columns {
		if primarySet[column] {
			continue
		}
		quoted := quoteIdentifier(column)
		updates = append(updates, quoted+" = excluded."+quoted)
		changed = append(changed, "excluded."+quoted+" IS NOT "+table+"."+quoted)
	}
	conflict := "DO NOTHING"
	if len(updates) > 0 {
		conflict = "DO UPDATE SET " + strings.Join(updates, ", ") + " WHERE " + strings.Join(changed, " OR ")
	}
	return statement{
		SQL: `INSERT INTO ` + table + ` (` + strings.Join(columns, ", ") + `)
			SELECT ` + strings.Join(values, ", ") + ` FROM json_each(?) AS changed_row WHERE TRUE
			ON CONFLICT (` + strings.Join(primaryKey, ", ") + `) ` + conflict,
		Params: []any{string(encoded)},
	}, nil
}

func jsonValueExpression(rowAlias string, index int) string {
	path := fmt.Sprintf("$[%d]", index)
	return "CASE WHEN json_type(" + rowAlias + ".value, '" + path + "') = 'object' " +
		"THEN unhex(json_extract(" + rowAlias + ".value, '" + path + ".blob')) " +
		"ELSE json_extract(" + rowAlias + ".value, '" + path + "') END"
}

func encodeSequences(values []sequence) (string, error) {
	rows := make([][]any, len(values))
	for i, value := range values {
		rows[i] = []any{value.Name, value.Seq}
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return "", fmt.Errorf("encode SQLite sequences: %w", err)
	}
	return string(encoded), nil
}

func ddlCreatesAutoIncrement(ddl []string) bool {
	for _, sql := range ddl {
		if strings.Contains(strings.ToUpper(sql), "AUTOINCREMENT") {
			return true
		}
	}
	return false
}

func (c *client) ensureControlSchema(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if c.schemaReady {
		return nil
	}
	_, err := c.query(ctx, []statement{
		{SQL: `CREATE TABLE IF NOT EXISTS "__slopless_meta" (
			"id" INTEGER NOT NULL PRIMARY KEY CHECK ("id" = 1),
			"version" INTEGER NOT NULL
		)`},
		{SQL: `INSERT OR IGNORE INTO "__slopless_meta" ("id", "version") VALUES (1, 0)`},
		{SQL: `CREATE TABLE IF NOT EXISTS "__slopless_commits" (
			"txid" TEXT NOT NULL PRIMARY KEY,
			"version" INTEGER NOT NULL,
			"created_at" INTEGER NOT NULL
		) WITHOUT ROWID`},
	})
	if err != nil {
		return err
	}
	c.schemaReady = true
	return nil
}

func (c *client) committed(ctx context.Context, txID string) (bool, error) {
	if !txIDPattern.MatchString(txID) {
		return false, nil
	}
	if err := c.ensureControlSchema(ctx); err != nil {
		return false, err
	}
	return c.committedWithoutSchema(ctx, txID)
}

func (c *client) committedWithoutSchema(ctx context.Context, txID string) (bool, error) {
	results, err := c.query(ctx, []statement{{
		SQL:    `SELECT "txid" FROM "__slopless_commits" WHERE "txid" = ?`,
		Params: []any{txID},
	}})
	if err != nil {
		return false, err
	}
	if len(results[0].Results) == 0 {
		return false, nil
	}
	if len(results[0].Results) != 1 {
		return false, fmt.Errorf("invalid D1 commit status")
	}
	committedTxID, err := rowString(results[0].Results[0], "txid")
	if err != nil {
		return false, err
	}
	if committedTxID != txID {
		return false, fmt.Errorf("invalid D1 commit status")
	}
	return true, nil
}

func (c *client) resolveCommit(ctx context.Context, txID string) (bool, error) {
	committed, err := c.committed(ctx, txID)
	if err == nil || ctx.Err() == nil {
		return committed, err
	}
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return c.committed(resolveCtx, txID)
}

func (c *client) commitRevisionChanged(ctx context.Context, version int64) (bool, error) {
	results, err := c.query(ctx, []statement{{
		SQL: `SELECT "version" FROM "__slopless_meta" WHERE "id" = 1`,
	}})
	if err != nil {
		return false, err
	}
	current, err := resultInt64(results[0], 0, "version")
	if err != nil {
		return false, err
	}
	return current != version, nil
}

func isRESTClientFailure(err error) bool {
	var failure *apiError
	if !errors.As(err, &failure) || failure.Status == http.StatusTooManyRequests {
		return false
	}
	statementFailure := failure.Status >= http.StatusOK && failure.Status < http.StatusMultipleChoices
	clientFailure := failure.Status >= http.StatusBadRequest && failure.Status < http.StatusInternalServerError
	return statementFailure || clientFailure
}
