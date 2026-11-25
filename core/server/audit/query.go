package audit

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Filter selects audit records for the admin UI. Zero fields match everything.
type Filter struct {
	Actor        string // exact
	ActionPrefix string // literal prefix, e.g. "admin." (no wildcards)
	Target       string // exact
	Before       int64  // only records with seq < Before (paging; 0 = from the newest)
	Limit        int    // 1..500, default 50
}

// Query returns matching records, newest first.
func (l *Log) Query(ctx context.Context, f Filter) ([]Record, error) {
	if f.Limit < 1 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	var where []string
	var args []any
	if f.Actor != "" {
		where, args = append(where, "actor=?"), append(args, f.Actor)
	}
	if f.ActionPrefix != "" {
		where, args = append(where, "substr(action,1,?)=?"), append(args, utf8.RuneCountInString(f.ActionPrefix), f.ActionPrefix)
	}
	if f.Target != "" {
		where, args = append(where, "target=?"), append(args, f.Target)
	}
	if f.Before > 0 {
		where, args = append(where, "seq<?"), append(args, f.Before)
	}
	q := `SELECT seq,time,actor,action,target,detail,prev_hash,hash FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY seq DESC LIMIT ?"
	rows, err := l.db.QueryContext(ctx, q, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
