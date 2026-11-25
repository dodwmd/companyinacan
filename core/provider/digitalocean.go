package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DigitalOcean provisions a Droplet with cloud-init, a Cloud Firewall and an
// SSH key. It mirrors the Hetzner provider, including its safety rules:
// nothing without can's tags is ever adopted or deleted.
type DigitalOcean struct{ deps Deps }

// Name implements Provider.
func (*DigitalOcean) Name() string { return "digitalocean" }

// Defaults. UNVERIFIED against a live account: the size slug and price come from
// DigitalOcean's pricing page (specs/020-digitalocean-cloudflare/research.md); the
// image and region slugs are from memory of the public slug list. Operators can override all three.
const (
	defaultDOSize        = "s-1vcpu-2gb"
	defaultDOImage       = "ubuntu-24-04-x64"
	defaultDORegion      = "fra1"
	defaultDOTokenRef    = "env:DIGITALOCEAN_TOKEN"
	doPollInterval       = 3 * time.Second
	doBootstrapPollEvery = 5 * time.Second
)

// doOptionKeys are every provider.options key the provider accepts: its own
// plus the bootstrap keys shared with ssh and hetzner. Anything else is an error.
var doOptionKeys = []string{
	"token_secret_ref", "region", "size", "image", "server_name", "operator_ip", "wait_seconds",
	"ssh_public_key_file", "ssh_private_key_file", "ssh_key_fingerprints", "port",
	"k3s_version", "node_name", "swap_mb", "image_tarballs",
}

// DigitalOceanOptionKeys returns the accepted provider.options keys, sorted.
func DigitalOceanOptionKeys() []string {
	k := append([]string(nil), doOptionKeys...)
	sort.Strings(k)
	return k
}

