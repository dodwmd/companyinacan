package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Hetzner provisions a Hetzner Cloud server with cloud-init.
type Hetzner struct{ deps Deps }

// Name implements Provider.
func (*Hetzner) Name() string { return "hetzner" }

// Defaults. UNVERIFIED against the live API: E00 confirms the cheapest server
// type that meets the footprint budget; operators can override every one.
const (
	defaultHetznerType     = "cx23"
	defaultHetznerImage    = "ubuntu-24.04"
	defaultHetznerLocation = "fsn1"
	defaultTokenRef        = "env:HCLOUD_TOKEN"
	defaultWaitSeconds     = 600
)

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(reSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "company"
	}
	return s
}

type hetznerConf struct {
	tokenRef   string
	serverName string
	fwName     string
	keyName    string
	typ        string
	image      string
	location   string
	keyFile    string
	operator   string
	wait       time.Duration
	target     Target
	slug       string
}

func (p *Hetzner) conf(spec Spec) (hetznerConf, error) {
	o := opts(spec.Options)
	s := slug(spec.Name)
	c := hetznerConf{
		tokenRef:   o.str("token_secret_ref", defaultTokenRef),
		serverName: o.str("server_name", "can-"+s),
		fwName:     "can-" + s,
		keyName:    "can-" + s,
		typ:        o.str("server_type", defaultHetznerType),
		image:      o.str("image", defaultHetznerImage),
		location:   o.str("location", defaultHetznerLocation),
		keyFile:    expandHome(o.str("ssh_public_key_file", "")),
		operator:   o.str("operator_ip", ""),
		wait:       time.Duration(o.integer("wait_seconds", defaultWaitSeconds)) * time.Second,
		target:     Target{User: "root", Port: 22, KeyFile: expandHome(o.str("ssh_private_key_file", "")), KnownHosts: knownHostsPath(spec.Dir)},
		slug:       s,
	}
	// The cloud firewall opens only 22 and the target above is fixed to 22, so a
	// different ssh port (which the host firewall would then enforce) would lock us out.
	if _, set := o["port"]; set && o.integer("port", 22) != 22 {
		return c, fmt.Errorf("provider.options.port is not supported for hetzner (SSH stays on 22)")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(c.serverName) {
		return c, fmt.Errorf("provider.options.server_name %q is not a valid hostname label", c.serverName)
	}
	return c, nil
}

func (p *Hetzner) client(c hetznerConf) (*HetznerClient, error) {
	tok, err := p.deps.ResolveSecret(c.tokenRef)
	if err != nil {
		return nil, err
	}
	return NewHetznerClient(p.deps.APIBase, tok, p.deps.HTTP, p.deps.Redactor, p.deps.Sleep), nil
}

func (p *Hetzner) publicKey(c hetznerConf) (string, error) {
	path := c.keyFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("provider.options.ssh_public_key_file is required (no home directory)")
		}
		path = filepath.Join(home, ".ssh", "id_ed25519.pub")
	}
	b, err := p.deps.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read SSH public key %s: set provider.options.ssh_public_key_file (%v)", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (c hetznerConf) rules() []FirewallRule {
	all := []string{"0.0.0.0/0", "::/0"}
	ssh := all
	if c.operator != "" {
		ssh = []string{operatorPrefix(c.operator)}
	}
	return []FirewallRule{
		{"in", "tcp", "22", ssh},
		{"in", "tcp", "80", all},
		{"in", "tcp", "443", all},
		{"in", "icmp", "", all},
	}
}

func operatorPrefix(s string) string {
	if strings.Contains(s, "/") {
		return s
	}
	if strings.Contains(s, ":") {
		return s + "/128"
	}
	return s + "/32"
}

// Plan implements Provider.
func (p *Hetzner) Plan(_ context.Context, spec Spec, st *Store) ([]Action, error) {
	c, err := p.conf(spec)
	if err != nil {
		return nil, err
	}
	if _, err := BootstrapOptionsFrom(spec); err != nil {
		return nil, err
	}
	state, err := st.Load()
	if err != nil {
		return nil, err
	}
	return []Action{
		{"hetzner.ssh-key", fmt.Sprintf("upload SSH public key %q (reused if the fingerprint already exists)", c.keyName), state.SSHKeyID != 0},
		{"hetzner.firewall", fmt.Sprintf("create cloud firewall %q: in 22 (%s), 80, 443, icmp", c.fwName, ifEmpty(c.operator, "any")), state.FirewallID != 0},
		{"hetzner.server", fmt.Sprintf("create server %q (%s, %s, %s) with cloud-init bootstrap", c.serverName, c.typ, c.image, c.location), state.Server != nil && state.Server.ID != 0},
		{"hetzner.bootstrap", "wait for cloud-init to finish installing k3s (marker " + BootstrapDoneMarker + ")", state.Done("hetzner.bootstrap")},
	}, nil
}

func ifEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Create implements Provider. Every resource is recorded in state as soon as it
// exists, so a failed or interrupted run resumes instead of duplicating.
func (p *Hetzner) Create(ctx context.Context, spec Spec, st *Store, out io.Writer) error {
	c, err := p.conf(spec)
	if err != nil {
		return err
	}
	bo, err := BootstrapOptionsFrom(spec)
	if err != nil {
		return err
	}
	userData, err := CloudInit(bo)
	if err != nil {
		return err
	}
	pub, err := p.publicKey(c)
	if err != nil {
		return err
	}
	fp, err := SSHFingerprint(pub)
	if err != nil {
		return err
	}
	api, err := p.client(c)
	if err != nil {
		return err
	}
	labels := map[string]string{"managed-by": "can", "company": slug(spec.Name)}
	state, err := st.Load()
	if err != nil {
		return err
	}

	// SSH key
	if state.SSHKeyID == 0 {
		key, found, err := api.FindSSHKey(ctx, fp)
		if err != nil {
			return err
		}
		owned := false
		if !found {
			if key, err = api.UploadSSHKey(ctx, c.keyName, pub, labels); err != nil {
				return err
			}
			owned = true
		}
		fmt.Fprintf(out, "ssh key: %s (id %d)\n", c.keyName, key.ID)
		if err := st.Mutate(func(s *State) error { s.Provider = "hetzner"; s.SSHKeyID, s.SSHKeyOwned = key.ID, owned; return nil }); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "ssh key: already created")
	}

	// Firewall
	if state, err = st.Load(); err != nil {
		return err
	}
	if state.FirewallID == 0 {
		fw, found, err := api.FindFirewall(ctx, c.fwName)
		if err != nil {
			return err
		}
		if found && !OwnedBy(fw.Labels, c.slug) {
			return fmt.Errorf("a firewall named %q already exists and was not created by can (labels managed-by=can, company=%s are missing); refusing to adopt it. Rename or delete it, or change the company name", c.fwName, c.slug)
		}
		if !found {
			if fw, err = api.CreateFirewall(ctx, c.fwName, c.rules(), labels); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "firewall: %s (id %d)\n", c.fwName, fw.ID)
		if err := st.Mutate(func(s *State) error { s.FirewallID = fw.ID; return nil }); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "firewall: already created")
	}

	// Server
	if state, err = st.Load(); err != nil {
		return err
	}
	var srv HServer
	haveSrv := false
	if state.Server != nil && state.Server.ID != 0 {
		s, err := api.GetServer(ctx, state.Server.ID)
		switch {
		case err == nil:
			if !OwnedBy(s.Labels, c.slug) {
				return fmt.Errorf("server %d in state is not labelled as created by can; refusing to use it", s.ID)
			}
			srv, haveSrv = s, true
		case !IsNotFound(err):
			return err
		}
	}
	if !haveSrv {
		s, found, err := api.FindServer(ctx, c.serverName)
		if err != nil {
			return err
		}
		if found && !OwnedBy(s.Labels, c.slug) {
			return fmt.Errorf("a server named %q already exists and was not created by can (labels managed-by=can, company=%s are missing); refusing to adopt it. Set provider.options.server_name or remove the server", c.serverName, c.slug)
		}
		srv, haveSrv = s, found
	}
	if !haveSrv {
		srv, err = api.CreateServer(ctx, CreateServerRequest{
			Name: c.serverName, ServerType: c.typ, Image: c.image, Location: c.location,
			SSHKeys: []int64{state.SSHKeyID}, Firewalls: []FirewallRef{{state.FirewallID}},
			UserData: userData, Labels: labels, StartAfter: true,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "server: created %s (id %d)\n", c.serverName, srv.ID)
	} else {
		fmt.Fprintf(out, "server: %s already exists (id %d)\n", srv.Name, srv.ID)
	}
	if err := st.Mutate(func(s *State) error {
		s.Server = &ServerState{ID: srv.ID, Name: srv.Name, IP: srv.PublicNet.IPv4.IP, User: "root", Port: 22}
		return nil
	}); err != nil {
		return err
	}

	deadline := time.Now().Add(c.wait)
	for srv.Status != "running" || srv.PublicNet.IPv4.IP == "" {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("server %d did not reach running with an IPv4 address in %s", srv.ID, c.wait)
		}
		p.deps.Sleep(ctx, 3*time.Second)
		if srv, err = api.GetServer(ctx, srv.ID); err != nil {
			return err
		}
	}
	ip := srv.PublicNet.IPv4.IP
	if err := st.Mutate(func(s *State) error { s.Server.IP = ip; return nil }); err != nil {
		return err
	}
	fmt.Fprintf(out, "server: running at %s\n", ip)

	// Wait for cloud-init to finish the bootstrap.
	if state.Done("hetzner.bootstrap") {
		return nil
	}
	t := c.target
	t.Host = ip
	r := p.deps.Runner(t)
	fmt.Fprintln(out, "bootstrap: waiting for cloud-init (k3s install)")
	var lastErr error
	for {
		o, err := r.Run(ctx, StatusScript())
		if err != nil {
			lastErr = err
		}
		if err == nil && BootstrappedVersion(o) != "" {
			ver := BootstrappedVersion(o)
			fmt.Fprintf(out, "bootstrap: done (k3s %s)\n", ver)
			return st.Mutate(func(s *State) error { s.K3sVersion = ver; s.Mark("hetzner.bootstrap", st.Time()); return nil })
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			detail := ""
			if lastErr != nil {
				detail = " (last ssh error: " + p.deps.Redactor.Redact(lastErr.Error()) + ")"
			}
			return fmt.Errorf("cloud-init did not finish the bootstrap within %s%s; inspect /var/log/can-bootstrap.log on %s", c.wait, detail, ip)
		}
		p.deps.Sleep(ctx, 5*time.Second)
	}
}

