package provider

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRecords(t *testing.T) {
	recs, err := Records("acme.example", "203.0.113.7", []string{"admin", "help"})
	if err != nil || len(recs) != 3 || recs[0].Name != "acme.example" || recs[1].Name != "admin.acme.example" || recs[2].Type != "A" {
		t.Fatalf("%+v %v", recs, err)
	}
	v6, _ := Records("acme.example", "2001:db8::1", nil)
	if v6[0].Type != "AAAA" {
		t.Fatalf("%+v", v6)
	}
	if _, err := Records("a.example", "not-an-ip", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestManualDNSPrintsRecords(t *testing.T) {
	recs, _ := Records("acme.example", "203.0.113.7", []string{"admin"})
	var out strings.Builder
	d, err := NewDNS("")
	if err != nil || d.Name() != "manual" {
		t.Fatal(err)
	}
	if err := d.Apply(context.Background(), "acme.example", recs, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TYPE", "A", "acme.example", "admin.acme.example", "203.0.113.7", "300", "Propagation"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestNewDNS(t *testing.T) {
	if _, err := NewDNS("provider-native"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("%v", err)
	}
	if _, err := NewDNS("cloudflare"); err == nil || errors.Is(err, ErrNotImplemented) {
		t.Fatalf("%v", err)
	}
	if _, err := NewDNS("route66"); err == nil || errors.Is(err, ErrNotImplemented) {
		t.Fatalf("%v", err)
	}
}

func TestTunnelArgs(t *testing.T) {
	args, err := TunnelArgs(Target{Host: "203.0.113.7", User: "root", KeyFile: "/k"}, 16443)
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(args, " ")
	for _, want := range []string{"-N", "ExitOnForwardFailure=yes", "-L 127.0.0.1:16443:127.0.0.1:6443", "root@203.0.113.7"} {
		if !strings.Contains(j, want) {
			t.Errorf("%q missing %q", j, want)
		}
	}
	if strings.Contains(j, "0.0.0.0") {
		t.Error("tunnel must bind loopback only")
	}
	for _, p := range []int{0, 80, 70000} {
		if _, err := TunnelArgs(Target{Host: "h"}, p); err == nil {
			t.Errorf("port %d accepted", p)
		}
	}
}

func TestFreeLocalPort(t *testing.T) {
	p, err := FreeLocalPort()
	if err != nil || p < 1024 {
		t.Fatalf("%d %v", p, err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatalf("port not actually free: %v", err)
	}
	_ = l.Close()
}

const k3sKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Q0E=
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

func TestRewriteKubeconfig(t *testing.T) {
	out, err := RewriteKubeconfig(k3sKubeconfig, 16443, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"server: https://127.0.0.1:16443", "name: acme", "cluster: acme", "user: acme", "current-context: acme"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "default") || strings.Contains(out, ":6443") {
		t.Errorf("leftovers:\n%s", out)
	}
	if _, err := RewriteKubeconfig("apiVersion: v1\n", 1234, "a"); err == nil {
		t.Error("no server entry: want error")
	}
	if _, err := RewriteKubeconfig(k3sKubeconfig, 16443, "bad name"); err == nil {
		t.Error("bad name accepted")
	}
}

func TestFetchKubeconfig(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(fakeRule{contains: "/etc/rancher/k3s/k3s.yaml", out: k3sKubeconfig})
	p, err := FetchKubeconfig(context.Background(), r, dir, "acme", 16443)
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, ".can", "kubeconfig") {
		t.Fatal(p)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode %v", fi.Mode())
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "127.0.0.1:16443") {
		t.Error("not rewritten")
	}

	bad := newFakeRunner(fakeRule{contains: "k3s.yaml", out: "cat: no such file"})
	if _, err := FetchKubeconfig(context.Background(), bad, t.TempDir(), "acme", 16443); err == nil {
		t.Error("garbage output accepted")
	}
}
