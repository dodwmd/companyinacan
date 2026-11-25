package installer

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// dagTemplate builds a list whose pointer-shared structure expands to 2^40 nodes when written out,
// then applies expr to it.
func dagTemplate(expr string) string {
	var b strings.Builder
	b.WriteString("{{- $l := list 1 2 3 4 -}}\n")
	for i := 0; i < 40; i++ {
		b.WriteString("{{- $l = list $l $l -}}\n")
	}
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: dag}\ndata: {x: \"{{ " + expr + " }}\"}\n")
	return b.String()
}

func vmHWMkB(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmHWM:") {
			var n int
			for _, f := range strings.Fields(l) {
				if v, err := atoi(f); err == nil {
					n = v
				}
			}
			return n
		}
	}
	return 0
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, os.ErrInvalid
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func TestListDAGThroughEverySerialiserIsAnErrorNotACrash(t *testing.T) {
	exprs := []string{
		"toYaml $l", "toJson $l", "join \",\" $l", "toString $l", "printf \"%v\" $l", "print $l", "quote $l", "squote $l",
	}
	before := vmHWMkB(t)
	for _, e := range exprs {
		in, _, _, spec := fixture(t, map[string]string{"stub/chart/templates/dag.yaml": dagTemplate(e)})
		start := time.Now()
		err := in.Enable(context.Background(), spec)
		if err == nil {
			t.Errorf("%s: hostile chart installed", e)
			continue
		}
		if d := time.Since(start); d > 60*time.Second {
			t.Errorf("%s: took %s", e, d)
		}
	}
	if after := vmHWMkB(t); after-before > 200*1024 {
		t.Errorf("the can-core stand-in process grew from %d kB to %d kB", before, after)
	}
}

// These forms are not covered by the cheap guards on purpose: they show that the child process, not
// the guards, is what keeps can-core alive. The runtime's out-of-memory failure is fatal and
// unrecoverable, so without the child this test would kill the test binary.
func TestUnguardedDAGUseCannotKillTheParent(t *testing.T) {
	before := vmHWMkB(t)
	for _, e := range []string{"$l", "html $l", "urlquery $l", "printf \"%s\" (toString 1) $l"} {
		in, _, _, spec := fixture(t, map[string]string{"stub/chart/templates/dag.yaml": dagTemplate(e)})
		start := time.Now()
		err := in.Enable(context.Background(), spec)
		if err == nil {
			t.Errorf("%s: hostile chart installed", e)
			continue
		}
		t.Logf("%s -> %v (%s)", e, strings.SplitN(err.Error(), "\n", 2)[0], time.Since(start).Round(time.Millisecond))
		if d := time.Since(start); d > 60*time.Second {
			t.Errorf("%s: took %s", e, d)
		}
	}
	if after := vmHWMkB(t); after-before > 200*1024 {
		t.Errorf("parent grew from %d kB to %d kB", before, after)
	}
	// and the installer still works afterwards
	in, _, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatalf("installer unusable after a child died: %v", err)
	}
}

func TestRenderChildFailsClearlyWhenItCannotBeStarted(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.RenderExe = "/nonexistent/can-core"
	err := in.Enable(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "refusing to render in-process") {
		t.Fatalf("%v", err)
	}
	if len(api.Objects) != 0 {
		t.Fatalf("the cluster was touched without a render: %v", api.Objects)
	}
}

func TestGoroutinesReturnToBaselineAfterChildFailures(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < 3; i++ {
		in, _, _, spec := fixture(t, map[string]string{"stub/chart/templates/dag.yaml": dagTemplate("$l")})
		_ = in.Enable(context.Background(), spec)
	}
	if n := goroutinesSettle(t, base); n > base+1 {
		t.Fatalf("%d goroutines, baseline %d", n, base)
	}
}
