package exportfmt

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// SealedMarker stands for an encrypted value when sealed fields were not asked for.
const SealedMarker = "[sealed]"

// Queryer is a *sql.Tx or *sql.Conn: every pass over a table must see the same rows.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Table says how one table is exported. Every column of the table must be named in Columns,
// Sealed or Omitted; one that is not is left out and reported, so that a column added later is
// never exported by accident.
type Table struct {
	Name        string
	Description string
	Columns     string // space-separated names, exported as they are
	// Sealed columns are encrypted at rest. Aux names further columns the opener needs.
	Sealed map[string]Sealed
	// Omitted maps a column to why it is never exported.
	Omitted map[string]string
	// Where restricts the rows (a constant SQL expression; the table is aliased "t").
	Where string
	// Skip, when set, is why the whole table is never exported.
	Skip string
}

// Sealed describes the additional data an encrypted column is bound to: the SQL expression Row
// (over the alias "t") yields the row's id, and the AAD is Prefix + id + Suffix.
type Sealed struct{ Row, Prefix, Suffix string }

// Opener decrypts a sealed value.
type Opener func(sealed, aad string) (string, error)

// Column is a column in schema.json.
type Column struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
	// Sealed: encrypted at rest; "[sealed]" unless the export included sealed fields.
	Sealed bool `json:"sealed,omitempty"`
}

// Omission is something left out, and why.
type Omission struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// TableSchema describes one exported table in schema.json.
type TableSchema struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Files       []string   `json:"files"`
	Rows        int        `json:"rows"`
	Columns     []Column   `json:"columns"`
	Omitted     []Omission `json:"omitted_columns,omitempty"`
}

// Schema is schema.json.
type Schema struct {
	Format        int           `json:"format"`
	IncludeSealed bool          `json:"sealed_fields_decrypted"`
	Tables        []TableSchema `json:"tables"`
	OmittedTables []Omission    `json:"omitted_tables,omitempty"`
}

const unclassified = "not classified by this version of `can`: left out"

type colInfo struct {
	name, typ string
	pk        int
}

