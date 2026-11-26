package modmgr

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
)

// Wizard statuses.
const (
	WizardOpen      = "open"
	WizardCompleted = "completed"
	WizardDismissed = "dismissed"
)

// Where the relay credentials are written (the email policy's source Secret, docs/email-policy.md).
const (
	RelayNamespace  = "can-system"
	RelaySecretName = "can-smtp-relay"
)

// Bundle is a starter template: a named set of modules.
type Bundle struct {
	ID      string
	Modules []string
}

// DefaultBundles are the templates offered by the wizard.
var DefaultBundles = []Bundle{
	{ID: "starter", Modules: []string{"forms", "support"}},
	{ID: "blank"},
}

// Skip reasons of a bundle module.
const (
	SkipNotDeployable = "not-deployable"
	SkipUnknown       = "unknown"
)

// SkippedModule is a bundle member that cannot be enabled.
type SkippedModule struct{ ID, Reason string }

// RelayInfo is what the wizard remembers about the email relay: everything except the password.
type RelayInfo struct {
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Username   string `json:"username,omitempty"`
	From       string `json:"from,omitempty"`
	Configured bool   `json:"configured,omitempty"`
}

// WizardData is the wizard's saved answers.
type WizardData struct {
	Name        string    `json:"name"`
	Domain      string    `json:"domain"`
	Template    string    `json:"template,omitempty"`
	Modules     []string  `json:"modules"`
	Acks        []string  `json:"acks,omitempty"`
	ServerRAMMB int       `json:"server_ram_mb,omitempty"`
	Relay       RelayInfo `json:"relay"`
	KitAck      bool      `json:"kit_ack,omitempty"`
}

// WizardState is the stored wizard.
type WizardState struct {
	Status   string
	Data     WizardData
	Revision int64
	ApplyKey string
	ChangeID string
}

// Hardening is the admin-account step, derived from the audit log, not from a checkbox.
type Hardening struct{ PasswordRotated, TOTPRotated bool }

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func (s *Service) bundles() []Bundle {
	if s.Bundles != nil {
		return s.Bundles
	}
	return DefaultBundles
}

// BundleList returns the available templates.
func (s *Service) BundleList() []Bundle { return s.bundles() }

// BundleModules resolves a template to the modules that can be enabled and those that cannot.
func (s *Service) BundleModules(_ context.Context, id string) ([]string, []SkippedModule, error) {
	cat, err := s.Catalogue()
	if err != nil {
		return nil, nil, err
	}
	for _, b := range s.bundles() {
		if b.ID != id {
			continue
		}
		var sel []string
		var skipped []SkippedModule
		for _, m := range b.Modules {
			switch e := cat.Get(m); {
			case e == nil:
				skipped = append(skipped, SkippedModule{m, SkipUnknown})
			case !e.Deployable():
				skipped = append(skipped, SkippedModule{m, SkipNotDeployable})
			default:
				sel = append(sel, m)
			}
		}
		sort.Strings(sel)
		sort.Slice(skipped, func(i, j int) bool { return skipped[i].ID < skipped[j].ID })
		return sel, skipped, nil
	}
	return nil, nil, fmt.Errorf("unknown template %q", id)
}

