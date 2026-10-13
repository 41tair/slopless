package d1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type localDatabase struct {
	db     *sql.DB
	before *snapshot
}

const sqliteTimeFormat = "2006-01-02 15:04:05.000000000-07:00"

func restoreSnapshot(ctx context.Context, before *snapshot, foreignKeys bool) (*localDatabase, error) {
	normalizeSnapshotTimes(before)
	dsn := "file:d1-" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open local SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	failed := true
	defer func() {
		if failed {
			_ = db.Close()
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("open local SQLite connection: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return nil, fmt.Errorf("disable local foreign keys: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin snapshot restore: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	for _, entry := range before.Schema {
		if entry.Type != "table" {
			continue
		}
		if _, err := tx.ExecContext(ctx, entry.SQL); err != nil {
			return nil, fmt.Errorf("restore table schema: %w", err)
		}
	}
	for _, table := range before.Tables {
		if len(table.Rows) == 0 {
			continue
		}
		query := insertQuery(table)
		for _, row := range table.Rows {
			arguments := make([]any, len(row))
			for i := range row {
				arguments[i] = nativeScalar(row[i])
			}
			if _, err := tx.ExecContext(ctx, query, arguments...); err != nil {
				return nil, fmt.Errorf("restore table rows: %w", err)
			}
		}
	}
	if len(before.Sequences) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence`); err != nil {
			return nil, fmt.Errorf("clear SQLite sequences: %w", err)
		}
		for _, value := range before.Sequences {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)`, value.Name, value.Seq); err != nil {
				return nil, fmt.Errorf("restore SQLite sequence: %w", err)
			}
		}
	}
	for _, entry := range before.Schema {
		if entry.Type == "table" {
			continue
		}
		if _, err := tx.ExecContext(ctx, entry.SQL); err != nil {
			return nil, fmt.Errorf("restore auxiliary schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit snapshot restore: %w", err)
	}
	rollback = false
	foreignKeySetting := "OFF"
	if foreignKeys {
		foreignKeySetting = "ON"
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = `+foreignKeySetting); err != nil {
		return nil, fmt.Errorf("configure local foreign keys: %w", err)
	}
	failed = false
	return &localDatabase{db: db, before: before}, nil
}

func insertQuery(table tableData) string {
	columns := make([]string, len(table.Columns))
	parameters := make([]string, len(table.Columns))
	for i, column := range table.Columns {
		columns[i] = quoteIdentifier(column.Name)
		parameters[i] = "?"
	}
	return "INSERT INTO " + quoteIdentifier(table.Name) + " (" + strings.Join(columns, ", ") +
		") VALUES (" + strings.Join(parameters, ", ") + ")"
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func nativeScalar(value scalar) any {
	if bytes, ok := value.Value.([]byte); ok {
		return append([]byte(nil), bytes...)
	}
	return value.Value
}

func inspectDatabase(ctx context.Context, tx *sql.Tx) ([]tableData, []sequence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, nil, fmt.Errorf("list local tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scan local table: %w", err)
		}
		names = append(names, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, fmt.Errorf("list local tables: %w", err)
	}
	tables := make([]tableData, 0, len(names))
	for _, name := range names {
		table, err := inspectTable(ctx, tx, name)
		if err != nil {
			return nil, nil, err
		}
		tables = append(tables, table)
	}
	sequences, err := inspectSequences(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	return tables, sequences, nil
}

func inspectTable(ctx context.Context, tx *sql.Tx, name string) (tableData, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(name)+")")
	if err != nil {
		return tableData{}, fmt.Errorf("inspect local table: %w", err)
	}
	var columns []columnInfo
	for rows.Next() {
		var (
			cid        int
			column     columnInfo
			notNull    int
			defaultSQL sql.NullString
		)
		if err := rows.Scan(&cid, &column.Name, &column.Type, &notNull, &defaultSQL, &column.PK); err != nil {
			_ = rows.Close()
			return tableData{}, fmt.Errorf("scan local table metadata: %w", err)
		}
		columns = append(columns, column)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return tableData{}, fmt.Errorf("inspect local table: %w", err)
	}
	primary := make([]columnInfo, 0)
	columnNames := make([]string, len(columns))
	for i, column := range columns {
		columnNames[i] = quoteIdentifier(column.Name)
		if column.PK > 0 {
			primary = append(primary, column)
		}
	}
	if len(primary) == 0 {
		return tableData{}, fmt.Errorf("local table %q has no primary key", name)
	}
	sort.Slice(primary, func(i, j int) bool { return primary[i].PK < primary[j].PK })
	order := make([]string, len(primary))
	for i := range primary {
		order[i] = quoteIdentifier(primary[i].Name)
	}
	query := "SELECT " + strings.Join(columnNames, ", ") + " FROM " + quoteIdentifier(name) +
		" ORDER BY " + strings.Join(order, ", ")
	dataRows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return tableData{}, fmt.Errorf("read local table: %w", err)
	}
	tableRows := make([][]scalar, 0)
	for dataRows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := dataRows.Scan(destinations...); err != nil {
			_ = dataRows.Close()
			return tableData{}, fmt.Errorf("scan local table row: %w", err)
		}
		row := make([]scalar, len(values))
		for i, value := range values {
			converted, err := scalarFromNative(value)
			if err != nil {
				_ = dataRows.Close()
				return tableData{}, fmt.Errorf("encode local table row: %w", err)
			}
			row[i] = converted
		}
		tableRows = append(tableRows, row)
	}
	if err := errors.Join(dataRows.Err(), dataRows.Close()); err != nil {
		return tableData{}, fmt.Errorf("read local table: %w", err)
	}
	return tableData{Name: name, Columns: columns, Rows: tableRows}, nil
}

func inspectSequences(ctx context.Context, tx *sql.Tx) ([]sequence, error) {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'sqlite_sequence'
	)`).Scan(&present); err != nil {
		return nil, fmt.Errorf("inspect SQLite sequences: %w", err)
	}
	if !present {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read SQLite sequences: %w", err)
	}
	var result []sequence
	for rows.Next() {
		var value sequence
		if err := rows.Scan(&value.Name, &value.Seq); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan SQLite sequence: %w", err)
		}
		result = append(result, value)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read SQLite sequences: %w", err)
	}
	return result, nil
}

