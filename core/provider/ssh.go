package provider

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// SSH is the bring-your-own-server provider: any Linux host reachable over SSH.
type SSH struct{ deps Deps }

// Name implements Provider.
func (*SSH) Name() string { return "ssh" }

const bootstrapPath = "/usr/local/sbin/can-bootstrap.sh"

func (p *SSH) target(spec Spec) (Target, error) {
	o := opts(spec.Options)
	host := o.str("host", "")
	if host == "" {
		return Target{}, fmt.Errorf("provider.options.host is required for the ssh provider")
	}
	t := Target{Host: host, User: o.str("user", "root"), Port: o.integer("port", 22), KeyFile: o.str("key_file", "")}
	if _, err := SSHArgs(t, nil, ""); err != nil {
		return Target{}, err
	}
	return t, nil
}

// BootstrapOptionsFrom reads the bootstrap-related provider options shared by
// the ssh and hetzner providers.
func BootstrapOptionsFrom(spec Spec) (BootstrapOptions, error) {
	o := opts(spec.Options)
	bo := BootstrapOptions{
		K3sVersion:   o.str("k3s_version", ""),
		NodeName:     o.str("node_name", ""),
		SSHPort:      o.integer("port", 22),
		OperatorCIDR: o.str("operator_ip", ""),
		SwapMB:       o.integer("swap_mb", 0),
	}
	if l, ok := spec.Options["image_tarballs"].([]any); ok {
		for i, e := range l {
			m, ok := e.(map[string]any)
			if !ok {
				return bo, fmt.Errorf("provider.options.image_tarballs[%d]: want a mapping with name, url, sha256", i)
			}
			om := opts(m)
			bo.ImageTarballs = append(bo.ImageTarballs, ImageTarball{Name: om.str("name", ""), URL: om.str("url", ""), SHA256: om.str("sha256", "")})
		}
	}
	return bo, bo.normalise()
}

// Plan implements Provider.
func (p *SSH) Plan(_ context.Context, spec Spec, st *Store) ([]Action, error) {
	t, err := p.target(spec)
	if err != nil {
		return nil, err
	}
	bo, err := BootstrapOptionsFrom(spec)
	if err != nil {
		return nil, err
	}
	state, err := st.Load()
	if err != nil {
		return nil, err
	}
	ver := state.K3sVersion
	return []Action{
		{"ssh.preflight", fmt.Sprintf("check %s over ssh: OS, RAM >= %d MB, disk >= %d GB, ports 80/443/6443 free", t.Host, MinMemMB, MinDiskMB/1024), false},
		{"ssh.bootstrap", fmt.Sprintf("install k3s %s (trimmed flags, kubelet fail-swap-on=false), swap, host firewall (22/80/443 + pod/service CIDR), image pre-seed (%d tarball(s))", bo.K3sVersion, len(bo.ImageTarballs)), ver == bo.K3sVersion && state.Done("ssh.bootstrap")},
	}, nil
}

// Create implements Provider. It is idempotent: a host that already carries
// the pinned k3s version is left alone.
func (p *SSH) Create(ctx context.Context, spec Spec, st *Store, out io.Writer) error {
	t, err := p.target(spec)
	if err != nil {
		return err
	}
	bo, err := BootstrapOptionsFrom(spec)
	if err != nil {
		return err
	}
	script, err := Bootstrap(bo)
	if err != nil {
		return err
	}
	r := p.deps.Runner(t)

	fmt.Fprintf(out, "preflight: probing %s\n", t.Host)
	probe, err := r.Run(ctx, PreflightScript())
	if err != nil {
		return p.deps.Redactor.Err(fmt.Errorf("preflight probe failed: %w", err))
	}
	checks := ParsePreflight(probe)
	for _, c := range checks {
		fmt.Fprintf(out, "  %-8s %s\n", okWord(c.OK), c.Name+": "+c.Detail)
	}
	if err := PreflightError(checks); err != nil {
		return err
	}

	if err := st.Mutate(func(s *State) error {
		s.Provider = "ssh"
		s.Server = &ServerState{IP: t.Host, User: t.user(), Port: t.port()}
		return nil
	}); err != nil {
		return err
	}

	if BootstrappedVersion(probe) == bo.K3sVersion {
		fmt.Fprintf(out, "bootstrap: k3s %s already installed, skipping\n", bo.K3sVersion)
	} else {
		fmt.Fprintf(out, "bootstrap: installing k3s %s\n", bo.K3sVersion)
		if err := r.Copy(ctx, bootstrapPath, []byte(script), "0700"); err != nil {
			return p.deps.Redactor.Err(fmt.Errorf("upload bootstrap script: %w", err))
		}
		log, err := r.Run(ctx, "bash "+bootstrapPath)
		fmt.Fprint(out, indent(p.deps.Redactor.Redact(log)))
		if err != nil {
			return p.deps.Redactor.Err(fmt.Errorf("bootstrap failed: %w", err))
		}
	}
	return st.Mutate(func(s *State) error {
		s.K3sVersion = bo.K3sVersion
		s.Mark("ssh.bootstrap", st.Time())
		return nil
	})
}

