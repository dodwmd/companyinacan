package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// Child limits. The address-space limit is the start-up size plus defaultChildAsMB (a fixed absolute
// limit does not fit a Go binary: it fails to start at 512 MiB); the heap watchdog is what keeps
// the real memory small. Both are per render and transient.
const (
	defaultChildAsMB   = 256
	defaultChildHeapMB = 32
	childWallClock     = 15 * time.Second
	maxChildStdout     = 16 << 20
	maxChildStderr     = 4 << 10
)

// capWriter keeps at most max bytes and reports overflow.
type capWriter struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		w.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return w.buf.Write(p)
}

// renderInChild renders the module's chart in `can-core render-chart` and returns the objects.
// Any failure of the child (crash, kill, limit, timeout, no output) is an error; nothing in the
// parent depends on the child behaving.
func (in *Installer) renderInChild(ctx context.Context, j renderJob) ([]kube.Object, error) {
	exe := in.cfg.RenderExe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("%w: %v (refusing to render in-process)", errNoChild, err)
		}
	}
	asMB, heapMB := in.cfg.ChildAddressSpaceMB, in.cfg.ChildHeapMB
	if asMB == 0 {
		asMB = defaultChildAsMB
	}
	if asMB < 0 {
		asMB = 0 // unlimited (tests under the race detector, which maps huge shadow memory)
	}
	if heapMB == 0 {
		heapMB = defaultChildHeapMB
	}
	j.TimeoutSec = int(renderTimeout / time.Second)
	in0, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "can-render-")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoChild, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	ctx, cancel := context.WithTimeout(ctx, childWallClock)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "render-chart", fmt.Sprintf("--as-mb=%d", asMB), fmt.Sprintf("--heap-mb=%d", heapMB)) //nolint:gosec // our own binary
	cmd.Dir = work
	cmd.Env = append([]string{"GOMAXPROCS=2", fmt.Sprintf("GOMEMLIMIT=%dMiB", heapMB)}, raceChildEnv...) // nothing inherited: no tokens, no proxy settings
	cmd.Stdin = bytes.NewReader(in0)
	out, errOut := &capWriter{max: maxChildStdout}, &capWriter{max: maxChildStderr}
	cmd.Stdout, cmd.Stderr = out, errOut
	applyProcAttr(cmd)
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %v (refusing to render in-process)", errNoChild, err)
	}
	waitErr := cmd.Wait()
	_ = killGroup(cmd.Process.Pid) // stragglers, if any
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("chart renderer killed after %s", childWallClock)
	case out.overflow:
		return nil, fmt.Errorf("chart renderer produced more than %d bytes", maxChildStdout)
	case waitErr != nil:
		return nil, fmt.Errorf("chart renderer failed (%v): a limit was hit or the template crashed it", waitErr)
	}
	var res renderResult
	if err := json.Unmarshal(out.buf.Bytes(), &res); err != nil {
		return nil, errors.New("chart renderer returned no result")
	}
	if res.Error != "" {
		return nil, errors.New(strings.TrimSpace(res.Error))
	}
	return res.Objects, nil // kube.Object is an alias of map[string]any
}

// jobFor builds what the child needs from a Spec.
func (in *Installer) jobFor(s reconcile.Spec, dir, chart string) renderJob {
	can := map[string]any{
		"apiUrl": in.cfg.PlatformURL, "namespace": s.Namespace, "serviceAccount": s.ServiceAccount,
		"moduleSecret": secretModule, "databaseSecret": secretDB, "tokenDir": TokenDir,
	}
	settings := s.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	return renderJob{
		ModuleDir: dir, Chart: chart, ValuesFile: s.Manifest.Runtime.Values, Settings: settings, Can: can,
		Input: map[string]any{
			"Settings": settings,
			"Can": map[string]any{"APIURL": can["apiUrl"], "Namespace": s.Namespace, "ServiceAccount": s.ServiceAccount,
				"SecretName": secretModule, "DBSecretName": secretDB, "TokenDir": TokenDir},
			"Module": map[string]any{"ID": s.ID, "Version": s.Version},
		},
		Host:    in.hostFor(s),
		Release: ReleaseInfo{Name: s.ID, Namespace: s.Namespace},
	}
}
