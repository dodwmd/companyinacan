package jobs

import (
	"context"
	"slices"
	"sort"
)

// EEOInput is the optional, voluntary diversity questionnaire on the application form. It is
// collected only when apply.collect_eeo is on, which is a legal decision for the operator:
// some jurisdictions require or encourage it, others restrict it.
type EEOInput struct {
	Gender, Ethnicity, Veteran, Disability string
}

// eeoOptions are the accepted answers. Anything else is stored as blank, never as free text.
var eeoOptions = map[string][]string{
	"gender":     {"female", "male", "non_binary", "self_described", "prefer_not_to_say"},
	"ethnicity":  {"asian", "black", "hispanic_latino", "indigenous", "middle_eastern_north_african", "pacific_islander", "white", "two_or_more", "other", "prefer_not_to_say"},
	"veteran":    {"yes", "no", "prefer_not_to_say"},
	"disability": {"yes", "no", "prefer_not_to_say"},
}

// EEOFields lists the questionnaire fields in display order.
var EEOFields = []string{"gender", "ethnicity", "veteran", "disability"}

func eeoClean(field, v string) string {
	if slices.Contains(eeoOptions[field], v) {
		return v
	}
	return ""
}

// insertEEO adds one response to month counters. There is deliberately no per-response row, no
// id and no timestamp finer than a month: a row-per-response design (the first version) let
// anyone with the database file or a backup line responses up with applications by order.
// Counters carry no order. Residual risk, stated plainly: someone who can read the file before
// and after a single submission (for example two backups taken minutes apart) can still see
// which counters moved; this is not formal anonymisation, only a removal of the easy link.
func (s *Store) insertEEO(ctx context.Context, tx dbq, in EEOInput) error {
	vals := map[string]string{"gender": eeoClean("gender", in.Gender), "ethnicity": eeoClean("ethnicity", in.Ethnicity),
		"veteran": eeoClean("veteran", in.Veteran), "disability": eeoClean("disability", in.Disability)}
	any := false
	month := s.now().UTC().Format("2006-01")
	for _, f := range EEOFields {
		if vals[f] == "" {
			continue
		}
		any = true
		if err := bumpEEO(ctx, tx, month, f, vals[f]); err != nil {
			return err
		}
	}
	if !any {
		return nil
	}
	return bumpEEO(ctx, tx, month, eeoResponses, "")
}

// eeoResponses is the counter that counts submissions that answered at least one question.
const eeoResponses = "_responses"

func bumpEEO(ctx context.Context, tx dbq, month, field, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO eeo_counts (month, field, value, n) VALUES (?,?,?,1) ON CONFLICT (month, field, value) DO UPDATE SET n = n + 1`, month, field, value)
	return err
}

// EEOCell is one answer count. Count is nil (JSON null) when it is suppressed.
type EEOCell struct {
	Value string `json:"value"`
	Count *int   `json:"count"`
}

// EEOReport is the aggregate view. It is the only way diversity data is ever read, and it
// covers the whole organisation: per-job reports are not offered, because a global report and a
// per-job report could be subtracted from each other to expose a small job's cells.
type EEOReport struct {
	Responses *int                 `json:"responses"` // null when below the minimum cell size
	Fields    map[string][]EEOCell `json:"fields"`
	MinCell   int                  `json:"min_cell"`
}

// EEOAggregate returns counts per answer. If any answer of a question has fewer than min_cell
// responses the WHOLE question is suppressed (complementary suppression: a single hidden cell
// could otherwise be recovered from the total). Admin only and audited.
func (s *Store) EEOAggregate(ctx context.Context, p *Principal) (*EEOReport, error) {
	if !p.Can(ActAdmin) {
		return nil, ErrForbidden
	}
	min := s.cfg.EEO.MinCell
	rep := &EEOReport{MinCell: min, Fields: map[string][]EEOCell{}}
	var total int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(SUM(n), 0) FROM eeo_counts WHERE field = ?`, eeoResponses).Scan(&total); err != nil {
		return nil, err
	}
	if total >= min {
		rep.Responses = &total
	}
	for _, f := range EEOFields {
		rows, err := s.rdb.QueryContext(ctx, `SELECT value, SUM(n) FROM eeo_counts WHERE field = ? GROUP BY value`, f)
		if err != nil {
			return nil, err
		}
		cells := []EEOCell{}
		suppress := total < min
		counts := map[string]int{}
		for rows.Next() {
			var v string
			var n int
			if err := rows.Scan(&v, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			counts[v] = n
			if n < min {
				suppress = true
			}
		}
		_ = rows.Close()
		for v, n := range counts {
			c := EEOCell{Value: v}
			if !suppress {
				n := n
				c.Count = &n
			}
			cells = append(cells, c)
		}
		sort.Slice(cells, func(i, j int) bool { return cells[i].Value < cells[j].Value })
		rep.Fields[f] = cells
	}
	if err := s.auditRead(ctx, p, "eeo:report", []string{"eeo_aggregate"}); err != nil {
		return nil, err
	}
	return rep, nil
}
