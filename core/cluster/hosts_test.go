package cluster

import "testing"

func TestHostname(t *testing.T) {
	cases := map[string]string{"": "localtest.me", "admin": "admin.localtest.me", "Help": "help.localtest.me", "a.b": "a.b.localtest.me"}
	for in, want := range cases {
		got, err := Hostname(in)
		if err != nil || got != want {
			t.Errorf("Hostname(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"a b", "-x", "a.", "a..b", "x_y", "a/b"} {
		if _, err := Hostname(bad); err == nil {
			t.Errorf("Hostname(%q) should fail", bad)
		}
	}
}

func TestURL(t *testing.T) {
	if u, _ := URL("admin", false); u != "http://admin.localtest.me" {
		t.Error(u)
	}
	if u, _ := URL("admin", true); u != "https://admin.localtest.me" {
		t.Error(u)
	}
	if _, err := URL("bad host", false); err == nil {
		t.Error("expected error")
	}
}
