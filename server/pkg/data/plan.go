package data

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/41tair/slopless/pkg/d1"
)

// A plan contains only records touched by the business operation. Mutations
// with the same table/column shape are compiled into one set-based statement.
type mutationPlan struct {
	base     *Store
	now      time.Time
	users    *userPlan
	accounts *accountPlan
	rates    *ratePlan
	rows     map[string]map[int]*rowMutation
	ids      map[string]*idBlock
	extra    []d1.Statement
}

type rowMutation struct {
	created bool
	deleted bool
	fields  map[string]any
}

type idBlock struct{ next, end, size int }

func (p *mutationPlan) nextID(ctx context.Context, table string) (int, error) {
	if p.ids == nil {
		p.ids = make(map[string]*idBlock)
	}
	block := p.ids[table]
	if block == nil {
		block = &idBlock{size: 16}
		p.ids[table] = block
	}
	if block.next == block.end {
		first, err := p.base.driver.ReserveIDs(ctx, table, block.size)
		if err != nil {
			return 0, databaseError(err)
		}
		block.next, block.end = first, first+block.size
		block.size = min(block.size*2, 256)
	}
	id := block.next
	block.next++
	return id, nil
}

func (p *mutationPlan) row(table string, id int) *rowMutation {
	if p.rows == nil {
		p.rows = make(map[string]map[int]*rowMutation)
	}
	if p.rows[table] == nil {
		p.rows[table] = make(map[int]*rowMutation)
	}
	row := p.rows[table][id]
	if row == nil {
		row = &rowMutation{fields: make(map[string]any)}
		p.rows[table][id] = row
	}
	return row
}

func (p *mutationPlan) insert(table string, id int, fields map[string]any) {
	row := p.row(table, id)
	row.created, row.deleted = true, false
	row.fields = make(map[string]any, len(fields)+1)
	row.fields["id"] = id
	for name, value := range fields {
		row.fields[name] = copyPlanValue(value)
	}
}

func (p *mutationPlan) update(table string, id int, fields map[string]any) {
	row := p.row(table, id)
	for name, value := range fields {
		row.fields[name] = copyPlanValue(value)
	}
}

func (p *mutationPlan) delete(table string, id int) {
	row := p.row(table, id)
	if row.created {
		delete(p.rows[table], id)
		return
	}
	row.deleted = true
	row.fields = nil
}

func (p *mutationPlan) cancel(table string, id int) {
	delete(p.rows[table], id)
}

func copyPlanValue(value any) any {
	if value == nil {
		return nil
	}
	switch value := value.(type) {
	case []byte:
		return slices.Clone(value)
	case time.Time:
		return value.Round(0)
	}
	ref := reflect.ValueOf(value)
	if ref.Kind() == reflect.Pointer {
		if ref.IsNil() {
			return nil
		}
		return copyPlanValue(ref.Elem().Interface())
	}
	return value
}

var tableColumns = map[string][]string{
	"users":          {"id", "username", "password_hash", "credits", "created_at", "invitation_id", "inviter_id"},
	"invitations":    {"id", "code", "purchase_key", "cost", "created_at", "used_at", "issuer_id"},
	"accounts":       {"id", "platform", "username", "rewarded_confirmations", "awarded_credits", "created_at", "first_marker_id"},
	"confirmations":  {"id", "branch_id", "created_at", "rewarded_at", "account_id", "user_id"},
	"credentials":    {"id", "kind", "name", "secret_hash", "created_at", "expires_at", "user_id"},
	"credit_entries": {"id", "amount", "balance_after", "reason", "created_at", "account_id", "confirmation_id", "invitation_id", "user_id"},
	"idempotencies":  {"id", "key", "fingerprint", "response", "expires_at", "user_id"},
	"marks":          {"id", "platform", "username", "user_id"},
	"rate_events":    {"id", "policy", "subject", "units", "expires_at"},
}

type mutationGroup struct {
	table   string
	created bool
	columns []string
	rows    []map[string]any
}

