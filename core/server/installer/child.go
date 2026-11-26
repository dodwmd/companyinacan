package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"
)

// Charts are rendered in a CHILD PROCESS (`can-core render-chart`), not in can-core. In-process limits
// (budgets, caps) cannot stop everything a template can do: Go's "fatal error: out of memory" is not
// recoverable and would take the control plane down. The child has hard limits (address space,
// heap watchdog, wall clock, output cap, clean environment, own process group); whatever happens
// to it, the parent sees an error and keeps running. See research R18.

// renderJob is what the child needs: everything about the chart, nothing about the cluster.
type renderJob struct {
	ModuleDir  string         `json:"module_dir"`
	Chart      string         `json:"chart"`
	ValuesFile string         `json:"values_file,omitempty"`
	Settings   map[string]any `json:"settings"`
	Can        map[string]any `json:"can"`                 // .Values.can
	Input      map[string]any `json:"input"`               // inputs of values.tmpl.yaml
	Overrides  map[string]any `json:"overrides,omitempty"` // exposure settings, over the values file
	Host       string         `json:"host"`                // ingress.host, merged last
	Release    ReleaseInfo    `json:"release"`
	TimeoutSec int            `json:"timeout_s"` // the child's own deadline
}

type renderResult struct {
	Objects []map[string]any `json:"objects,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// buildValues maps settings and can-core inputs onto chart values (values.tmpl.yaml), as the
// installer did in-process before the render moved into the child.
func buildValues(ctx context.Context, j renderJob) (map[string]any, error) {
	settings := j.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	out := map[string]any{"settings": settings}
	if vf := j.ValuesFile; vf != "" {
		rootDir, err := filepath.EvalSymlinks(j.ModuleDir)
		if err != nil {
			return nil, err
		}
		b, err := readInside(rootDir, filepath.Join(rootDir, filepath.Clean("/"+vf))) // cannot escape the module directory
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", vf, err)
		}
		vst := &renderState{ctx: ctx, deadline: time.Now().Add(renderTimeout)}
		t := template.New("values").Option("missingkey=zero")
		t.Funcs(funcs(t, vst))
		t.Funcs(template.FuncMap{"tick": vst.tick})
		if _, err := t.Parse(string(b)); err != nil {
			return nil, fmt.Errorf("%s does not parse", vf)
		}
		if err := instrumentRanges(t, vst); err != nil {
			return nil, err
		}
		buf := &limitWriter{max: maxObjectBytes}
		if err := t.Execute(buf, j.Input); err != nil {
			return nil, fmt.Errorf("%s: %w", vf, err)
		}
		var mapped map[string]any
		if err := yaml.Unmarshal([]byte(strings.ReplaceAll(buf.buf.String(), "<no value>", "")), &mapped); err != nil {
			return nil, fmt.Errorf("%s is not valid YAML after rendering", vf)
		}
		out = deepMerge(out, mapped)
	}
	out = deepMerge(out, j.Overrides)
	if j.Host != "" { // last, so no values file can override the host can-core validates routes against
		out = deepMerge(out, map[string]any{"ingress": map[string]any{"host": j.Host}})
	}
	out["can"] = j.Can
	return out, nil
}

// renderJobInProcess does the work. It runs only inside the child.
func renderJobInProcess(ctx context.Context, j renderJob) ([]map[string]any, error) {
	vals, err := buildValues(ctx, j)
	if err != nil {
		return nil, err
	}
	return RenderChartContext(ctx, j.Chart, vals, j.Release)
}

// RunRenderChild is the body of `can-core render-chart`: read one job from stdin, write one result
// to stdout, exit code 0 even for a chart that fails to render (the error is in the result). The
// caller's flags set the limits; the child enforces them on itself so they hold however it is started.
func RunRenderChild(stdin io.Reader, stdout io.Writer, asMB, heapMB int) int {
	if err := setAddressSpaceLimit(asMB); err != nil {
		fmt.Fprintln(io.Discard, err)
		return 4
	}
	if heapMB > 0 {
		go heapWatchdog(heapMB)
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return 2
	}
	var j renderJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return 2
	}
	timeout := time.Duration(j.TimeoutSec) * time.Second
	if timeout <= 0 || timeout > renderTimeout*2 {
		timeout = renderTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var res renderResult
	objs, err := renderJobInProcess(ctx, j)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Objects = objs
	}
	if json.NewEncoder(stdout).Encode(res) != nil {
		return 3
	}
	return 0
}

// heapWatchdog ends the child when its live heap passes limitMB. The runtime's own out-of-memory
// failure is fatal anyway; this makes the exit prompt and well before the address-space limit, so a
// slowly growing render cannot take the container's memory from can-core.
func heapWatchdog(limitMB int) {
	var m runtime.MemStats
	for {
		time.Sleep(5 * time.Millisecond)
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > uint64(limitMB)<<20 {
			// exit without unwinding; the parent reports "killed: heap limit"
			exitHeapLimit()
		}
	}
}

var errNoChild = errors.New("the chart renderer process could not be started")
