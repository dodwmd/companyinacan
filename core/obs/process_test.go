package obs

import (
	"strings"
	"testing"
)

func TestRSSFromStatm(t *testing.T) {
	got, err := rssFromStatm("12345 2560 100 5 0 900 0\n", 4096)
	if err != nil || got != 2560*4096 {
		t.Fatalf("rss = %d, %v", got, err)
	}
	for _, bad := range []string{"", "1", "1 x 3", "1 -5 3"} {
		if _, err := rssFromStatm(bad, 4096); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestDeclaredRAM(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		env  map[string]string
		want int64
	}{
		{nil, 0},
		{map[string]string{"CAN_RAM_DECLARED_BYTES": "50331648"}, 50331648},
		{map[string]string{"CAN_RAM_DECLARED_MB": "48"}, 48 << 20},
		{map[string]string{"CAN_RAM_DECLARED_BYTES": "5", "CAN_RAM_DECLARED_MB": "48"}, 5},
		{map[string]string{"CAN_RAM_DECLARED_MB": "-3"}, 0},
		{map[string]string{"CAN_RAM_DECLARED_MB": "lots"}, 0},
	}
	for _, c := range cases {
		if got := DeclaredRAMBytes(env(c.env)); got != c.want {
			t.Errorf("%v: got %d want %d", c.env, got, c.want)
		}
	}
}

func TestRegisterProcessExposesFootprintGauges(t *testing.T) {
	r := NewRegistry()
	RegisterProcess(r, BuildInfo{Version: "v1.2.3", Commit: "abc1234"}, func(k string) string {
		if k == "CAN_RAM_DECLARED_MB" {
			return "48"
		}
		return ""
	})
	out := expose(t, r)
	for _, want := range []string{
		`can_build_info{version="v1.2.3",commit="abc1234",go="go`,
		"can_ram_declared_bytes 50331648",
		"can_ram_rss_bytes ",
		"can_goroutines ",
		"can_gc_pause_seconds_total ",
		"can_uptime_seconds ",
		"can_process_start_time_seconds ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestDeclaredGaugeAbsentWhenUnset(t *testing.T) {
	r := NewRegistry()
	RegisterProcess(r, BuildInfo{}, func(string) string { return "" })
	if strings.Contains(expose(t, r), "can_ram_declared_bytes") {
		t.Fatal("declared gauge must be absent when nothing is declared, so alerts do not divide by zero")
	}
}
