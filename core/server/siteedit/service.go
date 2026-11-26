package siteedit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/site"
)

// Limits.
const (
	maxDrafts    = 20
	maxRevisions = 50
	// DraftTTL is how long a previewed change may be published.
	DraftTTL = 24 * time.Hour
)

// Kinds of drafts and revisions.
const (
	KindPublish  = "publish"
	KindRestore  = "restore"
	KindExternal = "external" // found in company.yaml, not published from here
)

// Errors.
var (
	ErrStale    = errors.New("the website content changed after this change was prepared")
	ErrNotFound = errors.New("not found")
	ErrExpired  = errors.New("this change has expired")
	ErrTooBig   = errors.New("the built site is too large to publish")
	// ErrNotLive means company.yaml was saved but the public site could not be replaced.
	ErrNotLive = errors.New("saved, but the public site could not be updated")
)

// InvalidError carries what is wrong with a change.
type InvalidError struct{ Fields []FieldError }

func (e *InvalidError) Error() string {
	var s []string
	for _, f := range e.Fields {
		s = append(s, strings.TrimPrefix(f.Field+": "+f.Message, ": "))
	}
	return "the change is not valid: " + strings.Join(s, "; ")
}

// AuditLog is what the service records to (audit.Log satisfies it).
type AuditLog interface {
	Append(ctx context.Context, e audit.Entry) (audit.Record, error)
}

// Service is the website editor.
type Service struct {
	DB         *store.DB
	Audit      AuditLog
	Company    modmgr.CompanyStore
	Publisher  Publisher
	ModulesDir string
	// BaseURL is the public origin used when the published site does not record one.
	BaseURL string
	Now     func() time.Time

	mu sync.Mutex // one publish at a time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Live states of the public site compared with company.yaml.
const (
	LiveCurrent  = "current"
	LiveOutdated = "outdated"
	LiveUnknown  = "unknown"
	LiveNone     = "none"
)

// State is the saved website content.
type State struct {
	Company *config.Company
	// Hash identifies the `site:` section; a change is prepared from one and refused on another.
	Hash string
	Live string
}

type current struct {
	snap modmgr.Snapshot
	c    *config.Company
	hash string
}

func (s *Service) current(ctx context.Context) (current, error) {
	snap, err := s.Company.Get(ctx)
	if err != nil {
		return current{}, err
	}
	c, err := modmgr.ParseCompany(snap.Data)
	if err != nil {
		return current{}, err
	}
	return current{snap: snap, c: c, hash: site.SectionHash(c.Site)}, nil
}

// State reads the saved content and says whether the public site was built from it.
func (s *Service) State(ctx context.Context) (State, error) {
	cur, err := s.current(ctx)
	if err != nil {
		return State{}, err
	}
	st := State{Company: cur.c, Hash: cur.hash, Live: LiveUnknown}
	p, err := s.Publisher.Current(ctx)
	switch {
	case errors.Is(err, ErrNoSite):
		st.Live = LiveNone
	case err != nil:
		// The editor stays usable; publishing will say why it cannot work.
	case p.SourceSHA256 == cur.hash:
		st.Live = LiveCurrent
	case p.SourceSHA256 != "":
		st.Live = LiveOutdated
	}
	return st, nil
}

// Draft is a previewed change.
type Draft struct {
	ID, Actor, Kind, BaseHash, State string
	CreatedAt                        time.Time
	Site                             config.Site
}

// published is Current, with an installation that has no site yet being an empty one.
func (s *Service) published(ctx context.Context) (Published, error) {
	p, err := s.Publisher.Current(ctx)
	if errors.Is(err, ErrNoSite) {
		return Published{}, nil
	}
	return p, err
}

// prepare puts next into the saved document and checks the result. The logo is a file of the
// company repository: the editor never changes it.
func (s *Service) prepare(ctx context.Context, cur current, next config.Site, p Published) (doc []byte, c *config.Company, out *site.Output, err error) {
	next.Brand.Logo = cur.c.Site.Brand.Logo
	if doc, err = Merge(cur.snap.Data, next); err != nil {
		return nil, nil, nil, err
	}
	if len(doc) > modmgr.MaxCompanyBytes {
		return nil, nil, nil, &InvalidError{Fields: []FieldError{{Message: fmt.Sprintf("company.yaml would be larger than %d KiB", modmgr.MaxCompanyBytes>>10)}}}
	}
	c, out, fe := s.check(doc, p)
	if len(fe) > 0 {
		return nil, nil, nil, &InvalidError{Fields: fe}
	}
	return doc, c, out, nil
}

// CreateDraft validates a change prepared from the content with hash base and stores it.
func (s *Service) CreateDraft(ctx context.Context, actor, base string, next config.Site, kind string) (Draft, error) {
	cur, err := s.current(ctx)
	if err != nil {
		return Draft{}, err
	}
	p, err := s.published(ctx)
	if err != nil {
		return Draft{}, err
	}
	// Invalid input is reported even when the content moved meanwhile, so that it is not lost.
	_, c, _, err := s.prepare(ctx, cur, next, p)
	if err != nil {
		return Draft{}, err
	}
	if base != cur.hash {
		return Draft{}, ErrStale
	}
	b, err := json.Marshal(c.Site)
	if err != nil {
		return Draft{}, err
	}
	d := Draft{ID: newID(), Actor: actor, Kind: kind, BaseHash: cur.hash, State: "open", CreatedAt: s.now().UTC(), Site: c.Site}
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO site_drafts(id, actor, created_at, kind, base_hash, site_json, state) VALUES (?,?,?,?,?,?,?)`,
			d.ID, d.Actor, d.CreatedAt.Unix(), d.Kind, d.BaseHash, string(b), d.State); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM site_drafts WHERE id NOT IN (SELECT id FROM site_drafts ORDER BY created_at DESC, rowid DESC LIMIT ?)`, maxDrafts)
		return err
	})
	return d, err
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// GetDraft returns a stored draft.
func (s *Service) GetDraft(ctx context.Context, id string) (Draft, error) {
	d := Draft{ID: id}
	var at int64
	var js string
	err := s.DB.QueryRowContext(ctx, `SELECT actor, created_at, kind, base_hash, site_json, state FROM site_drafts WHERE id=?`, id).
		Scan(&d.Actor, &at, &d.Kind, &d.BaseHash, &js, &d.State)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}
	d.CreatedAt = time.Unix(at, 0).UTC()
	return d, json.Unmarshal([]byte(js), &d.Site)
}

