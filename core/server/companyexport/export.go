// Package companyexport is can-core's side of `can export` (specs/071-company-export): one
// tar.gz of the company's data in open formats, written straight to whoever asked for it. can-core
// writes its own part, then runs each module's export Job; the Job's upload (PUT /v1/exports) is
// copied entry by entry into the archive as it arrives. Nothing of the export is stored on the
// server.
package companyexport

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the driver of the state database

	"github.com/dodwmd/companyinacan/core/exportfmt"
	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Runner runs one module's export Job; the Job sends its stream to Accept. dumps.KubeHooks
// implements it.
type Runner interface {
	RunExport(ctx context.Context, module string, includeSealed bool) error
}

// Errors.
var (
	// ErrBusy: an export or a backup dump run is in progress.
	ErrBusy = errors.New("an export or a backup dump is already running; try again when it has finished")
	// ErrBadRequest wraps a request that names no such module.
	ErrBadRequest = errors.New("export: bad request")
	// ErrNoTurn refuses an upload that no running export asked for.
	ErrNoTurn = errors.New("export: no export of this module is running")
)

const (
	maxEntries      = 1_000_000
	maxModuleName   = 200
	maxModuleReport = 1 << 20
	// moduleTimeout is the export Job's deadline and a margin.
	moduleTimeout = 61 * time.Minute
)

// Service produces exports, one at a time.
type Service struct {
	DBPath      string
	CompanyFile string
	Company     string // the company's name
	Version     string // can-core's
	Audit       *audit.Log
	Modules     func(ctx context.Context) ([]store.Module, error)
	// Hooks runs export Jobs; nil (no cluster) exports can-core's own data only and fails when
	// modules are installed.
	Hooks Runner
	// Site returns the archive of the published website; nil or an error leaves it out.
	Site func(ctx context.Context) ([]byte, error)
	// NeverInstalled reports a module whose first install failed: it never holds data.
	NeverInstalled func(ctx context.Context, module string) bool
	// DumpRunning reports a backup dump run, during which no export starts.
	DumpRunning func() bool
	Now         func() time.Time
	Log         *slog.Logger

	mu     sync.Mutex
	active bool
	cur    *turn
}

// Options of one export.
type Options struct {
	// IncludeSealed decrypts the sealed fields into the export.
	IncludeSealed bool
	// Only restricts the export to these modules ("core" is can-core's own part); Skip leaves
	// modules out.
	Only, Skip []string
	// Actor is who asked, for the audit log.
	Actor string
}

// CorePart names can-core's own part in Options.
const CorePart = "core"

// ModuleStatus values of the manifest.
const (
	StatusExported    = "exported"
	StatusNoData      = "no-data"
	StatusNotExported = "not-exported"
	StatusSkipped     = "skipped"
)

// ModuleReport is one module in the manifest.
type ModuleReport struct {
	ID      string                  `json:"id"`
	Version string                  `json:"version"`
	State   string                  `json:"state"`
	Status  string                  `json:"status"`
	Reason  string                  `json:"reason,omitempty"`
	Export  *modbackup.ModuleExport `json:"export,omitempty"`
}

// Manifest is manifest.json, the last entry of an archive.
type Manifest struct {
	Format        int       `json:"format"`
	Complete      bool      `json:"complete"`
	Company       string    `json:"company"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by"`
	CanCore       string    `json:"can_core_version"`
	IncludeSealed bool      `json:"sealed_fields_decrypted"`
	// Partial: the export was restricted to some modules, or left some out on request.
	Partial  bool           `json:"partial"`
	Modules  []ModuleReport `json:"modules"`
	Warnings []string       `json:"warnings,omitempty"`
}