// WizardState returns the wizard, seeding it from the stored company.yaml the first time.
func (s *Service) WizardState(ctx context.Context) (WizardState, error) {
	ws, err := s.loadWizard(ctx)
	if !errors.Is(err, ErrNotFound) {
		return ws, err
	}
	var d WizardData
	if cur, gerr := s.Company.Get(ctx); gerr == nil {
		if c, _ := decodeCompany("company.yaml", cur.Data); c != nil {
			d.Name, d.Domain, d.ServerRAMMB = c.Name, c.Domain, serverRAM(c)
			for id := range enabledSet(c) {
				d.Modules = append(d.Modules, id)
			}
			sort.Strings(d.Modules)
		}
	}
	b, _ := json.Marshal(d)
	// An installation that already runs modules is not a first login: do not send its owner to the wizard.
	status := WizardOpen
	if len(d.Modules) > 0 {
		status = WizardDismissed
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO wizard_state(id, status, data_json, revision, updated_at) VALUES (1, ?, ?, 0, ?)`, status, string(b), s.now().Unix()); err != nil {
		return WizardState{}, err
	}
	return s.loadWizard(ctx)
}

func (s *Service) loadWizard(ctx context.Context) (WizardState, error) {
	var ws WizardState
	var dj string
	err := s.DB.QueryRowContext(ctx, `SELECT status, data_json, revision, apply_key, change_id FROM wizard_state WHERE id=1`).
		Scan(&ws.Status, &dj, &ws.Revision, &ws.ApplyKey, &ws.ChangeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ws, ErrNotFound
	}
	if err != nil {
		return ws, err
	}
	_ = json.Unmarshal([]byte(dj), &ws.Data)
	return ws, nil
}

// WizardNeeded reports whether the first-login redirect should send the owner to the wizard.
func (s *Service) WizardNeeded(ctx context.Context) (bool, error) {
	ws, err := s.WizardState(ctx)
	if err != nil {
		return false, err
	}
	return ws.Status == WizardOpen, nil
}

func (s *Service) validateWizard(ctx context.Context, d *WizardData) error {
	if l := len([]rune(d.Name)); l < 1 || l > 80 || strings.ContainsAny(d.Name, "\r\n\x00") {
		return fmt.Errorf("%w: company name must be 1 to 80 characters", ErrInvalid)
	}
	if !domainRe.MatchString(d.Domain) {
		return fmt.Errorf("%w: domain is not a valid lowercase DNS name", ErrInvalid)
	}
	if d.ServerRAMMB != 0 && (d.ServerRAMMB < 256 || d.ServerRAMMB > 1<<20) {
		return fmt.Errorf("%w: server RAM must be between 256 and %d MB", ErrInvalid, 1<<20)
	}
	if d.Template != "" {
		ok := false
		for _, b := range s.bundles() {
			ok = ok || b.ID == d.Template
		}
		if !ok {
			return fmt.Errorf("%w: unknown template", ErrInvalid)
		}
	}
	cat, err := s.Catalogue()
	if err != nil {
		return err
	}
	already := map[string]bool{}
	if cur, err := s.Company.Get(ctx); err == nil {
		if c, _ := decodeCompany("company.yaml", cur.Data); c != nil {
			already = enabledSet(c)
		}
	}
	seen := map[string]bool{}
	for _, m := range d.Modules {
		e := cat.Get(m)
		if e == nil || seen[m] {
			return fmt.Errorf("%w: unknown or repeated module %q", ErrInvalid, Sanitise(m))
		}
		if !e.Deployable() && !already[m] {
			return fmt.Errorf("%w: module %q cannot be enabled", ErrNotDeployable, Sanitise(m))
		}
		seen[m] = true
	}
	sort.Strings(d.Modules)
	sort.Strings(d.Acks)
	if len(d.Acks) > 2*MaxModules {
		return fmt.Errorf("%w: too many acknowledgements", ErrInvalid)
	}
	return nil
}

// WizardUpdate applies fn to the saved answers, validates them and stores them. Saving answers that
// did not change leaves the revision alone, so every step is safe to submit twice.
func (s *Service) WizardUpdate(ctx context.Context, actor string, fn func(*WizardData) error) (WizardState, error) {
	ws, err := s.WizardState(ctx)
	if err != nil {
		return ws, err
	}
	if ws.Status != WizardOpen {
		return ws, fmt.Errorf("%w: the wizard is %s", ErrInvalid, ws.Status)
	}
	before, _ := json.Marshal(ws.Data)
	d := ws.Data
	d.Modules = append([]string(nil), d.Modules...)
	d.Acks = append([]string(nil), d.Acks...)
	if err := fn(&d); err != nil {
		return ws, err
	}
	if err := s.validateWizard(ctx, &d); err != nil {
		return ws, err
	}
	after, _ := json.Marshal(d)
	if string(before) == string(after) {
		return ws, nil
	}
	if _, err := s.audit(ctx, actor, "wizard.update", "wizard", map[string]any{"revision": ws.Revision + 1}); err != nil {
		return ws, err
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE wizard_state SET data_json=?, revision=revision+1, updated_at=?, apply_key='' WHERE id=1 AND revision=? AND status='open'`,
		string(after), s.now().Unix(), ws.Revision)
	if err != nil {
		return ws, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ws, ErrStale // another tab saved first
	}
	return s.loadWizard(ctx)
}

