package diagnose

import "testing"

func TestScrubInternationalEmails(t *testing.T) {
	for _, a := range []string{"jürgen@bücher.example", "用户@例子.公司", "a@xn--mnchen-3ya.example", "b@firm.xn--p1ai", "o'brien@example.ie", "alice%40example.com", "José@example.com"} {
		if got := Scrub("mail to " + a + " bounced"); got != "mail to [email] bounced" {
			t.Errorf("%q -> %q", a, got)
		}
	}
}
