package provider

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestSSHRunnerStream(t *testing.T) {
	rec := t.TempDir() + "/rec"
	r := &SSHRunner{T: Target{Host: "203.0.113.7", User: "deploy"}, Command: fakeCommand(rec)}
	var out, errs strings.Builder
	if err := r.Stream(context.Background(), "k3s kubectl version", &out, &errs); err != nil || out.String() != "remote-output\n" {
		t.Fatalf("stream: %q %v", out.String(), err)
	}
	b, _ := os.ReadFile(rec)
	argv, _, _ := strings.Cut(string(b), "\x01")
	if !strings.HasSuffix(argv, "deploy@203.0.113.7\x00sudo -n k3s kubectl version") {
		t.Errorf("argv: %q", argv)
	}
}