// WizardDismiss closes the wizard without applying anything.
func (s *Service) WizardDismiss(ctx context.Context, actor string) error {
	if _, err := s.WizardState(ctx); err != nil {
		return err
	}
	if _, err := s.audit(ctx, actor, "wizard.dismiss", "wizard", nil); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE wizard_state SET status='dismissed', updated_at=? WHERE id=1 AND status='open'`, s.now().Unix())
	return err
}

// wizardPatch turns the answers into the company.yaml edits they imply.
func (s *Service) wizardPatch(ctx context.Context, d WizardData) (Patch, error) {
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return Patch{}, err
	}
	c, ps := decodeCompany("company.yaml", cur.Data)
	if c == nil {
		return Patch{}, fmt.Errorf("%w: %s", ErrInvalid, firstProblem(ps))
	}
	var p Patch
	if d.Name != c.Name {
		p.Ops = append(p.Ops, SetString("name", d.Name))
	}
	if d.Domain != c.Domain {
		p.Ops = append(p.Ops, SetString("domain", d.Domain))
	}
	if d.ServerRAMMB != 0 && d.ServerRAMMB != serverRAM(c) {
		p.Ops = append(p.Ops, Op{Path: []string{"provider", "options", "server_ram_mb"}, Value: int64(d.ServerRAMMB)})
	}
	want, have := map[string]bool{}, enabledSet(c)
	for _, m := range d.Modules {
		want[m] = true
	}
	var ids []string
	for id := range want {
		ids = append(ids, id)
	}
	for id := range have {
		if !want[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if want[id] != have[id] {
			p.Ops = append(p.Ops, SetEnabled(id, want[id]))
		}
	}
	return p, nil
}

// WizardPreview shows what applying the answers would do, without storing anything.
func (s *Service) WizardPreview(ctx context.Context) (*Preview, error) {
	ws, err := s.WizardState(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.wizardPatch(ctx, ws.Data)
	if err != nil {
		return nil, err
	}
	return s.PreviewPatch(ctx, p, ws.Data.Acks)
}

// WizardApply proposes and applies the answers. It is idempotent: once applied, calling it again
// returns the same run and does no further work; an identical retry after a failure reuses the same
// proposal.
func (s *Service) WizardApply(ctx context.Context, actor string) (Run, error) {
	ws, err := s.WizardState(ctx)
	if err != nil {
		return Run{}, err
	}
	if ws.Status == WizardCompleted && ws.ChangeID != "" {
		ch, err := s.GetChange(ctx, ws.ChangeID)
		if err != nil {
			return Run{}, err
		}
		return s.Run(ctx, ch.RunID)
	}
	if ws.Status != WizardOpen {
		return Run{}, fmt.Errorf("%w: the wizard is %s", ErrInvalid, ws.Status)
	}
	patch, err := s.wizardPatch(ctx, ws.Data)
	if err != nil {
		return Run{}, err
	}
	if len(patch.Ops) == 0 {
		if _, err := s.audit(ctx, actor, "wizard.complete", "wizard", map[string]any{"change": ""}); err != nil {
			return Run{}, err
		}
		_, _ = s.DB.ExecContext(ctx, `UPDATE wizard_state SET status='completed', updated_at=? WHERE id=1`, s.now().Unix())
		return Run{}, ErrNoChange
	}
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return Run{}, err
	}
	pj, _ := patch.Marshal()
	h := sha256.Sum256([]byte(string(pj) + "\x00" + cur.Version + "\x00" + strings.Join(ws.Data.Acks, ",")))
	key := hex.EncodeToString(h[:8])
	changeID := ws.ChangeID
	if ws.ApplyKey != key || changeID == "" {
		ch, err := s.Propose(ctx, actor, Proposal{Patch: patch, Acks: ws.Data.Acks, Summary: "onboarding wizard"})
		if err != nil {
			return Run{}, err
		}
		changeID = ch.ID
		if _, err := s.DB.ExecContext(ctx, `UPDATE wizard_state SET apply_key=?, change_id=? WHERE id=1`, key, changeID); err != nil {
			return Run{}, err
		}
	}
	run, err := s.Apply(ctx, actor, changeID)
	if err != nil {
		return Run{}, err
	}
	if _, err := s.audit(ctx, actor, "wizard.complete", "wizard", map[string]any{"change": changeID, "run": run.ID}); err != nil {
		return run, err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE wizard_state SET status='completed', updated_at=? WHERE id=1`, s.now().Unix())
	return run, err
}

// RelayInput is the email relay form. The password is written to a Secret and nowhere else.
type RelayInput struct {
	Host, Username, Password, From string
	Port                           int
}

var hostRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)

