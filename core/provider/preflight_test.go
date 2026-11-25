package provider

import (
	"strings"
	"testing"
)

func checkNamed(cs []Check, name string) Check {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: "MISSING " + name}
}

func TestPreflightGoodHost(t *testing.T) {
	cs := ParsePreflight(probeGood)
	if !PreflightOK(cs) || PreflightError(cs) != nil {
		t.Fatalf("good host failed: %+v", cs)
	}
}

func TestPreflightFailures(t *testing.T) {
	cases := []struct {
		name, from, to, check string
	}{
		{"not root", "uid=0", "uid=1000", "root"},
		{"unsupported os", "os_id=ubuntu", "os_id=alpine", "os"},
		{"low ram", "mem_mb=1950", "mem_mb=900", "ram"},
		{"low disk", "disk_free_mb=30000", "disk_free_mb=2000", "disk"},
		{"no systemd", "systemd=yes", "systemd=no", "systemd"},
		{"port 80 busy", "port_80=free", "port_80=busy", "port 80"},
		{"port 6443 busy", "port_6443=free", "port_6443=busy", "port 6443"},
		{"bad arch", "arch=x86_64", "arch=armv6l", "arch"},
		{"garbage ram", "mem_mb=1950", "mem_mb=lots", "ram"},
	}
	for _, c := range cases {
		cs := ParsePreflight(strings.Replace(probeGood, c.from, c.to, 1))
		if got := checkNamed(cs, c.check); got.OK {
			t.Errorf("%s: check %q passed", c.name, c.check)
		}
		err := PreflightError(cs)
		if err == nil || !strings.Contains(err.Error(), c.check) {
			t.Errorf("%s: error %v does not name %q", c.name, err, c.check)
		}
	}
}

func TestPreflightRerunToleratesBusyPorts(t *testing.T) {
	if cs := ParsePreflight(probeBootstrapped); !PreflightOK(cs) {
		t.Fatalf("re-run on a bootstrapped host must pass: %+v", cs)
	}
	if BootstrappedVersion(probeBootstrapped) != DefaultK3sVersion {
		t.Fatal("version marker not parsed")
	}
}

func TestPreflightEmptyOutputFails(t *testing.T) {
	if PreflightOK(ParsePreflight("")) {
		t.Fatal("empty probe output must fail")
	}
}
