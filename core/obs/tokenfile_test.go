package obs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type recLog struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recLog) add(level, msg string, args []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, level+" "+msg)
	for _, a := range args {
		if s, ok := a.(string); ok {
			r.msgs = append(r.msgs, s)
		}
	}
}
func (r *recLog) Info(m string, a ...any)  { r.add("info", m, a) }
func (r *recLog) Warn(m string, a ...any)  { r.add("warn", m, a) }
func (r *recLog) Error(m string, a ...any) { r.add("error", m, a) }
func (r *recLog) text() string             { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.msgs, "\n") }

func TestWarnsWhenTheTokenFileIsReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o600, false}, {0o400, false}, {0o640, true}, {0o644, true}, {0o604, true}} {
		f := filepath.Join(dir, "tok-"+tc.mode.String())
		if err := os.WriteFile(f, []byte("s3cret-token-value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f, tc.mode); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		rl := &recLog{}
		env := map[string]string{"CAN_METRICS_ADDR": "127.0.0.1:0", "CAN_METRICS_TOKEN_FILE": f}
		if _, err := StartMetrics(ctx, func(k string) string { return env[k] }, NewRegistry(), rl); err != nil {
			t.Fatalf("%v: %v", tc.mode, err)
		}
		cancel()
		got := strings.Contains(rl.text(), "warn ")
		if got != tc.warn {
			t.Errorf("mode %v: warned=%v want %v\n%s", tc.mode, got, tc.warn, rl.text())
		}
		if strings.Contains(rl.text(), "s3cret-token-value") {
			t.Errorf("the token was logged: %s", rl.text())
		}
	}
}