// Review is a draft against the saved content as it is now.
type Review struct {
	Draft Draft
	// Stale: the content changed since the draft was prepared; it cannot be published.
	Stale, Expired bool
	Diff           []modmgr.DiffLine
	DiffTooLarge   bool
	Pages          []string // slugs of the extra pages
	Warnings       []string
	Invalid        []FieldError
}

// Review prepares the page that shows a draft before it is published.
func (s *Service) Review(ctx context.Context, id string) (Review, error) {
	d, err := s.GetDraft(ctx, id)
	if err != nil {
		return Review{}, err
	}
	cur, err := s.current(ctx)
	if err != nil {
		return Review{}, err
	}
	p, err := s.published(ctx)
	if err != nil {
		return Review{}, err
	}
	r := Review{Draft: d, Expired: s.now().Sub(d.CreatedAt) > DraftTTL}
	doc, c, out, err := s.prepare(ctx, cur, d.Site, p)
	var inv *InvalidError
	switch {
	case errors.As(err, &inv):
		r.Invalid = inv.Fields
		r.Stale = d.BaseHash != cur.hash
		return r, nil
	case err != nil:
		return Review{}, err
	}
	r.Stale = d.State == "open" && d.BaseHash != cur.hash && site.SectionHash(c.Site) != cur.hash
	r.Warnings = out.Warnings
	for _, pg := range c.Site.Pages {
		r.Pages = append(r.Pages, pg.Slug)
	}
	if r.DiffTooLarge = modmgr.DiffTooLarge(string(cur.snap.Data), string(doc)); !r.DiffTooLarge {
		r.Diff = modmgr.Diff(string(cur.snap.Data), string(doc))
	}
	return r, nil
}

// Preview renders one page of a draft as a self-contained document (site.Output.Preview).
func (s *Service) Preview(ctx context.Context, id, page string) ([]byte, string, error) {
	d, err := s.GetDraft(ctx, id)
	if err != nil {
		return nil, "", err
	}
	cur, err := s.current(ctx)
	if err != nil {
		return nil, "", err
	}
	p, err := s.published(ctx)
	if err != nil {
		return nil, "", err
	}
	_, _, out, err := s.prepare(ctx, cur, d.Site, p)
	if err != nil {
		return nil, "", err
	}
	doc, hash, err := out.Preview(page)
	if errors.Is(err, site.ErrNoPage) {
		return nil, "", ErrNotFound
	}
	return doc, hash, err
}