var (
	reDOSlug        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	reDOFingerprint = regexp.MustCompile(`^([0-9a-f]{2}:){15}[0-9a-f]{2}$`)
	reDOName        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

type doConf struct {
	tokenRef   string
	serverName string
	fwName     string
	keyName    string
	region     string
	size       string
	image      string
	keyFile    string
	extraKeys  []string
	operator   string
	wait       time.Duration
	target     Target
	slug       string
	ownerTags  []string
}

func (p *DigitalOcean) conf(spec Spec) (doConf, error) {
	o := opts(spec.Options)
	for k := range spec.Options {
		known := false
		for _, a := range doOptionKeys {
			known = known || a == k
		}
		if !known {
			return doConf{}, fmt.Errorf("provider.options.%s is not a digitalocean option (known: %s)", k, strings.Join(DigitalOceanOptionKeys(), ", "))
		}
	}
	s := slug(spec.Name)
	c := doConf{
		tokenRef:   o.str("token_secret_ref", defaultDOTokenRef),
		serverName: o.str("server_name", "can-"+s),
		fwName:     "can-" + s,
		keyName:    "can-" + s,
		region:     o.str("region", defaultDORegion),
		size:       o.str("size", defaultDOSize),
		image:      o.str("image", defaultDOImage),
		keyFile:    expandHome(o.str("ssh_public_key_file", "")),
		operator:   o.str("operator_ip", ""),
		target:     Target{User: "root", Port: 22, KeyFile: expandHome(o.str("ssh_private_key_file", "")), KnownHosts: knownHostsPath(spec.Dir)},
		slug:       s,
		ownerTags:  []string{"managed-by:can", "company:" + s},
	}
	// The cloud firewall opens only 22, so any other SSH port would lock us out.
	port, err := o.integerE("port", 22)
	if err != nil {
		return c, err
	}
	if port != 22 {
		return c, fmt.Errorf("provider.options.port is not supported for digitalocean (SSH stays on 22)")
	}
	wait, err := o.integerE("wait_seconds", defaultWaitSeconds)
	if err != nil {
		return c, err
	}
	c.wait = time.Duration(wait) * time.Second
	if _, err := operatorIPString(spec.Options); err != nil {
		return c, err
	}
	if err := CheckOperatorIP(c.operator); err != nil {
		return c, err
	}
	if !reDOName.MatchString(c.serverName) {
		return c, fmt.Errorf("provider.options.server_name %q is not a valid hostname label", c.serverName)
	}
	for _, kv := range []struct{ k, v string }{{"region", c.region}, {"size", c.size}, {"image", c.image}} {
		if !reDOSlug.MatchString(kv.v) {
			return c, fmt.Errorf("provider.options.%s %q is not a valid DigitalOcean slug", kv.k, kv.v)
		}
	}
	if l, ok := spec.Options["ssh_key_fingerprints"]; ok && l != nil {
		items, ok := l.([]any)
		if !ok {
			return c, fmt.Errorf("provider.options.ssh_key_fingerprints must be a list of MD5 fingerprints (aa:bb:...)")
		}
		for _, it := range items {
			fp, _ := it.(string)
			if !reDOFingerprint.MatchString(fp) {
				return c, fmt.Errorf("provider.options.ssh_key_fingerprints: %v is not an MD5 fingerprint (aa:bb:...)", it)
			}
			c.extraKeys = append(c.extraKeys, fp)
		}
	}
	return c, nil
}

func (p *DigitalOcean) client(c doConf) (*DOClient, error) {
	tok, err := p.deps.ResolveSecret(c.tokenRef)
	if err != nil {
		return nil, err
	}
	return NewDOClient(p.deps.DOBase, tok, p.deps.HTTP, p.deps.Redactor, p.deps.Sleep), nil
}

func (p *DigitalOcean) publicKey(c doConf) (string, error) {
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

func (c doConf) firewall() DOCreateFirewall {
	all := []string{"0.0.0.0/0", "::/0"}
	ssh := all
	if c.operator != "" {
		ssh = []string{operatorPrefix(c.operator)}
	}
	in := func(proto, ports string, src []string) DOFirewallRule {
		return DOFirewallRule{Protocol: proto, Ports: ports, Sources: &DOAddresses{Addresses: src}}
	}
	out := func(proto, ports string) DOFirewallRule {
		return DOFirewallRule{Protocol: proto, Ports: ports, Destinations: &DOAddresses{Addresses: all}}
	}
	return DOCreateFirewall{
		Name: c.fwName,
		InboundRules: []DOFirewallRule{
			in("tcp", "22", ssh), in("tcp", "80", all), in("tcp", "443", all), in("icmp", "", all),
		},
		// A DigitalOcean firewall that lists no outbound rules blocks all outbound
		// traffic, which would break image pulls, ACME and updates.
		OutboundRule: []DOFirewallRule{out("tcp", "0"), out("udp", "0"), out("icmp", "")},
		Tags:         c.ownerTags,
	}
}

// ownedFirewall reports whether fw is can's: the expected name, targeting
// exactly the two owner tags, and attached by id to no droplet other than
// ours (serverID, 0 if not known yet).
func (c doConf) ownedFirewall(fw DOFirewall, serverID int64) bool {
	if fw.Name != c.fwName || len(fw.Tags) != len(c.ownerTags) || !OwnedByTags(fw.Tags, c.slug) {
		return false
	}
	for _, id := range fw.DropletIDs {
		if id != serverID {
			return false
		}
	}
	return true
}

// ruleKeys is a comparable form of inbound rules.
func ruleKeys(rs []DOFirewallRule) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		var addrs []string
		if r.Sources != nil {
			addrs = append(addrs, r.Sources.Addresses...)
		}
		sort.Strings(addrs)
		ports := r.Ports
		if r.Protocol == "icmp" {
			ports = ""
		}
		out = append(out, r.Protocol+"|"+ports+"|"+strings.Join(addrs, ","))
	}
	sort.Strings(out)
	return out
}

func sameRules(a, b []DOFirewallRule) bool {
	x, y := ruleKeys(a), ruleKeys(b)
	return strings.Join(x, ";") == strings.Join(y, ";")
}

// Plan implements Provider.
func (p *DigitalOcean) Plan(_ context.Context, spec Spec, st *Store) ([]Action, error) {
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
		{"digitalocean.ssh-key", fmt.Sprintf("upload SSH public key %q (reused if the fingerprint already exists)", c.keyName), state.SSHKeyID != 0},
		{"digitalocean.firewall", fmt.Sprintf("create cloud firewall %q: in 22 (%s), 80, 443, icmp; applies to droplets tagged %s", c.fwName, ifEmpty(c.operator, "any"), strings.Join(c.ownerTags, ", ")), state.FirewallUUID != ""},
		{"digitalocean.droplet", fmt.Sprintf("create droplet %q (%s, %s, %s) tagged %s with cloud-init bootstrap", c.serverName, c.size, c.image, c.region, strings.Join(c.ownerTags, ", ")), state.Server != nil && state.Server.ID != 0},
		{"digitalocean.bootstrap", "wait for cloud-init to finish installing k3s (marker " + BootstrapDoneMarker + ")", state.Done("digitalocean.bootstrap")},
	}, nil
}