func scalarFromNative(value any) (scalar, error) {
	switch value := value.(type) {
	case nil, bool, string, int64, float64:
		return scalar{Value: value}, nil
	case []byte:
		return scalar{Value: append([]byte(nil), value...)}, nil
	case time.Time:
		return scalar{Value: formatSQLiteTime(value)}, nil
	default:
		return scalar{}, fmt.Errorf("unsupported SQLite value type %T", value)
	}
}

func normalizeSnapshotTimes(value *snapshot) {
	for tableIndex := range value.Tables {
		table := &value.Tables[tableIndex]
		for columnIndex, column := range table.Columns {
			if !isTimeColumn(column.Type) {
				continue
			}
			for rowIndex := range table.Rows {
				text, ok := table.Rows[rowIndex][columnIndex].Value.(string)
				if !ok {
					continue
				}
				parsed, err := parseSQLiteTime(text)
				if err == nil {
					table.Rows[rowIndex][columnIndex].Value = formatSQLiteTime(parsed)
				}
			}
		}
	}
}

func isTimeColumn(declaredType string) bool {
	declaredType = strings.ToUpper(declaredType)
	return strings.Contains(declaredType, "DATE") || strings.Contains(declaredType, "TIME")
}

func formatSQLiteTime(value time.Time) string {
	return value.Round(0).UTC().Format(sqliteTimeFormat)
}

func diffTables(before, after []tableData) ([]change, error) {
	oldTables := make(map[string]tableData, len(before))
	for _, table := range before {
		oldTables[table.Name] = table
	}
	changes := make([]change, 0)
	for _, current := range after {
		previous, exists := oldTables[current.Name]
		if !exists {
			previous = tableData{Name: current.Name, Columns: current.Columns}
		}
		value, err := diffTable(previous, current)
		if err != nil {
			return nil, err
		}
		if len(value.Rows) > 0 || len(value.Deleted) > 0 {
			changes = append(changes, value)
		}
	}
	return changes, nil
}

var (
	renameTablePattern = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\s+[` + "`\"" + `]?([A-Za-z_][A-Za-z0-9_]*)[` + "`\"" + `]?\s+RENAME\s+TO\s+[` + "`\"" + `]?([A-Za-z_][A-Za-z0-9_]*)`)
	dropTablePattern   = regexp.MustCompile(`(?i)^\s*DROP\s+TABLE(?:\s+IF\s+EXISTS)?\s+[` + "`\"" + `]?([A-Za-z_][A-Za-z0-9_]*)`)
)

