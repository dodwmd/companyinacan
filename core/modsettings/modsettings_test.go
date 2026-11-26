package modsettings

import "testing"

func TestWidens(t *testing.T) {
	i := func(n int) any { return int64(n) }
	l := func(s ...any) any { return s }
	for _, c := range []struct {
		how      string
		was, now any
		want     bool
	}{
		{widensTrue, false, true, true}, {widensTrue, true, false, false},
		{widensFalse, true, false, true}, {widensFalse, false, true, false},
		{"becomes:fake", "none", "fake", true}, {"becomes:fake", "fake", "none", false},
		{widensChanged, "USD", "AUD", true}, {widensChanged, "USD", "USD", false},
		{widensAdded, l(), l("a"), true}, {widensAdded, l("a", "b"), l("b"), false},
		{widensRemoved, l("a", "b"), l("b"), true}, {widensRemoved, l(), l("a"), false},
		{widensLower, i(10), i(5), true}, {widensLower, i(5), i(10), false},
		{widensHigher, i(5), i(10), true}, {widensHigher, i(10), i(5), false},
		{widensKeepLess, i(0), i(10), true}, {widensKeepLess, i(10), i(0), false}, {widensKeepLess, i(10), i(5), true}, {widensKeepLess, i(5), i(10), false},
		{widensLimit, i(30), i(0), true}, {widensLimit, i(30), i(60), true}, {widensLimit, i(0), i(60), false}, {widensLimit, i(60), i(30), false},
		{widensRetention, "7y", "0", true}, {widensRetention, "legal", "10y", true}, {widensRetention, "7y", "legal", false},
		{widensRetention, "1y", "12m", true}, {widensRetention, "1y", "13m", false},
	} {
		if got := widens(c.how, c.was, c.now); got != c.want {
			t.Errorf("%s %v -> %v: %v", c.how, c.was, c.now, got)
		}
	}
}

func TestNilSchemaWarnsAboutEverything(t *testing.T) {
	var s *Schema
	r := s.Map(map[string]any{"a": 1}, Facts{})
	if len(r.Values) != 0 || len(r.Issues) != 1 || s.Widening(nil, nil) != nil {
		t.Fatalf("%+v", r)
	}
}
