package people

import "testing"

func TestUIConfig(t *testing.T) {
	ok := "keyring_file: /k\nui:\n  enabled: true\n  insecure_cookies: true\n  notifications_url: https://admin.example.com/notifications\n  idle_minutes: 15\n"
	c, err := ParseConfig([]byte(ok))
	if err != nil || !c.UI.Enabled || c.UI.IdleMinutes != 15 {
		t.Fatalf("config = %+v, %v", c, err)
	}
	if c, err := ParseConfig([]byte("keyring_file: /k\n")); err != nil || c.UI.Enabled {
		t.Fatalf("the UI must be off by default: %+v %v", c, err)
	}
	for _, bad := range []string{"ui:\n  notifications_url: ftp://x.example\n", "ui:\n  idle_minutes: -1\n", "ui:\n  nope: 1\n"} {
		if _, err := ParseConfig([]byte("keyring_file: /k\n" + bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