func fullRowsForDDL(changes []change, after []tableData, ddl []string) ([]change, error) {
	byTable := make(map[string]change, len(changes))
	for _, value := range changes {
		byTable[value.Table] = value
	}
	force := rebuiltTables(ddl)
	result := make([]change, 0, len(after))
	for _, table := range after {
		value, changed := byTable[table.Name]
		if force[table.Name] {
			full, err := diffTable(tableData{Name: table.Name, Columns: table.Columns}, table)
			if err != nil {
				return nil, err
			}
			full.Deleted = value.Deleted
			value, changed = full, len(full.Rows) > 0 || len(full.Deleted) > 0
		}
		if changed {
			result = append(result, value)
		}
		delete(byTable, table.Name)
	}
	// A table omitted from the final schema is handled by its DROP statement.
	return result, nil
}

func rebuiltTables(ddl []string) map[string]bool {
	dropped := make(map[string]bool)
	result := make(map[string]bool)
	for _, statement := range ddl {
		if match := dropTablePattern.FindStringSubmatch(statement); len(match) == 2 {
			dropped[match[1]] = true
		}
		if match := renameTablePattern.FindStringSubmatch(statement); len(match) == 3 && match[1] != match[2] {
			result[match[2]] = true
		}
	}
	for table := range dropped {
		result[table] = true
	}
	return result
}

func diffTable(before, after tableData) (change, error) {
	columns := make([]string, len(after.Columns))
	primaryIndexes := make([]int, 0)
	primaryNames := make([]string, 0)
	primary := append([]columnInfo(nil), after.Columns...)
	sort.SliceStable(primary, func(i, j int) bool {
		if primary[i].PK == 0 {
			return false
		}
		if primary[j].PK == 0 {
			return true
		}
		return primary[i].PK < primary[j].PK
	})
	for i, column := range after.Columns {
		columns[i] = column.Name
	}
	for _, column := range primary {
		if column.PK == 0 {
			break
		}
		for index := range after.Columns {
			if after.Columns[index].Name == column.Name {
				primaryIndexes = append(primaryIndexes, index)
				primaryNames = append(primaryNames, column.Name)
				break
			}
		}
	}
	if len(primaryIndexes) == 0 {
		return change{}, fmt.Errorf("table %q has no primary key", after.Name)
	}
	old := make(map[string][]scalar, len(before.Rows))
	for _, row := range before.Rows {
		keyIndexes, ok := mapPrimaryIndexes(before.Columns, after.Columns, primaryIndexes)
		if !ok {
			continue
		}
		key, err := scalarKey(selectScalars(row, keyIndexes))
		if err != nil {
			return change{}, err
		}
		old[key] = row
	}
	result := change{Table: after.Name, Columns: columns, PrimaryKey: primaryNames}
	for _, row := range after.Rows {
		keyValues := selectScalars(row, primaryIndexes)
		key, err := scalarKey(keyValues)
		if err != nil {
			return change{}, err
		}
		oldRow, exists := old[key]
		if !exists || !sameColumns(before.Columns, after.Columns) || !sameRow(oldRow, row) {
			result.Rows = append(result.Rows, row)
		}
		delete(old, key)
	}
	keys := make([]string, 0, len(old))
	for key := range old {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	oldPrimaryIndexes, mapped := mapPrimaryIndexes(before.Columns, after.Columns, primaryIndexes)
	if mapped {
		for _, key := range keys {
			result.Deleted = append(result.Deleted, selectScalars(old[key], oldPrimaryIndexes))
		}
	}
	return result, nil
}

func mapPrimaryIndexes(before, after []columnInfo, afterIndexes []int) ([]int, bool) {
	indexes := make([]int, 0, len(afterIndexes))
	for _, afterIndex := range afterIndexes {
		name := after[afterIndex].Name
		found := false
		for i := range before {
			if before[i].Name == name {
				indexes = append(indexes, i)
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return indexes, true
}

func selectScalars(row []scalar, indexes []int) []scalar {
	result := make([]scalar, len(indexes))
	for i, index := range indexes {
		result[i] = row[index]
	}
	return result
}

func scalarKey(values []scalar) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode primary key: %w", err)
	}
	return string(encoded), nil
}

func sameColumns(left, right []columnInfo) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name || left[i].PK != right[i].PK ||
			!strings.EqualFold(left[i].Type, right[i].Type) {
			return false
		}
	}
	return true
}

func sameRow(left, right []scalar) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !sameScalar(left[i], right[i]) {
			return false
		}
	}
	return true
}

func sameScalar(left, right scalar) bool {
	switch left := left.Value.(type) {
	case []byte:
		right, ok := right.Value.([]byte)
		return ok && bytes.Equal(left, right)
	default:
		return left == right.Value
	}
}

func sameSequences(left, right []sequence) bool {
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
