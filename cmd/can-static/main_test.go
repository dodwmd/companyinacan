package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func site(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestServeAndHealthcheck(t *testing.T) {
	addr, dir := freeAddr(t), site(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() { done <- run(ctx, []string{"-dir", dir, "-addr", addr}, &out, &errb) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if code := run(context.Background(), []string{"-healthcheck", "-addr", addr}, &out, &errb); code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy: %s", errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d: %s", code, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not shut down")
	}
}

func TestHealthcheckFailsWhenDown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-healthcheck", "-addr", freeAddr(t)}, &out, &errb); code == 0 {
		t.Fatal("healthcheck against a closed port must fail")
	}
}

func TestEnvDefaultsAndFlagErrors(t *testing.T) {
	t.Setenv("CAN_STATIC_ADDR", "127.0.0.1:1")
	t.Setenv("CAN_STATIC_DIR", "/nonexistent")
	cfg, err := parseFlags(nil)
	if err != nil || cfg.addr != "127.0.0.1:1" || cfg.dir != "/nonexistent" {
		t.Fatalf("%+v %v", cfg, err)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-bogus"}, &out, &errb); code != 2 {
		t.Errorf("bad flag: exit %d", code)
	}
	if !strings.Contains(errb.String(), "bogus") {
		t.Errorf("stderr: %q", errb.String())
	}
}

func TestListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-dir", site(t), "-addr", ln.Addr().String()}, &out, &errb); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestExtractFlagUnpacksArchiveAndExits(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(t.TempDir(), "site.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "index.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(arc, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-extract", arc, "-dir", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "index.html")); err != nil || string(b) != "hi" {
		t.Fatalf("index.html: %q %v", b, err)
	}
	if code := run(context.Background(), []string{"-extract", filepath.Join(dir, "missing"), "-dir", dir}, &out, &errb); code == 0 {
		t.Fatal("a missing archive must fail")
	}
}