// Destroy implements Provider: uninstall k3s and can state, leave the box.
func (p *SSH) Destroy(ctx context.Context, spec Spec, st *Store, out io.Writer) error {
	state, err := st.Load()
	if err != nil {
		return err
	}
	if state.Server == nil {
		fmt.Fprintln(out, "nothing to do: no server recorded in state")
		return nil
	}
	t, err := p.target(spec)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "uninstalling k3s and can-core from %s (the server itself is left as is)\n", t.Host)
	log, err := p.deps.Runner(t).Run(ctx, UninstallScript())
	fmt.Fprint(out, indent(p.deps.Redactor.Redact(log)))
	if err != nil {
		return p.deps.Redactor.Err(fmt.Errorf("uninstall failed: %w", err))
	}
	return st.Remove()
}

// StatusScript is the read-only probe behind Status.
func StatusScript() string {
	return `#!/usr/bin/env bash
set -u
echo "bootstrapped=$(cat /var/lib/can/bootstrap.done 2>/dev/null || true)"
echo "k3s_active=$(systemctl is-active k3s 2>/dev/null || true)"
ready=$(k3s kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready' || true)
echo "nodes_ready=${ready:-0}"
`
}

// HostStatus interprets StatusScript output. An unreachable host is reported
// as degraded (not an error) so `can status` can still print something.
func HostStatus(ctx context.Context, r Runner) Status {
	out, err := r.Run(ctx, StatusScript())
	if err != nil {
		return Status{Phase: PhaseDegraded, Checks: []Check{{"ssh", false, "host unreachable or command failed"}}}
	}
	kv := parseKV(out)
	checks := []Check{
		{"ssh", true, ""},
		{"bootstrap", kv["bootstrapped"] != "", kv["bootstrapped"]},
		{"k3s service", kv["k3s_active"] == "active", kv["k3s_active"]},
		{"node ready", kv["nodes_ready"] == "1", kv["nodes_ready"] + " ready"},
	}
	phase := PhaseReady
	switch {
	case PreflightOK(checks):
	case kv["bootstrapped"] == "":
		phase = PhaseProvisioning
	default:
		phase = PhaseDegraded
	}
	return Status{Phase: phase, Checks: checks}
}

// Status implements Provider.
func (p *SSH) Status(ctx context.Context, spec Spec, st *Store) (Status, error) {
	state, err := st.Load()
	if err != nil {
		return Status{}, err
	}
	if state.Server == nil {
		return Status{Phase: PhaseAbsent}, nil
	}
	t, err := p.target(spec)
	if err != nil {
		return Status{}, err
	}
	return HostStatus(ctx, p.deps.Runner(t)), nil
}

// Address implements Provider.
func (*SSH) Address(st *Store) (string, error) {
	state, err := st.Load()
	if err != nil {
		return "", err
	}
	if state.Server == nil {
		return "", fmt.Errorf("no server recorded yet; run `can up` first")
	}
	return state.Server.IP, nil
}

// TargetFor returns the SSH target of a provisioned company (used by the CLI
// for the kubeconfig fetch and the tunnel).
func TargetFor(spec Spec, st *Store, deps Deps) (Target, error) {
	deps.defaults()
	state, err := st.Load()
	if err != nil {
		return Target{}, err
	}
	if state.Server == nil {
		return Target{}, fmt.Errorf("no server recorded yet; run `can up` first")
	}
	o := opts(spec.Options)
	return Target{Host: state.Server.IP, User: o.str("user", "root"), Port: o.integer("port", 22), KeyFile: o.str("key_file", "")}, nil
}

func okWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAIL"
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ") + "\n"
}
