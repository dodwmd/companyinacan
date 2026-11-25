package work

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
