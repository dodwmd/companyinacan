package modclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// QA: a form posted without a session is bounded in bytes and in fields, query string included,
// before anything is turned into values.
func TestLimitPublicForm(t *testing.T) {
	for _, c := range []struct {
		name, query, body string
		ok                bool
	}{
		{"a sign-in", "", "token=abc&csrf=def", true},
		{"at the size bound", "", "token=" + strings.Repeat("x", MaxPublicFormBytes-6), true},
		{"too large", "", "token=" + strings.Repeat("x", MaxPublicFormBytes), false},
		{"65 KB", "", "token=" + strings.Repeat("x", 65_000), false},
		{"sixteen fields", "", strings.Repeat("a&", 15) + "a", true},
		{"many fields", "", strings.Repeat("a&", 16) + "a", false},
		{"many query fields", strings.Repeat("a&", 40), "token=abc", false},
		{"long query", "q=" + strings.Repeat("x", 2000), "token=abc", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/login?"+c.query, strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		LimitPublicForm(httptest.NewRecorder(), r)
		err := r.ParseForm()
		if c.ok && (err != nil || len(r.PostForm) == 0) {
			t.Errorf("%s refused: %v", c.name, err)
		}
		if !c.ok && (err == nil || len(r.Form) != 0) {
			t.Errorf("%s accepted: %v, %d values", c.name, err, len(r.Form))
		}
	}
}