func (p *mutationPlan) statements() ([]d1.Statement, error) {
	statements := slices.Clone(p.extra)
	tables := make([]string, 0, len(p.rows))
	for table := range p.rows {
		if _, ok := tableColumns[table]; !ok {
			return nil, fmt.Errorf("invalid mutation table %q", table)
		}
		tables = append(tables, table)
	}
	slices.Sort(tables)
	var groups []*mutationGroup
	for _, table := range tables {
		ids := make([]int, 0, len(p.rows[table]))
		for id := range p.rows[table] {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		var deleted []int
		byShape := make(map[string]*mutationGroup)
		for _, id := range ids {
			row := p.rows[table][id]
			if row.deleted {
				deleted = append(deleted, id)
				continue
			}
			if len(row.fields) == 0 {
				continue
			}
			columns := make([]string, 0, len(row.fields))
			for column := range row.fields {
				if !slices.Contains(tableColumns[table], column) {
					return nil, fmt.Errorf("invalid mutation column %q.%q", table, column)
				}
				columns = append(columns, column)
			}
			slices.Sort(columns)
			shape := strconv.FormatBool(row.created) + ":" + strings.Join(columns, ",")
			group := byShape[shape]
			if group == nil {
				group = &mutationGroup{table: table, created: row.created, columns: columns}
				byShape[shape] = group
				groups = append(groups, group)
			}
			values := make(map[string]any, len(row.fields)+1)
			for name, value := range row.fields {
				normalized, err := normalizePlanValue(value)
				if err != nil {
					return nil, err
				}
				values[name] = normalized
			}
			values["id"] = id
			group.rows = append(group.rows, values)
		}
		if len(deleted) > 0 {
			encoded, err := json.Marshal(deleted)
			if err != nil {
				return nil, err
			}
			statements = append(statements, d1.Statement{
				SQL: `DELETE FROM "` + table + `" WHERE "id" IN (SELECT value FROM json_each(?))`, Params: []any{string(encoded)},
			})
		}
	}
	// Deletes precede replacement inserts for natural-key uniqueness. All foreign
	// keys are checked at the end of the atomic D1 batch.
	for _, group := range groups {
		statement, err := group.statement()
		if err != nil {
			return nil, err
		}
		statements = append(statements, statement)
	}
	return statements, nil
}

func normalizePlanValue(value any) (any, error) {
	value = copyPlanValue(value)
	switch value := value.(type) {
	case nil, string, bool:
		return value, nil
	case time.Time:
		return value.UTC().Format("2006-01-02 15:04:05.000000000-07:00"), nil
	case []byte:
		return map[string]string{"blob": hex.EncodeToString(value)}, nil
	case int:
		return safePlanInteger(int64(value))
	case int64:
		return safePlanInteger(value)
	default:
		return nil, fmt.Errorf("unsupported mutation field type %T", value)
	}
}

func safePlanInteger(value int64) (any, error) {
	if value < -9007199254740991 || value > 9007199254740991 {
		return nil, errorsUnsafeInteger(value)
	}
	return value, nil
}

func errorsUnsafeInteger(value int64) error {
	return fmt.Errorf("mutation integer %d exceeds D1's exact integer range", value)
}

func (g *mutationGroup) statement() (d1.Statement, error) {
	encoded, err := json.Marshal(g.rows)
	if err != nil {
		return d1.Statement{}, err
	}
	columns := make([]string, len(g.columns))
	values := make([]string, len(g.columns))
	for i, column := range g.columns {
		columns[i] = `"` + column + `"`
		path := "$." + column
		values[i] = `CASE WHEN json_type(j.value, '` + path + `') = 'object' THEN unhex(json_extract(j.value, '` + path + `.blob')) ELSE json_extract(j.value, '` + path + `') END`
	}
	var query string
	if g.created {
		query = `INSERT INTO "` + g.table + `" (` + strings.Join(columns, ",") + `) SELECT ` + strings.Join(values, ",") + ` FROM json_each(?) AS j`
	} else {
		assignments := make([]string, len(columns))
		for i := range columns {
			assignments[i] = columns[i] + " = " + values[i]
		}
		query = `UPDATE "` + g.table + `" SET ` + strings.Join(assignments, ",") + ` FROM json_each(?) AS j WHERE "` + g.table + `"."id" = json_extract(j.value, '$.id')`
	}
	return d1.Statement{SQL: query, Params: []any{string(encoded)}}, nil
}
