package d1

import (
	"database/sql"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

type bufferedRows struct {
	columns []string
	rows    [][]any
	index   int
	current []any
	closed  bool
}

func newBufferedRows(result Result) *bufferedRows {
	rows := make([][]any, len(result.Rows))
	for i, row := range result.Rows {
		rows[i] = make([]any, len(row))
		for j, value := range row {
			rows[i][j] = cloneValue(value)
		}
	}
	return &bufferedRows{columns: append([]string(nil), result.Columns...), rows: rows}
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case []byte:
		return append([]byte(nil), value...)
	case time.Time:
		return value.Round(0).UTC()
	default:
		return value
	}
}

func (r *bufferedRows) Close() error {
	r.closed = true
	r.current = nil
	return nil
}

func (r *bufferedRows) Columns() ([]string, error) {
	return append([]string(nil), r.columns...), nil
}

// D1 does not expose SQLite declared types with query results. Ent falls back
// to scanning unknown column types into *any when this slice is empty.
func (*bufferedRows) ColumnTypes() ([]*sql.ColumnType, error) { return nil, nil }

func (*bufferedRows) Err() error { return nil }

func (r *bufferedRows) Next() bool {
	if r.closed || r.index >= len(r.rows) {
		r.current = nil
		return false
	}
	r.current = r.rows[r.index]
	r.index++
	return true
}

func (*bufferedRows) NextResultSet() bool { return false }

func (r *bufferedRows) Scan(destinations ...any) error {
	if r.current == nil {
		return fmt.Errorf("scan called without a current row")
	}
	if len(destinations) != len(r.current) {
		return fmt.Errorf("scan destination count %d does not match column count %d", len(destinations), len(r.current))
	}
	for i := range destinations {
		if err := assignValue(destinations[i], r.current[i]); err != nil {
			return fmt.Errorf("scan column %d: %w", i, err)
		}
	}
	return nil
}

func assignValue(destination, source any) error {
	if destination == nil {
		return fmt.Errorf("destination is nil")
	}
	if nullTime, ok := destination.(*sql.NullTime); ok {
		value, err := scanTime(source)
		if err != nil {
			return err
		}
		if source == nil {
			*nullTime = sql.NullTime{}
		} else {
			*nullTime = sql.NullTime{Time: value, Valid: true}
		}
		return nil
	}
	if scanner, ok := destination.(sql.Scanner); ok {
		return scanner.Scan(cloneValue(source))
	}
	value := reflect.ValueOf(destination)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("destination %T is not a non-nil pointer", destination)
	}
	return assignReflect(value.Elem(), source)
}

func assignReflect(destination reflect.Value, source any) error {
	if source == nil {
		switch destination.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			destination.SetZero()
			return nil
		default:
			return fmt.Errorf("cannot scan NULL into %s", destination.Type())
		}
	}
	if destination.Kind() == reflect.Interface {
		destination.Set(reflect.ValueOf(cloneValue(source)))
		return nil
	}
	if destination.Kind() == reflect.Pointer {
		if destination.IsNil() {
			destination.Set(reflect.New(destination.Type().Elem()))
		}
		return assignReflect(destination.Elem(), source)
	}
	if destination.Type() == reflect.TypeOf(time.Time{}) {
		value, err := scanTime(source)
		if err != nil {
			return err
		}
		destination.Set(reflect.ValueOf(value))
		return nil
	}
	if destination.Kind() == reflect.Slice && destination.Type().Elem().Kind() == reflect.Uint8 {
		switch source := source.(type) {
		case []byte:
			destination.SetBytes(append([]byte(nil), source...))
		case string:
			destination.SetBytes([]byte(source))
		default:
			return fmt.Errorf("cannot scan %T into %s", source, destination.Type())
		}
		return nil
	}
	text := valueText(source)
	switch destination.Kind() {
	case reflect.String:
		destination.SetString(text)
	case reflect.Bool:
		value, err := strconv.ParseBool(text)
		if err != nil {
			return err
		}
		destination.SetBool(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := strconv.ParseInt(text, 10, destination.Type().Bits())
		if err != nil {
			return err
		}
		destination.SetInt(value)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value, err := strconv.ParseUint(text, 10, destination.Type().Bits())
		if err != nil {
			return err
		}
		destination.SetUint(value)
	case reflect.Float32, reflect.Float64:
		value, err := strconv.ParseFloat(text, destination.Type().Bits())
		if err != nil {
			return err
		}
		destination.SetFloat(value)
	default:
		return fmt.Errorf("cannot scan %T into %s", source, destination.Type())
	}
	return nil
}

func valueText(value any) string {
	if bytes, ok := value.([]byte); ok {
		return string(bytes)
	}
	return fmt.Sprint(value)
}

func scanTime(value any) (time.Time, error) {
	if value == nil {
		return time.Time{}, nil
	}
	if value, ok := value.(time.Time); ok {
		return value.Round(0).UTC(), nil
	}
	return parseSQLiteTime(valueText(value))
}

func parseSQLiteTime(text string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		sqliteTimeFormat,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid SQLite time")
}