func columnsOf(ctx context.Context, q Queryer, table string) ([]colInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []colInfo
	for rows.Next() {
		var c colInfo
		if err := rows.Scan(&c.name, &c.typ, &c.pk); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TableNames lists the tables of a SQLite database.
func TableNames(ctx context.Context, q Queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Classify reports every table and column of the database that the specs do not name, as
// "table" or "table.column". Tests fail on any; an export leaves them out and says so.
func Classify(ctx context.Context, q Queryer, tables []Table) ([]string, error) {
	names, err := TableNames(ctx, q)
	if err != nil {
		return nil, err
	}
	byName := map[string]Table{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	var out []string
	for _, n := range names {
		t, ok := byName[n]
		switch {
		case !ok:
			out = append(out, n)
			continue
		case t.Skip != "":
			continue
		}
		cols, err := columnsOf(ctx, q, n)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			if !t.has(c.name) {
				out = append(out, n+"."+c.name)
			}
		}
	}
	return out, nil
}

func (t Table) plain() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(t.Columns) {
		m[c] = true
	}
	return m
}

func (t Table) has(col string) bool {
	_, sealed := t.Sealed[col]
	_, omitted := t.Omitted[col]
	return sealed || omitted || t.plain()[col]
}

// Options of Database.
type Options struct {
	// Dir is the directory of the entries: Dir/schema.json, Dir/records/<table>.jsonl and .csv.
	Dir string
	// Open decrypts sealed columns; nil writes SealedMarker instead.
	Open Opener
}

// Database writes every table of the database that tables names and returns the schema it also
// writes, with the warnings. All reads go through q, which must give one consistent view.
func Database(ctx context.Context, a *Archive, q Queryer, tables []Table, o Options) (Schema, []string, error) {
	s := Schema{Format: FormatVersion, IncludeSealed: o.Open != nil, Tables: []TableSchema{}}
	present, err := TableNames(ctx, q)
	if err != nil {
		return s, nil, err
	}
	byName := map[string]Table{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	var warnings []string
	for _, name := range present {
		t, ok := byName[name]
		if !ok {
			s.OmittedTables = append(s.OmittedTables, Omission{name, unclassified})
			warnings = append(warnings, fmt.Sprintf("table %s is %s", name, unclassified))
			continue
		}
		if t.Skip != "" {
			s.OmittedTables = append(s.OmittedTables, Omission{name, t.Skip})
			continue
		}
		ts, w, err := writeTable(ctx, a, q, t, o)
		if err != nil {
			return s, nil, fmt.Errorf("table %s: %w", name, err)
		}
		warnings = append(warnings, w...)
		s.Tables = append(s.Tables, ts)
	}
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	return s, warnings, a.JSON(join(o.Dir, "schema.json"), s)
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func writeTable(ctx context.Context, a *Archive, q Queryer, t Table, o Options) (TableSchema, []string, error) {
	ts := TableSchema{Name: t.Name, Description: t.Description, Columns: []Column{}}
	cols, err := columnsOf(ctx, q, t.Name)
	if err != nil {
		return ts, nil, err
	}
	var warnings []string
	var sel []string
	var order []colInfo
	var seals []*Sealed
	plain := t.plain()
	for _, c := range cols {
		spec, sealed := t.Sealed[c.name]
		switch why, omitted := t.Omitted[c.name]; {
		case omitted:
			ts.Omitted = append(ts.Omitted, Omission{c.name, why})
			continue
		case !sealed && !plain[c.name]:
			ts.Omitted = append(ts.Omitted, Omission{c.name, unclassified})
			warnings = append(warnings, fmt.Sprintf("column %s.%s is %s", t.Name, c.name, unclassified))
			continue
		}
		col := Column{Name: c.name, Type: c.typ, PrimaryKey: c.pk > 0, Sealed: sealed}
		ts.Columns = append(ts.Columns, col)
		sel = append(sel, "t."+ident(c.name))
		if sealed {
			s := spec
			seals = append(seals, &s)
		} else {
			seals = append(seals, nil)
		}
		if c.pk > 0 {
			order = append(order, c)
		}
	}
	if len(sel) == 0 {
		return ts, warnings, fmt.Errorf("no column to export")
	}
	n := len(sel)
	// The row ids of the sealed columns are selected after the exported ones.
	aux := map[string]int{}
	for _, s := range seals {
		if s == nil {
			continue
		}
		if _, ok := aux[s.Row]; !ok {
			aux[s.Row] = len(sel)
			sel = append(sel, s.Row)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].pk < order[j].pk })
	var by []string
	for _, c := range order {
		by = append(by, "t."+ident(c.name))
	}
	if len(by) == 0 {
		for i := range n {
			by = append(by, strconv.Itoa(i+1))
		}
	}
	query := "SELECT " + strings.Join(sel, ", ") + " FROM " + ident(t.Name) + " AS t"
	if t.Where != "" {
		query += " WHERE " + t.Where
	}
	query += " ORDER BY " + strings.Join(by, ", ")

	// each calls row for every row, with sealed values opened or replaced.
	each := func(row func([]any) error) error {
		rows, err := q.QueryContext(ctx, query) //nolint:gosec // identifiers from the fixed specs and the schema
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		vals := make([]any, len(sel))
		ptrs := make([]any, len(sel))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			out := append([]any(nil), vals[:n]...)
			for i, s := range seals {
				if s == nil || out[i] == nil {
					continue
				}
				v := text(out[i])
				switch {
				case v == "":
					out[i] = ""
				case o.Open == nil:
					out[i] = SealedMarker
				default:
					plain, err := o.Open(v, s.Prefix+text(vals[aux[s.Row]])+s.Suffix)
					if err != nil {
						return fmt.Errorf("column %s: a sealed value cannot be decrypted with this module's key: %w", ts.Columns[i].Name, err)
					}
					out[i] = plain
				}
			}
			if err := row(out); err != nil {
				return err
			}
		}
		return rows.Err()
	}

	jsonl, csvName := join(o.Dir, "records/"+t.Name+".jsonl"), join(o.Dir, "records/"+t.Name+".csv")
	err = a.Render(jsonl, func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 32<<10)
		rows := 0
		err := each(func(v []any) error {
			rows++
			return writeObject(bw, ts.Columns, v)
		})
		if err != nil {
			return err
		}
		ts.Rows = rows
		return bw.Flush()
	})
	if err != nil {
		return ts, warnings, err
	}
	err = a.Render(csvName, func(w io.Writer) error {
		cw := csv.NewWriter(w)
		rec := make([]string, n)
		for i, c := range ts.Columns {
			rec[i] = CSVSafe(c.Name)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
		err := each(func(v []any) error {
			for i := range rec {
				rec[i] = cell(v[i])
			}
			return cw.Write(rec)
		})
		if err != nil {
			return err
		}
		cw.Flush()
		return cw.Error()
	})
	ts.Files = []string{"records/" + t.Name + ".jsonl", "records/" + t.Name + ".csv"}
	return ts, warnings, err
}

func writeObject(w *bufio.Writer, cols []Column, v []any) error {
	_ = w.WriteByte('{')
	for i, c := range cols {
		if i > 0 {
			_ = w.WriteByte(',')
		}
		k, err := json.Marshal(c.Name)
		if err != nil {
			return err
		}
		val, err := json.Marshal(v[i]) // []byte becomes base64
		if err != nil {
			return err
		}
		_, _ = w.Write(k)
		_ = w.WriteByte(':')
		_, _ = w.Write(val)
	}
	_, err := w.WriteString("}\n")
	return err
}

func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// cell is the CSV form of a value. Only text can be a formula; numbers are written as they are.
func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []byte:
		return CSVSafe(base64.StdEncoding.EncodeToString(x))
	default:
		return CSVSafe(text(x))
	}
}