// Destroy implements Provider: delete the server, then the firewall, then the
// SSH key if can uploaded it.
func (p *Hetzner) Destroy(ctx context.Context, spec Spec, st *Store, out io.Writer) error {
	c, err := p.conf(spec)
	if err != nil {
		return err
	}
	state, err := st.Load()
	if err != nil {
		return err
	}
	if state.Server == nil && state.FirewallID == 0 && state.SSHKeyID == 0 {
		fmt.Fprintln(out, "nothing to do: no hetzner resources recorded in state")
		return nil
	}
	api, err := p.client(c)
	if err != nil {
		return err
	}
	if state.Server != nil && state.Server.ID != 0 {
		// Re-check ownership right before deleting: a server whose id is in
		// state but whose labels are not ours is never touched.
		srv, err := api.GetServer(ctx, state.Server.ID)
		switch {
		case IsNotFound(err):
			fmt.Fprintf(out, "server %d already gone\n", state.Server.ID)
		case err != nil:
			return err
		case !OwnedBy(srv.Labels, c.slug):
			return fmt.Errorf("refusing to delete server %d: it is not labelled managed-by=can, company=%s", srv.ID, c.slug)
		default:
			if err := api.DeleteServer(ctx, srv.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "server %d deleting\n", srv.ID)
			if err := p.waitServerGone(ctx, api, srv.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "server %d deleted\n", srv.ID)
		}
		if err := st.Mutate(func(s *State) error { s.Server = nil; return nil }); err != nil {
			return err
		}
	}
	if state.FirewallID != 0 {
		fw, err := api.GetFirewall(ctx, state.FirewallID)
		switch {
		case IsNotFound(err):
		case err != nil:
			return err
		case !OwnedBy(fw.Labels, c.slug):
			return fmt.Errorf("refusing to delete firewall %d: it is not labelled managed-by=can, company=%s", fw.ID, c.slug)
		default:
			if err := api.DeleteFirewall(ctx, fw.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "firewall %d deleted\n", fw.ID)
		}
		if err := st.Mutate(func(s *State) error { s.FirewallID = 0; return nil }); err != nil {
			return err
		}
	}
	if state.SSHKeyID != 0 {
		if state.SSHKeyOwned {
			if err := api.DeleteSSHKey(ctx, state.SSHKeyID); err != nil {
				return err
			}
			fmt.Fprintf(out, "ssh key %d deleted\n", state.SSHKeyID)
		} else {
			fmt.Fprintf(out, "ssh key %d kept (not uploaded by can)\n", state.SSHKeyID)
		}
	}
	return st.Remove()
}

// Status implements Provider.
func (p *Hetzner) Status(ctx context.Context, spec Spec, st *Store) (Status, error) {
	c, err := p.conf(spec)
	if err != nil {
		return Status{}, err
	}
	state, err := st.Load()
	if err != nil {
		return Status{}, err
	}
	if state.Server == nil || state.Server.ID == 0 {
		return Status{Phase: PhaseAbsent}, nil
	}
	api, err := p.client(c)
	if err != nil {
		return Status{}, err
	}
	srv, err := api.GetServer(ctx, state.Server.ID)
	if IsNotFound(err) {
		return Status{Phase: PhaseAbsent, Checks: []Check{{"hetzner server", false, "recorded server no longer exists"}}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	srvCheck := Check{"hetzner server", srv.Status == "running", srv.Status}
	if srv.Status != "running" {
		return Status{Phase: PhaseProvisioning, Checks: []Check{srvCheck}}, nil
	}
	t := c.target
	t.Host = srv.PublicNet.IPv4.IP
	hs := HostStatus(ctx, p.deps.Runner(t))
	hs.Checks = append([]Check{srvCheck}, hs.Checks...)
	return hs, nil
}

// Address implements Provider.
func (*Hetzner) Address(st *Store) (string, error) {
	state, err := st.Load()
	if err != nil {
		return "", err
	}
	if state.Server == nil || state.Server.IP == "" {
		return "", fmt.Errorf("no server recorded yet; run `can up` first")
	}
	return state.Server.IP, nil
}

// waitServerGone polls until the server no longer exists (deletion is
// asynchronous and a firewall cannot be removed while it is still attached).
func (p *Hetzner) waitServerGone(ctx context.Context, api *HetznerClient, id int64) error {
	for i := 0; i < 60; i++ {
		_, err := api.GetServer(ctx, id)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		p.deps.Sleep(ctx, 2*time.Second)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("server %d still exists after deletion was requested", id)
}