// findOwnedDroplet finds the company's droplet by name. A same-named droplet
// without can's tags is an error (never adopted).
func (p *DigitalOcean) findDroplet(ctx context.Context, api *DOClient, c doConf) (DODroplet, bool, error) {
	list, err := api.ListDroplets(ctx, c.serverName, "")
	if err != nil {
		return DODroplet{}, false, err
	}
	var foreign bool
	for _, d := range list {
		if d.Name != c.serverName {
			continue
		}
		if OwnedByTags(d.Tags, c.slug) {
			return d, true, nil
		}
		foreign = true
	}
	if foreign {
		return DODroplet{}, false, fmt.Errorf("a droplet named %q already exists and was not created by can (tags managed-by:can, company:%s are missing); refusing to adopt it. Set provider.options.server_name or remove the droplet", c.serverName, c.slug)
	}
	return DODroplet{}, false, nil
}

func (p *DigitalOcean) findFirewall(ctx context.Context, api *DOClient, c doConf, serverID int64) (DOFirewall, bool, error) {
	list, err := api.ListFirewalls(ctx)
	if err != nil {
		return DOFirewall{}, false, err
	}
	var foreign bool
	for _, fw := range list {
		if fw.Name != c.fwName {
			continue
		}
		if c.ownedFirewall(fw, serverID) {
			return fw, true, nil
		}
		foreign = true
	}
	if foreign {
		return DOFirewall{}, false, fmt.Errorf("a firewall named %q already exists and was not created by can (it must target exactly the tags managed-by:can, company:%s and be attached to no other droplet); refusing to adopt it. Rename or delete it, or change the company name", c.fwName, c.slug)
	}
	return DOFirewall{}, false, nil
}

