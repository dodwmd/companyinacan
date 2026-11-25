package work

import "context"

// Fractional ranks order issues on a board or backlog without renumbering: a rank is a
// string over rankAlphabet read as a base-36 fraction, compared lexicographically, so
// moving one issue between two others writes one row. Ranks never end in '0' (that would
// make two spellings of the same fraction), which is why a rank strictly between any two
// ranks always exists.
const rankAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

const rankBase = len(rankAlphabet)

func rankDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	}
	return 0
}

// rankBetween returns a rank strictly between lo and hi. An empty lo means "before
// everything" and an empty hi means "after everything". It requires lo < hi when both are
// set; callers that cannot guarantee that (stale neighbours) get a rank after lo.
func rankBetween(lo, hi string) string {
	if hi != "" && lo >= hi {
		hi = ""
	}
	if hi == "" && lo != "" {
		// Appending: bump the first character that can be bumped. That grows the string by
		// one character per ~35 appends instead of one per ~5 (midpoint to infinity).
		for i := 0; i < len(lo); i++ {
			if d := rankDigit(lo[i]); d < rankBase-1 {
				return lo[:i] + string(rankAlphabet[d+1])
			}
		}
		return lo + "i"
	}
	var res []byte
	bounded := hi != ""
	for i := 0; ; i++ {
		da := 0
		if i < len(lo) {
			da = rankDigit(lo[i])
		}
		db := rankBase
		if bounded && i < len(hi) {
			db = rankDigit(hi[i])
		}
		if db-da > 1 {
			return string(append(res, rankAlphabet[(da+db)/2]))
		}
		res = append(res, rankAlphabet[da])
		if db != da {
			bounded = false
		}
	}
}

// maxRankLen is when a project's ranks are respaced. Repeated inserts at one spot lengthen a
// rank; respacing rewrites every rank of the project once, in order, so lengths stay bounded.
const maxRankLen = 24

// rebalanceRanks rewrites every rank of a project as evenly spaced short strings, keeping
// the order (ties broken by id).
func (s *Store) rebalanceRanks(ctx context.Context, q dbq, projectID int64) error {
	rs, err := q.QueryContext(ctx, `SELECT id FROM issues WHERE project_id = ? ORDER BY rank, id`, projectID)
	if err != nil {
		return err
	}
	var ids []int64
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			_ = rs.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rs.Close(); err != nil {
		return err
	}
	const slots = 36 * 36 * 36 * 36
	gap := slots / (len(ids) + 1)
	if gap < 2 {
		return nil // more than ~800k issues in one project: leave the ranks alone
	}
	for i, id := range ids {
		v := (i + 1) * gap
		b := []byte{rankAlphabet[v/46656%36], rankAlphabet[v/1296%36], rankAlphabet[v/36%36], rankAlphabet[v%36], 'i'}
		if _, err := q.ExecContext(ctx, `UPDATE issues SET rank = ? WHERE id = ?`, string(b), id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) maybeRebalance(ctx context.Context, q dbq, projectID int64, rank string) error {
	if len(rank) <= maxRankLen {
		return nil
	}
	return s.rebalanceRanks(ctx, q, projectID)
}