// SetRelay validates the relay settings, writes them to the Secret can-system/can-smtp-relay and
// remembers everything except the password in the wizard state.
func (s *Service) SetRelay(ctx context.Context, actor string, in RelayInput) error {
	switch {
	case s.Secrets == nil:
		return fmt.Errorf("%w: secrets cannot be written in this installation", ErrInvalid)
	case !hostRe.MatchString(in.Host) && net.ParseIP(in.Host) == nil:
		return fmt.Errorf("%w: relay host must be a host name or address", ErrInvalid)
	case in.Port < 1 || in.Port > 65535:
		return fmt.Errorf("%w: relay port must be 1 to 65535", ErrInvalid)
	case in.Password == "" || len(in.Password) > 1024 || strings.ContainsAny(in.Password, "\x00"):
		return fmt.Errorf("%w: relay password is required", ErrInvalid)
	case in.Username == "" || len(in.Username) > 256 || strings.ContainsAny(in.Username, "\r\n\x00"):
		// email.send refuses a password without a user name (and the other way round).
		return fmt.Errorf("%w: relay username is required", ErrInvalid)
	}
	if !config.ValidAddress(in.From) {
		return fmt.Errorf("%w: from must be an email address", ErrInvalid)
	}
	ws, err := s.WizardState(ctx)
	if err != nil {
		return err
	}
	if ws.Status != WizardOpen {
		return fmt.Errorf("%w: setup is %s, so the relay Secret is not changed from here", ErrInvalid, ws.Status)
	}
	if _, err := s.audit(ctx, actor, "email.relay.set", in.Host, map[string]any{"port": in.Port}); err != nil {
		return err
	}
	if err := s.Secrets.Put(ctx, RelayNamespace, RelaySecretName, map[string]string{
		"host": in.Host, "port": strconv.Itoa(in.Port), "username": in.Username, "password": in.Password, "from": in.From,
	}); err != nil {
		_, _ = s.audit(ctx, actor, "email.relay.failed", in.Host, nil)
		return fmt.Errorf("writing the relay Secret failed: %s", Sanitise(err.Error()))
	}
	_, err = s.WizardUpdate(ctx, actor, func(d *WizardData) error {
		d.Relay = RelayInfo{Host: in.Host, Port: in.Port, Username: in.Username, From: in.From, Configured: true}
		return nil
	})
	return err
}

// DNSRecord is a record the operator creates at their DNS provider.
type DNSRecord struct{ Name, Type, Value, Purpose string }

// DNSPlan is the DNS step.
type DNSPlan struct {
	Records    []DNSRecord
	NoAddress  bool   // the server's public address is not known
	InstantURL string // https://<ip-dashed>.sslip.io, usable before DNS exists
	Instant    string // InstantServing, InstantUnreachable or InstantUnknown
}

// DNS lists the records to create for the stored company.yaml and checks the instant URL.
func (s *Service) DNS(ctx context.Context) (DNSPlan, error) {
	var p DNSPlan
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return p, err
	}
	c, ps := decodeCompany("company.yaml", cur.Data)
	if c == nil {
		return p, fmt.Errorf("%w: %s", ErrInvalid, firstProblem(ps))
	}
	var v4, v6 []string
	if s.Net != nil {
		if v4, v6, err = s.Net.PublicIPs(ctx); err != nil {
			v4, v6 = nil, nil
		}
	}
	if len(v4) == 0 && len(v6) == 0 {
		p.NoAddress, p.Instant = true, InstantUnknown
		return p, nil
	}
	hosts := []struct{ name, purpose string }{{c.Domain, "site"}, {"admin." + c.Domain, "admin"}}
	if cat, err := s.Catalogue(); err == nil {
		for _, id := range sortedSelected(enabledSet(c), cat) {
			if sd := cat.Get(id).Manifest.Runtime.Subdomain; sd != "" {
				hosts = append(hosts, struct{ name, purpose string }{sd + "." + c.Domain, id})
			}
		}
	}
	for _, h := range hosts {
		for _, ip := range v4 {
			p.Records = append(p.Records, DNSRecord{Name: h.name, Type: "A", Value: ip, Purpose: h.purpose})
		}
		for _, ip := range v6 {
			p.Records = append(p.Records, DNSRecord{Name: h.name, Type: "AAAA", Value: ip, Purpose: h.purpose})
		}
	}
	p.Instant = InstantUnknown
	if len(v4) > 0 {
		p.InstantURL = "https://" + instantHost(v4[0])
		if s.Instant != nil {
			p.Instant = s.Instant.Probe(ctx, p.InstantURL)
		}
	}
	return p, nil
}

// AccountHardening reports whether the user has rotated the make-up password and authentication
// secret, from the audit log.
func (s *Service) AccountHardening(ctx context.Context, username string) (Hardening, error) {
	var h Hardening
	q := func(prefix string) (bool, error) {
		rs, err := s.Audit.Query(ctx, audit.Filter{Actor: username, ActionPrefix: prefix, Limit: 1})
		return len(rs) > 0, err
	}
	var err error
	if h.PasswordRotated, err = q("admin.password.change"); err != nil {
		return h, err
	}
	h.TOTPRotated, err = q("admin.totp.rotate")
	return h, err
}
