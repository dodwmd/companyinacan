package jobs

import "testing"

// Only a can-core account goes into application.received.v1: anything else could not be notified,
// and a free-form login has no place in an event.
func TestHiringAccount(t *testing.T) {
	for in, want := range map[string]string{"user:hana": "user:hana", "hana": "", "user:": "", "party:p1": "", "user:a b": ""} {
		if got := hiringAccount(in); got != want {
			t.Errorf("hiringAccount(%q) = %q, want %q", in, got, want)
		}
	}
}