type turn struct {
	module   string
	archive  *exportfmt.Archive
	closed   bool
	used     bool
	inflight sync.WaitGroup
	report   *modbackup.ModuleExport
	err      error
	// broken: the archive holds part of this module and cannot be completed.
	broken bool
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Request is a checked export that has not written anything yet.
type Request struct {
	s       *Service
	o       Options
	modules []store.Module
	core    bool
}

// Prepare checks a request, claims the single export slot and records it in the audit log. Write
// must follow, or Abort.
func (s *Service) Prepare(ctx context.Context, o Options) (*Request, error) {
	if o.Actor == "" {
		return nil, errors.New("export: no actor")
	}
	s.mu.Lock()
	if s.active || (s.DumpRunning != nil && s.DumpRunning()) {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.active = true
	s.mu.Unlock()
	r, err := s.prepare(ctx, o)
	if err != nil {
		s.release()
		return nil, err
	}
	return r, nil
}

func (s *Service) prepare(ctx context.Context, o Options) (*Request, error) {
	all, err := s.Modules(ctx)
	if err != nil {
		return nil, fmt.Errorf("list modules: %w", err)
	}
	known := map[string]bool{CorePart: true}
	for _, m := range all {
		known[m.ID] = true
	}
	for _, id := range append(append([]string{}, o.Only...), o.Skip...) {
		if !known[id] {
			return nil, fmt.Errorf("%w: no module %q is installed", ErrBadRequest, id)
		}
	}
	wanted := func(id string) bool {
		return (len(o.Only) == 0 || slices.Contains(o.Only, id)) && !slices.Contains(o.Skip, id)
	}
	r := &Request{s: s, o: o, core: wanted(CorePart)}
	var ids []string
	for _, m := range all {
		if wanted(m.ID) {
			r.modules = append(r.modules, m)
			ids = append(ids, m.ID)
		}
	}
	sort.Slice(r.modules, func(i, j int) bool { return r.modules[i].ID < r.modules[j].ID })
	sort.Strings(ids)
	if r.core {
		ids = append([]string{CorePart}, ids...)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: nothing is left to export", ErrBadRequest)
	}
	// On record before any data leaves.
	if _, err := s.Audit.Append(ctx, audit.Entry{Actor: o.Actor, Action: "company.export", Target: "company",
		Detail: map[string]any{"include_sealed": o.IncludeSealed, "parts": ids}}); err != nil {
		return nil, fmt.Errorf("the export cannot be recorded in the audit log, so it is refused: %w", err)
	}
	return r, nil
}

func (s *Service) release() {
	s.mu.Lock()
	s.active, s.cur = false, nil
	s.mu.Unlock()
}

// Abort gives up a prepared export.
func (r *Request) Abort() { r.s.release() }

// Write streams the archive to w. On an error the archive is left without its end and its
// manifest, so that nobody can take it for complete.
func (r *Request) Write(ctx context.Context, w io.Writer) (m Manifest, err error) {
	s := r.s
	defer s.release()
	defer func() {
		action, detail := "company.export.completed", map[string]any{"include_sealed": r.o.IncludeSealed}
		if err != nil {
			action, detail["error"] = "company.export.failed", "see the can-core log"
			s.log().Error("export failed", "error", err)
		} else {
			status := map[string]any{}
			for _, mod := range m.Modules {
				status[mod.ID] = mod.Status
			}
			detail["modules"] = status
		}
		if _, aerr := s.Audit.Append(context.WithoutCancel(ctx), audit.Entry{Actor: r.o.Actor, Action: action, Target: "company", Detail: detail}); aerr != nil {
			s.log().Error("the end of an export could not be recorded", "error", aerr)
		}
	}()
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return m, err
	}
	now := s.now().UTC()
	a := exportfmt.NewArchive(zw, now)
	m = Manifest{Format: exportfmt.FormatVersion, Complete: true, Company: s.Company, CreatedAt: now, CreatedBy: r.o.Actor, CanCore: s.Version,
		IncludeSealed: r.o.IncludeSealed, Partial: len(r.o.Only) > 0 || len(r.o.Skip) > 0, Modules: []ModuleReport{}}
	if err := a.Bytes("README.txt", []byte(readme(r.o.IncludeSealed))); err != nil {
		return m, err
	}
	if r.core {
		warnings, err := s.writeCore(ctx, a)
		if err != nil {
			return m, fmt.Errorf("can-core's data: %w", err)
		}
		m.Warnings = append(m.Warnings, warnings...)
	}
	for _, mod := range r.modules {
		rep, err := s.module(ctx, a, mod, r.o.IncludeSealed)
		if err != nil {
			return m, fmt.Errorf("module %s: %w (export without it with --skip %s)", mod.ID, err, mod.ID)
		}
		switch rep.Status {
		case StatusNotExported:
			m.Warnings = append(m.Warnings, fmt.Sprintf("module %s is NOT in this export: %s", mod.ID, rep.Reason))
		case StatusExported:
			if rep.Export != nil {
				for _, warn := range rep.Export.Warnings {
					m.Warnings = append(m.Warnings, "module "+mod.ID+": "+warn)
				}
			}
		}
		m.Modules = append(m.Modules, rep)
		if err := zw.Flush(); err != nil {
			return m, err
		}
	}
	if err := a.JSON(exportfmt.ManifestName, m); err != nil {
		return m, err
	}
	if err := a.Close(); err != nil {
		return m, err
	}
	return m, zw.Close()
}

// module runs one module's export and waits for its upload to be written.
func (s *Service) module(ctx context.Context, a *exportfmt.Archive, mod store.Module, sealed bool) (ModuleReport, error) {
	rep := ModuleReport{ID: mod.ID, Version: mod.Version, State: mod.State, Status: StatusExported}
	if s.NeverInstalled != nil && s.NeverInstalled(ctx, mod.ID) {
		rep.Status, rep.Reason = StatusSkipped, "its first install never succeeded, so it holds no data"
		return rep, nil
	}
	if s.Hooks == nil {
		return rep, errors.New("this can-core cannot run Jobs")
	}
	t := &turn{module: mod.ID, archive: a}
	s.mu.Lock()
	s.cur = t
	s.mu.Unlock()
	hctx, cancel := context.WithTimeout(ctx, moduleTimeout)
	err := s.Hooks.RunExport(hctx, mod.ID, sealed)
	cancel()
	s.mu.Lock()
	t.closed = true
	s.mu.Unlock()
	// An upload still running is writing into the archive: nothing else may until it is done.
	t.inflight.Wait()
	s.mu.Lock()
	used, uerr, report, broken := t.used, t.err, t.report, t.broken
	s.cur = nil
	s.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return rep, ctx.Err()
	case broken:
		return rep, errors.Join(errors.New("its upload broke off"), uerr, err)
	case report != nil: // complete, whatever became of the Job afterwards
		rep.Export = report
		return rep, nil
	case errors.Is(err, dumps.ErrNoData):
		rep.Status, rep.Reason = StatusNoData, "it has no database or volume"
		return rep, nil
	case errors.Is(err, dumps.ErrNoExport):
		rep.Status, rep.Reason = StatusNotExported, err.Error()
		return rep, nil
	case err != nil:
		return rep, errors.Join(err, uerr)
	case !used:
		return rep, errors.New("the export Job succeeded but sent nothing")
	default:
		return rep, errors.Join(errors.New("the export Job succeeded but its upload was not accepted"), uerr)
	}
}