// Publish saves a draft to company.yaml and replaces the public site.
//
// Neither side silently overwrites the other: the draft is refused when the `site:` section is no
// longer the one it was prepared from, and company.yaml is written only if nobody wrote it since it
// was read here (changes to other sections are therefore kept).
func (s *Service) Publish(ctx context.Context, actor, id string) (warnings []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.GetDraft(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.now().Sub(d.CreatedAt) > DraftTTL {
		return nil, ErrExpired
	}
	if d.State != "open" {
		return nil, nil // a repeated click
	}
	if err := s.Publisher.Ready(ctx); err != nil {
		return nil, err
	}
	p, err := s.Publisher.Current(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	doc, c, out, err := s.prepare(ctx, cur, d.Site, p)
	if err != nil {
		return nil, err
	}
	next := site.SectionHash(c.Site)
	// Equal to the saved content already: an earlier attempt saved it and could not publish it.
	if cur.hash != d.BaseHash && cur.hash != next {
		return nil, ErrStale
	}
	archive, _, err := out.Archive()
	if err != nil {
		return nil, err
	}
	if len(archive) > site.MaxArchive {
		return nil, ErrTooBig
	}
	sum := sha256.Sum256(archive)
	base := p.BaseURL
	if base == "" {
		base = s.BaseURL
	}
	rel := Release{Output: out, Archive: archive, SHA256: hex.EncodeToString(sum[:]), BaseURL: base, SourceSHA256: next}

	// On record before anything is changed.
	if err := s.record(ctx, actor, "site.publish", id, map[string]any{"kind": d.Kind, "from": cur.hash, "to": next, "archive": rel.SHA256, "sections": changedSections(cur.c.Site, c.Site)}); err != nil {
		return nil, err
	}
	// A browser that gives up does not stop a publish half way.
	ctx = context.WithoutCancel(ctx)
	if cur.hash != next {
		// What is about to be replaced stays in the history, wherever it came from.
		if err := s.remember(ctx, "company.yaml", KindExternal, cur.c.Site); err != nil {
			return nil, err
		}
		if _, err := s.Company.Put(ctx, doc, cur.snap.Version); err != nil {
			_ = s.record(ctx, actor, "site.publish_failed", id, map[string]any{"to": next})
			if errors.Is(err, modmgr.ErrConflict) {
				return nil, ErrStale
			}
			return nil, err
		}
	}
	if err := s.Publisher.Publish(ctx, p, rel); err != nil {
		_ = s.record(ctx, actor, "site.publish_failed", id, map[string]any{"to": next})
		return nil, fmt.Errorf("%w: %w", ErrNotLive, err)
	}
	if err := s.remember(ctx, actor, d.Kind, c.Site); err != nil {
		return nil, err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE site_drafts SET state='published' WHERE id=?`, id); err != nil {
		return nil, err
	}
	return out.Warnings, nil
}

func (s *Service) record(ctx context.Context, actor, action, target string, detail map[string]any) error {
	_, err := s.Audit.Append(ctx, audit.Entry{Actor: actor, Action: action, Target: target, Detail: detail})
	return err
}

// changedSections names the keys of `site:` that differ, for the audit log: never the content.
func changedSections(a, b config.Site) []string {
	fields := func(s config.Site) map[string]any {
		m := map[string]any{}
		j, _ := json.Marshal(s)
		_ = json.Unmarshal(j, &m)
		return m
	}
	x, y := fields(a), fields(b)
	out := []string{}
	for _, k := range []string{"Tagline", "Description", "Lang", "OGImage", "NoIndex", "SameAs", "Brand", "Hero", "Features", "Pricing", "FAQ", "CTA", "Contact", "Support", "Careers", "Pages", "Footer"} {
		p, _ := json.Marshal(x[k])
		q, _ := json.Marshal(y[k])
		if string(p) != string(q) {
			out = append(out, strings.ToLower(k))
		}
	}
	return out
}

// Revision is one entry of the history.
type Revision struct {
	ID          int64
	CreatedAt   time.Time
	Actor, Kind string
	// Current: it is what company.yaml holds now.
	Current bool
}

// remember adds the content to the history unless it is what the history already ends with.
func (s *Service) remember(ctx context.Context, actor, kind string, content config.Site) error {
	hash := site.SectionHash(content)
	b, err := json.Marshal(content)
	if err != nil {
		return err
	}
	return s.DB.InTx(ctx, func(tx *sql.Tx) error {
		var last string
		err := tx.QueryRowContext(ctx, `SELECT hash FROM site_revisions ORDER BY id DESC LIMIT 1`).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if last == hash {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO site_revisions(created_at, actor, kind, hash, site_json) VALUES (?,?,?,?,?)`, s.now().Unix(), actor, kind, hash, string(b)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM site_revisions WHERE id NOT IN (SELECT id FROM site_revisions ORDER BY id DESC LIMIT ?)`, maxRevisions)
		return err
	})
}

// History lists the revisions, newest first.
func (s *Service) History(ctx context.Context) ([]Revision, error) {
	cur, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, created_at, actor, kind, hash FROM site_revisions ORDER BY id DESC LIMIT ?`, maxRevisions)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Revision
	for rows.Next() {
		var r Revision
		var at int64
		var hash string
		if err := rows.Scan(&r.ID, &at, &r.Actor, &r.Kind, &hash); err != nil {
			return nil, err
		}
		r.CreatedAt, r.Current = time.Unix(at, 0).UTC(), hash == cur.hash
		out = append(out, r)
	}
	return out, rows.Err()
}

// Restore prepares a draft that brings a revision back; it is previewed and published like any other.
func (s *Service) Restore(ctx context.Context, actor string, rev int64) (Draft, error) {
	var js string
	err := s.DB.QueryRowContext(ctx, `SELECT site_json FROM site_revisions WHERE id=?`, rev).Scan(&js)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}
	var old config.Site
	if err := json.Unmarshal([]byte(js), &old); err != nil {
		return Draft{}, err
	}
	cur, err := s.current(ctx)
	if err != nil {
		return Draft{}, err
	}
	return s.CreateDraft(ctx, actor, cur.hash, old, KindRestore)
}