// Create implements Provider. Every resource is recorded in state as soon as it
// exists, so a failed or interrupted run resumes instead of duplicating.
func (p *DigitalOcean) Create(ctx context.Context, spec Spec, st *Store, out io.Writer) error {
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
	state, err := st.Load()
	if err != nil {
		return err
	}

	// SSH key
	if state.SSHKeyID == 0 {
		key, found, err := api.FindKey(ctx, fp)
		if err != nil {
			return err
		}
		// Only a key uploaded by this run is owned. A key that already exists is
		// reused and never deleted, even one named can-<slug>: after a crash it
		// stays behind (a public key, harmless) rather than risk deleting someone else's.
		owned := false
		if !found {
			if key, err = api.UploadKey(ctx, c.keyName, pub); err != nil {
				return err
			}
			owned = true
		}
		fmt.Fprintf(out, "ssh key: %s (id %d)\n", key.Name, key.ID)
		if err := st.Mutate(func(s *State) error {
			s.Provider = "digitalocean"
			s.SSHKeyID, s.SSHKeyOwned = key.ID, owned
			return nil
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "ssh key: already created")
	}

	// Firewall
	if state, err = st.Load(); err != nil {
		return err
	}
	if state.FirewallUUID == "" {
		var sid int64
		if state.Server != nil {
			sid = state.Server.ID
		}
		fw, found, err := p.findFirewall(ctx, api, c, sid)
		if err != nil {
			return err
		}
		if !found {
			if fw, err = api.CreateFirewall(ctx, c.firewall()); err != nil {
				return err
			}
		} else if want := c.firewall(); !sameRules(fw.InboundRules, want.InboundRules) {
			// an adopted firewall must not silently keep old rules (for example an old operator_ip)
			fmt.Fprintf(out, "firewall: rules differ from the configuration; updating %s\n", fw.ID)
			if err := api.UpdateFirewall(ctx, fw.ID, want, fw.DropletIDs); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "firewall: %s (id %s)\n", c.fwName, fw.ID)
		if err := st.Mutate(func(s *State) error { s.FirewallUUID = fw.ID; return nil }); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "firewall: already created")
	}

	// Droplet
	if state, err = st.Load(); err != nil {
		return err
	}
	var drop DODroplet
	haveDrop := false
	var actionID int64
	if state.Server != nil && state.Server.ID != 0 {
		d, err := api.GetDroplet(ctx, state.Server.ID)
		switch {
		case err == nil:
			if !OwnedByTags(d.Tags, c.slug) {
				return fmt.Errorf("droplet %d in state is not tagged as created by can; refusing to use it", d.ID)
			}
			drop, haveDrop = d, true
		case !IsNotFound(err):
			return err
		}
	}
	if !haveDrop {
		if drop, haveDrop, err = p.findDroplet(ctx, api, c); err != nil {
			return err
		}
	}
	if !haveDrop {
		keys := []any{state.SSHKeyID}
		for _, k := range c.extraKeys {
			keys = append(keys, k)
		}
		drop, actionID, err = api.CreateDroplet(ctx, DOCreateDroplet{
			Name: c.serverName, Region: c.region, Size: c.size, Image: c.image,
			SSHKeys: keys, UserData: userData, Tags: c.ownerTags,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "droplet: created %s (id %d)\n", c.serverName, drop.ID)
	} else {
		fmt.Fprintf(out, "droplet: %s already exists (id %d)\n", drop.Name, drop.ID)
	}
	// Saved before any waiting, so a crash here resumes by id.
	if err := st.Mutate(func(s *State) error {
		s.Server = &ServerState{ID: drop.ID, Name: drop.Name, IP: drop.PublicIPv4(), User: "root", Port: 22}
		return nil
	}); err != nil {
		return err
	}

	polls := int(c.wait / doPollInterval)
	if polls < 1 {
		polls = 1
	}
	timeout := fmt.Errorf("droplet %d did not reach active with a public IPv4 address in %s", drop.ID, c.wait)
	if actionID != 0 {
		done := false
		for i := 0; i < polls && !done; i++ {
			status, err := api.ActionStatus(ctx, actionID)
			if err != nil {
				return err
			}
			switch status {
			case "completed":
				done = true
			case "errored":
				return fmt.Errorf("droplet %d create action %d errored; check the DigitalOcean control panel", drop.ID, actionID)
			default:
				p.deps.Sleep(ctx, doPollInterval)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
		}
		if !done {
			return timeout
		}
	}
	for i := 0; drop.Status != "active" || drop.PublicIPv4() == ""; i++ {
		if i >= polls {
			return timeout
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.deps.Sleep(ctx, doPollInterval)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if drop, err = api.GetDroplet(ctx, drop.ID); err != nil {
			return err
		}
	}
	ip := drop.PublicIPv4()
	if err := st.Mutate(func(s *State) error { s.Server.IP = ip; return nil }); err != nil {
		return err
	}
	fmt.Fprintf(out, "droplet: active at %s\n", ip)

	if state, err = st.Load(); err != nil {
		return err
	}
	if state.Done("digitalocean.bootstrap") {
		return nil
	}
	t := c.target
	t.Host = ip
	r := p.deps.Runner(t)
	fmt.Fprintln(out, "bootstrap: waiting for cloud-init (k3s install)")
	var lastErr error
	bootPolls := int(c.wait / doBootstrapPollEvery)
	for i := 0; ; i++ {
		o, err := r.Run(ctx, StatusScript())
		if err != nil {
			lastErr = err
		}
		if err == nil && BootstrappedVersion(o) != "" {
			ver := BootstrappedVersion(o)
			fmt.Fprintf(out, "bootstrap: done (k3s %s)\n", ver)
			return st.Mutate(func(s *State) error { s.K3sVersion = ver; s.Mark("digitalocean.bootstrap", st.Time()); return nil })
		}
		if i >= bootPolls || ctx.Err() != nil {
			detail := ""
			if lastErr != nil {
				detail = " (last ssh error: " + p.deps.Redactor.Redact(lastErr.Error()) + ")"
			}
			return fmt.Errorf("cloud-init did not finish the bootstrap within %s%s; inspect /var/log/can-bootstrap.log on %s", c.wait, detail, ip)
		}
		p.deps.Sleep(ctx, doBootstrapPollEvery)
	}
}

// Destroy implements Provider: delete the droplet (and wait until it is gone),
// then the firewall, then the SSH key if can uploaded it.
func (p *DigitalOcean) Destroy(ctx context.Context, spec Spec, st *Store, out io.Writer) error {
	c, err := p.conf(spec)
	if err != nil {
		return err
	}
	state, err := st.Load()
	if err != nil {
		return err
	}
	api, err := p.client(c)
	if err != nil {
		return err
	}
	if state.Server == nil && state.FirewallUUID == "" && state.SSHKeyID == 0 {
		// A crash after create but before state was saved must not leak
		// resources: look for ours by tags. Anything without them is left alone.
		if state, err = p.discover(ctx, api, c, out); err != nil {
			return err
		}
		if state.Server == nil && state.FirewallUUID == "" && state.SSHKeyID == 0 {
			fmt.Fprintln(out, "nothing to do: no digitalocean resources of this company found")
			return nil
		}
	}
	if state.Server == nil || state.Server.ID == 0 {
		// State may hold the key and firewall but no droplet (a crash after the
		// droplet was created, before its id was saved): look it up by tag so it
		// is not orphaned (and left unprotected) when the firewall goes.
		drops, err := api.ListDroplets(ctx, "", "company:"+c.slug)
		if err != nil {
			return err
		}
		d, found, err := ownedDroplet(drops, c)
		if err != nil {
			return err
		}
		if found {
			state.Server = &ServerState{ID: d.ID, Name: d.Name, IP: d.PublicIPv4(), User: "root", Port: 22}
			fmt.Fprintf(out, "found droplet %d by name and tags\n", d.ID)
		}
	}
	if state.Server != nil && state.Server.ID != 0 {
		// Re-check the tags right before deleting.
		d, err := api.GetDroplet(ctx, state.Server.ID)
		switch {
		case IsNotFound(err):
			fmt.Fprintf(out, "droplet %d already gone\n", state.Server.ID)
		case err != nil:
			return err
		case !OwnedByTags(d.Tags, c.slug):
			return fmt.Errorf("refusing to delete droplet %d: it is not tagged managed-by:can, company:%s", d.ID, c.slug)
		default:
			if err := api.DeleteDroplet(ctx, d.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "droplet %d deleting\n", d.ID)
			if err := p.waitDropletGone(ctx, api, d.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "droplet %d deleted\n", d.ID)
		}
		if err := st.Mutate(func(s *State) error { s.Server = nil; return nil }); err != nil {
			return err
		}
	}
	if state.FirewallUUID != "" {
		fw, err := api.GetFirewall(ctx, state.FirewallUUID)
		switch {
		case IsNotFound(err):
		case err != nil:
			return err
		case !c.ownedFirewall(fw, serverID(state)):
			return fmt.Errorf("refusing to delete firewall %s: it is not named %q targeting exactly the tags managed-by:can, company:%s, or it is attached to another droplet", fw.ID, c.fwName, c.slug)
		default:
			if err := api.DeleteFirewall(ctx, fw.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "firewall %s deleted\n", fw.ID)
		}
		if err := st.Mutate(func(s *State) error { s.FirewallUUID = ""; return nil }); err != nil {
			return err
		}
	}
	if state.SSHKeyID != 0 {
		if state.SSHKeyOwned {
			if err := p.deleteOwnedKey(ctx, api, c, state.SSHKeyID, out); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(out, "ssh key %d kept (not uploaded by can)\n", state.SSHKeyID)
		}
	}
	return st.Remove()
}

// ownedDroplet picks the one droplet with our name and both owner tags. If
// several match (two companies with the same slug in one account, or a manual
// copy) it refuses rather than guess which to delete.
func ownedDroplet(drops []DODroplet, c doConf) (DODroplet, bool, error) {
	var hit []DODroplet
	for _, d := range drops {
		if d.Name == c.serverName && OwnedByTags(d.Tags, c.slug) {
			hit = append(hit, d)
		}
	}
	switch len(hit) {
	case 0:
		return DODroplet{}, false, nil
	case 1:
		return hit[0], true, nil
	}
	ids := make([]string, len(hit))
	for i, d := range hit {
		ids[i] = strconv.FormatInt(d.ID, 10)
	}
	return DODroplet{}, false, fmt.Errorf("refusing to choose: droplets %s all have the name %q and the tags managed-by:can, company:%s. Delete the extra ones in the DigitalOcean control panel (or rename one), then run `can down` again", strings.Join(ids, " and "), c.serverName, c.slug)
}

func serverID(s *State) int64 {
	if s.Server != nil {
		return s.Server.ID
	}
	return 0
}

// deleteOwnedKey deletes the key can uploaded, after checking that it still has
// the expected name and our key's fingerprint.
func (p *DigitalOcean) deleteOwnedKey(ctx context.Context, api *DOClient, c doConf, id int64, out io.Writer) error {
	key, err := api.GetKey(ctx, id)
	if IsNotFound(err) {
		fmt.Fprintf(out, "ssh key %d already gone\n", id)
		return nil
	}
	if err != nil {
		return err
	}
	pub, perr := p.publicKey(c)
	var fp string
	if perr == nil {
		fp, perr = SSHFingerprint(pub)
	}
	if perr != nil || key.Name != c.keyName || key.Fingerprint != fp {
		fmt.Fprintf(out, "ssh key %d kept (its name or fingerprint no longer matches what can uploaded)\n", id)
		return nil
	}
	if err := api.DeleteKey(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "ssh key %d deleted\n", id)
	return nil
}

// waitDropletGone polls until the droplet no longer exists.
func (p *DigitalOcean) waitDropletGone(ctx context.Context, api *DOClient, id int64) error {
	for i := 0; i < 60; i++ {
		_, err := api.GetDroplet(ctx, id)
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
	return fmt.Errorf("droplet %d still exists after deletion was requested", id)
}

// discover finds can-owned resources by tags when state has none.
func (p *DigitalOcean) discover(ctx context.Context, api *DOClient, c doConf, out io.Writer) (*State, error) {
	st := &State{Version: StateVersion, Provider: "digitalocean"}
	drops, err := api.ListDroplets(ctx, "", "company:"+c.slug)
	if err != nil {
		return nil, err
	}
	if d, found, err := ownedDroplet(drops, c); err != nil {
		return nil, err
	} else if found {
		st.Server = &ServerState{ID: d.ID, Name: d.Name, IP: d.PublicIPv4(), User: "root", Port: 22}
		fmt.Fprintf(out, "found droplet %d by name and tags\n", d.ID)
	}
	if st.Server == nil {
		// report (and skip) a same-named droplet that is not ours
		if named, err := api.ListDroplets(ctx, c.serverName, ""); err == nil && len(named) > 0 {
			fmt.Fprintf(out, "droplet %q exists but is not tagged as created by can; leaving it alone\n", c.serverName)
		}
	}
	fws, err := api.ListFirewalls(ctx)
	if err != nil {
		return nil, err
	}
	for _, fw := range fws {
		switch {
		case c.ownedFirewall(fw, serverID(st)):
			st.FirewallUUID = fw.ID
			fmt.Fprintf(out, "found firewall %s by name and tags\n", fw.ID)
		case fw.Name == c.fwName:
			fmt.Fprintf(out, "firewall %q exists but does not carry can's tags; leaving it alone\n", c.fwName)
		}
	}
	// An SSH key is never claimed without state: it stays in the account.
	return st, nil
}

// Status implements Provider.
func (p *DigitalOcean) Status(ctx context.Context, spec Spec, st *Store) (Status, error) {
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
	d, err := api.GetDroplet(ctx, state.Server.ID)
	if IsNotFound(err) {
		return Status{Phase: PhaseAbsent, Checks: []Check{{"digitalocean droplet", false, "recorded droplet no longer exists"}}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	check := Check{"digitalocean droplet", d.Status == "active", d.Status}
	if d.Status != "active" || d.PublicIPv4() == "" {
		return Status{Phase: PhaseProvisioning, Checks: []Check{check}}, nil
	}
	t := c.target
	t.Host = d.PublicIPv4()
	hs := HostStatus(ctx, p.deps.Runner(t))
	hs.Checks = append([]Check{check}, hs.Checks...)
	return hs, nil
}

// Address implements Provider.
func (*DigitalOcean) Address(st *Store) (string, error) {
	state, err := st.Load()
	if err != nil {
		return "", err
	}
	if state.Server == nil || state.Server.IP == "" {
		return "", fmt.Errorf("no server recorded yet; run `can up` first")
	}
	return state.Server.IP, nil
}
