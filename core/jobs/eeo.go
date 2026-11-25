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

// insertEEO stores one response with no link to the candidate or application: no id, and a
// month rather than a timestamp, so it cannot be joined back to a person.
func (s *Store) insertEEO(ctx context.Context, tx dbq, jobID string, in EEOInput) error {
	g, e, v, d := eeoClean("gender", in.Gender), eeoClean("ethnicity", in.Ethnicity), eeoClean("veteran", in.Veteran), eeoClean("disability", in.Disability)
	if g == "" && e == "" && v == "" && d == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO eeo_responses (job_id, month, gender, ethnicity, veteran, disability) VALUES (?,?,?,?,?,?)`, jobID, s.now().UTC().Format("2006-01"), g, e, v, d)
	return err
}

// EEOCell is one answer count. Count is nil (JSON null) when the cell is suppressed because
// fewer than eeo.min_cell people gave that answer.
type EEOCell struct {
	Value string `json:"value"`
	Count *int   `json:"count"`
}

// EEOReport is the aggregate view. It is the only way diversity data is ever read.
type EEOReport struct {
	JobID     string               `json:"job_id,omitempty"`
	Responses *int                 `json:"responses"` // null when below the minimum cell size
	Fields    map[string][]EEOCell `json:"fields"`
	MinCell   int                  `json:"min_cell"`
}

// EEOAggregate returns counts per answer with small cells suppressed. Admin only, audited, and
// no code path returns an individual response. Small-cell suppression reduces re-identification
// risk; it is not formal anonymisation, and suppressed totals can still be inferred in some
// cases, which is why the access is limited and audited.
func (s *Store) EEOAggregate(ctx context.Context, p *Principal, jobID string) (*EEOReport, error) {
	if !p.Can(ActAdmin) {
		return nil, ErrForbidden
	}
	min := s.cfg.EEO.MinCell
	rep := &EEOReport{JobID: jobID, MinCell: min, Fields: map[string][]EEOCell{}}
	var total int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM eeo_responses WHERE (? = '' OR job_id = ?)`, jobID, jobID).Scan(&total); err != nil {
		return nil, err
	}
	if total >= min {
		rep.Responses = &total
	}
	for _, f := range EEOFields {
		rows, err := s.rdb.QueryContext(ctx, `SELECT `+f+`, COUNT(*) FROM eeo_responses WHERE (? = '' OR job_id = ?) AND `+f+` != '' GROUP BY `+f, jobID, jobID) //nolint:gosec // f is from the fixed EEOFields list
		if err != nil {
			return nil, err
		}
		cells := []EEOCell{}
		for rows.Next() {
			var v string
			var n int
			if err := rows.Scan(&v, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			c := EEOCell{Value: v}
			if n >= min && total >= min {
				c.Count = &n
			}
			cells = append(cells, c)
		}
		_ = rows.Close()
		sort.Slice(cells, func(i, j int) bool { return cells[i].Value < cells[j].Value })
		rep.Fields[f] = cells
	}
	if err := s.auditRead(ctx, p, "eeo:report", []string{"eeo_aggregate"}); err != nil {
		return nil, err
	}
	return rep, nil
}