// Accept copies the export stream of the module whose export Job is running into the archive,
// under modules/<id>/. Everything else is refused. The module is not trusted with more than its
// own directory: entry names are checked and only regular files are taken.
func (s *Service) Accept(module string, r io.Reader) error {
	s.mu.Lock()
	t := s.cur
	if t == nil || t.module != module || t.closed || t.used {
		s.mu.Unlock()
		return ErrNoTurn
	}
	t.used = true
	t.inflight.Add(1)
	s.mu.Unlock()
	defer t.inflight.Done()

	report, wrote, err := copyModule(t.archive, module, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		t.report, t.err = report, nil
	case !wrote: // nothing in the archive yet: the Job may try again
		t.used, t.err = false, err
	default:
		t.broken, t.err = true, err
	}
	return err
}

func copyModule(a *exportfmt.Archive, module string, r io.Reader) (report *modbackup.ModuleExport, wrote bool, err error) {
	tr := tar.NewReader(r)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, wrote, fmt.Errorf("read the upload: %w", err)
		}
		switch {
		case report != nil:
			return nil, wrote, errors.New("the upload holds entries after its manifest")
		case n >= maxEntries:
			return nil, wrote, errors.New("the upload holds too many entries")
		case h.Typeflag != tar.TypeReg || len(h.Name) > maxModuleName || !exportfmt.ValidName(h.Name):
			return nil, wrote, errors.New("the upload holds an unacceptable entry")
		}
		name := "modules/" + module + "/" + h.Name
		wrote = true
		if h.Name == exportfmt.ManifestName {
			b, err := io.ReadAll(io.LimitReader(tr, maxModuleReport+1))
			if err != nil {
				return nil, wrote, err
			}
			var m modbackup.ModuleExport
			if len(b) > maxModuleReport || json.Unmarshal(b, &m) != nil || m.Format != exportfmt.FormatVersion {
				return nil, wrote, errors.New("the upload's manifest cannot be read")
			}
			if err := a.Bytes(name, b); err != nil {
				return nil, wrote, err
			}
			report = &m
			continue
		}
		if _, err := a.Copy(name, h.Size, tr); err != nil {
			return nil, wrote, err
		}
	}
	if report == nil {
		return nil, wrote, errors.New("the upload ended without its manifest")
	}
	_, err = io.Copy(io.Discard, io.LimitReader(r, 1<<20)) // the tar padding
	return report, wrote, err
}

// stateDB opens a read-only connection of its own on can-core's database: the application's pool
// has a single connection, which a long download must not occupy.
func (s *Service) stateDB() (*sql.DB, error) {
	if _, err := os.Stat(s.DBPath); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+s.DBPath+"?mode=ro&_pragma=busy_timeout(30000)")
}

func readme(sealed bool) string {
	b := &strings.Builder{}
	b.WriteString(`Company in a Can: export of your company's data. Format 1 (docs/export.md).

manifest.json            what is in here, and what is not (read its "warnings"). It is the last
                         entry: an archive without it is incomplete.
company/company.yaml     the company's configuration
company/records/         the control plane: accounts, roles, approvals and the audit log
company/schema.json      the columns of those tables
site/published.tar.gz    the website as it is published
modules/<id>/records/    each table as <table>.jsonl (exact values) and <table>.csv
modules/<id>/schema.json columns, types, and what was left out and why
modules/<id>/files/      the files the records refer to
modules/<id>/native/     a wrapped application's own SQL dump
modules/<id>/manifest.json

Times ending in _at are Unix seconds (UTC) unless text. Binary values are base64.
In the CSV files a cell that a spreadsheet would run as a formula starts with an apostrophe;
the .jsonl file of the same table holds the exact value.
Passwords, tokens, keys and other credentials are never exported. Erased data is not here.
`)
	if sealed {
		b.WriteString("\nWARNING: this export holds the SEALED FIELDS IN CLEAR (salaries, identity numbers, bank account\nnumbers, board materials and the like). Keep it encrypted and delete it when you are done.\n")
	} else {
		b.WriteString("\nEncrypted fields show as \"" + exportfmt.SealedMarker + "\". `can export --include-sealed` decrypts them.\n")
	}
	return b.String()
}
