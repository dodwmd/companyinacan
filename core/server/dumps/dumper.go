package dumps

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the driver the state copy is checked with

	"github.com/dodwmd/companyinacan/core/backup"
)

// HookRunner runs one module's backup hook. The hook uploads its dump through the dump API
// (Dumper.Accept); a non-nil error means the module's dump is missing or unusable.
type HookRunner interface {
	RunBackup(ctx context.Context, module string) error
}

// Dumper produces one consistent set of dumps per run and then, last, the completion marker.
type Dumper struct {
	Store *Store
	// DBPath is can-core's state database, copied with VACUUM INTO over a read-only connection of
	// its own: the application's pool has a single connection, which a copy must not occupy.
	DBPath string
	Hooks  HookRunner
	// Modules lists the enabled modules that have a backup hook.
	Modules func(ctx context.Context) ([]string, error)
	// Database names a module's requires.database, recorded in the DumpedList.
	Database func(ctx context.Context, module string) string
	// Disabled reports whether a module is disabled; Unrunnable why its hook is known not to run
	// ("" if it can). A disabled module never stops backups: it is skipped and reported.
	Disabled   func(ctx context.Context, module string) bool
	Unrunnable func(ctx context.Context, module string) string
	// CompanyFile is the applied company.yaml, copied into every run ("" skips).
	CompanyFile string
	Now         func() time.Time // default time.Now
	Log         *slog.Logger
	// RestoreMode allows modules to download their dumps (restore hooks). can-core has no restore
	// mode yet, so nothing sets it and every download is refused.
	RestoreMode bool
	// HookTimeout bounds one module's hook (default 15 minutes).
	HookTimeout time.Duration

	mu     sync.Mutex
	active bool
	cur    *run
	// syncPath flushes a file or directory to disk (default fsync); tests observe it.
	syncPath func(string) error
	// rename is os.Rename; tests fail it.
	rename func(from, to string) error
}

// ErrNoData is returned by a HookRunner when a module has nothing to dump.
var ErrNoData = errors.New("dump: the module has no data")

// DumpedList is the file naming the modules dumped in the run ("<id> <database>" lines). The
// backup script leaves raw SQLite files out only for the modules listed here.
const DumpedList = backup.DumpedList

// Result describes a finished run.
type Result struct {
	RunID    string
	Finished time.Time
	Files    int
	Duration time.Duration
}

func (d *Dumper) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dumper) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *Dumper) begin() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active {
		return false
	}
	d.active = true
	return true
}

func (d *Dumper) end() {
	d.mu.Lock()
	d.active = false
	d.mu.Unlock()
}

// Active reports whether a run is in progress.
func (d *Dumper) Active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}

func newRunID(t time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// Run performs one dump run. It removes the marker, copies can-core's state and company.yaml in
// place (so a failed run has already replaced those two), then runs each module's backup hook
// into a staging directory. Only if every enabled or failed module with data uploaded is the new
// set swapped in and the marker written; otherwise the previous module dumps stay, without a
// marker, so the host script backs up nothing. A disabled module never fails the run: if its hook
// cannot run or fails it is skipped, reported, and its raw volume files stay in the backup.
func (d *Dumper) Run(ctx context.Context) (res Result, rerr error) {
	if !d.begin() {
		return Result{}, ErrBusy
	}
	defer d.end()
	start := d.now()
	runID := newRunID(start)
	defer func() {
		if rerr == nil {
			return // recorded before the marker, with the set it describes
		}
		if err := backup.WriteDumpRun(d.Store.Dir, backup.DumpRun{RunID: runID, At: d.now(), Error: rerr.Error()}); err != nil {
			d.log().Error("could not record the failed dump run", "error", err)
		}
	}()
	if err := plainDir(d.Store.Dir); err != nil {
		return Result{}, err
	}
	marker := filepath.Join(d.Store.Dir, COMPLETE)
	for _, p := range []string{marker, filepath.Join(d.Store.Dir, DumpedList), filepath.Join(d.Store.Dir, backup.AlertsName)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("remove the completion marker: %w", err)
		}
	}
	var mods []string
	if d.Modules != nil {
		var err error
		if mods, err = d.Modules(ctx); err != nil {
			return Result{}, fmt.Errorf("list modules: %w", err)
		}
	}
	if err := d.recoverSwap(); err != nil {
		return Result{}, fmt.Errorf("recover the previous dumps: %w", err)
	}
	if err := d.Store.clean(mods); err != nil {
		return Result{}, fmt.Errorf("remove stale dumps: %w", err)
	}
	if err := d.openRun(runID); err != nil {
		return Result{}, err
	}
	defer d.closeRun()
	if err := d.dumpState(ctx); err != nil {
		return Result{}, fmt.Errorf("can-core state: %w", err)
	}
	if err := d.dumpCompany(); err != nil {
		return Result{}, fmt.Errorf("company.yaml: %w", err)
	}
	var failed []error
	var dumped strings.Builder
	if len(mods) > 0 && d.Hooks == nil {
		return Result{}, fmt.Errorf("modules %v need their backup hooks run, but this can-core cannot run Jobs", mods)
	}
	var skipped []backup.SkippedModule
	database := func(m string) string {
		if d.Database == nil {
			return "unknown"
		}
		return d.Database(ctx, m)
	}
	skip := func(m, reason string, noData bool) error {
		kept, err := d.carryForward(m)
		if err != nil {
			return fmt.Errorf("keep the earlier dump of %s: %w", m, err)
		}
		s := backup.SkippedModule{ID: m, Reason: reason, Database: database(m), NoData: noData, Kept: kept}
		d.log().Warn("module not dumped", "module", m, "detail", s.Message())
		skipped = append(skipped, s)
		return nil
	}
	for _, m := range mods {
		disabled := d.Disabled != nil && d.Disabled(ctx, m)
		if disabled && d.Unrunnable != nil {
			if why := d.Unrunnable(ctx, m); why != "" {
				if err := skip(m, "it is disabled and its backup hook cannot run ("+why+")", false); err != nil {
					return Result{}, err
				}
				continue
			}
		}
		d.startTurn(m)
		hctx, cancel := context.WithTimeout(ctx, d.hookTimeout())
		err := d.Hooks.RunBackup(hctx, m)
		cancel()
		n := d.endTurn()
		if errors.Is(err, ErrNoData) {
			// Nothing to dump is fine for a module that never had a dump; one that had one has lost
			// its volume or database, which must not pass silently.
			if d.published(m) {
				if err := skip(m, "it reports no data although it had a dump (volume or database gone?)", true); err != nil {
					return Result{}, err
				}
			} else {
				d.log().Info("module has no data to dump", "module", m)
			}
			continue
		}
		if err == nil && n == 0 {
			err = errors.New("the backup hook succeeded but uploaded nothing")
		}
		if err != nil && disabled && ctx.Err() == nil {
			if serr := skip(m, "it is disabled and its backup hook failed: "+err.Error(), false); serr != nil {
				return Result{}, serr
			}
			continue
		}
		if err != nil {
			d.log().Error("module backup hook failed", "module", m, "error", err)
			failed = append(failed, fmt.Errorf("module %s: %w", m, err))
			continue
		}
		fmt.Fprintf(&dumped, "%s %s\n", m, database(m))
	}
	if err := errors.Join(failed...); err != nil {
		return Result{}, fmt.Errorf("dump run %s incomplete, no completion marker written: %w", runID, err)
	}
	if err := d.publish(); err != nil {
		return Result{}, fmt.Errorf("publish the dumps: %w", err)
	}
	if err := d.Store.writeAtomic(d.Store.Dir, DumpedList, strings.NewReader(dumped.String())); err != nil {
		return Result{}, fmt.Errorf("write the list of dumped modules: %w", err)
	}
	record := backup.DumpRun{RunID: runID, At: d.now(), OK: true, Skipped: skipped}
	if alerts := record.Alerts(); len(alerts) > 0 {
		if err := d.Store.writeAtomic(d.Store.Dir, backup.AlertsName, strings.NewReader(strings.Join(alerts, "\n")+"\n")); err != nil {
			return Result{}, fmt.Errorf("write the alerts: %w", err)
		}
	}
	if err := backup.WriteDumpRun(d.Store.Dir, record); err != nil {
		return Result{}, fmt.Errorf("record the run: %w", err)
	}
	// The marker claims the set is complete: it must not reach the disk before the set does.
	if err := d.syncAll(); err != nil {
		return Result{}, fmt.Errorf("flush the dumps to disk: %w", err)
	}
	done := d.now()
	if err := backup.WriteMarker(marker, backup.Marker{RunID: runID, FinishedAt: done}); err != nil {
		return Result{}, fmt.Errorf("write the completion marker: %w", err)
	}
	return Result{RunID: runID, Finished: done, Files: d.Store.count(), Duration: time.Since(start)}, nil
}

func (d *Dumper) hookTimeout() time.Duration {
	if d.HookTimeout > 0 {
		return d.HookTimeout
	}
	return 15 * time.Minute
}

// dumpCompany copies the applied company.yaml into the dumps, where `can restore` expects it.
func (d *Dumper) dumpCompany() error {
	dst := filepath.Join(d.Store.Dir, "company.yaml")
	if d.CompanyFile == "" {
		return nil
	}
	f, err := os.Open(d.CompanyFile)
	if errors.Is(err, os.ErrNotExist) {
		if rerr := os.Remove(dst); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return rerr
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return d.Store.writeAtomic(d.Store.Dir, "company.yaml", f)
}

// dumpState writes a consistent copy of can-core's SQLite database (VACUUM INTO) and checks it.
func (d *Dumper) dumpState(ctx context.Context) error {
	dir := filepath.Join(d.Store.Dir, "can-core")
	if err := plainDir(dir); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".state.db.tmp")
	_ = os.Remove(tmp)
	src, err := sql.Open("sqlite", "file:"+d.DBPath+"?mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return err
	}
	_, err = src.ExecContext(ctx, `VACUUM INTO ?`, tmp)
	_ = src.Close()
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	cp, err := sql.Open("sqlite", "file:"+tmp+"?mode=ro")
	if err != nil {
		return err
	}
	var res string
	err = cp.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res)
	_ = cp.Close()
	if err != nil {
		return err
	}
	if res != "ok" {
		_ = os.Remove(tmp)
		return fmt.Errorf("integrity check of the copy failed: %s", res)
	}
	for _, ext := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Remove(tmp + ext)
	}
	return os.Rename(tmp, filepath.Join(dir, "state.db"))
}

// fsync flushes one file or directory.
func fsync(p string) error {
	f, err := os.Open(p) //nolint:gosec // inside the dumps directory
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncAll flushes every published file, then every directory holding one, deepest first.
func (d *Dumper) syncAll() error {
	syncPath := d.syncPath
	if syncPath == nil {
		syncPath = fsync
	}
	var dirs []string
	err := filepath.WalkDir(d.Store.Dir, func(p string, e os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p != d.Store.Dir && strings.HasPrefix(e.Name(), "."):
			if e.IsDir() {
				return filepath.SkipDir
			}
		case e.IsDir():
			dirs = append(dirs, p)
		case e.Type().IsRegular():
			return syncPath(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncPath(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// NeedsRun reports whether the marker is missing, unreadable or older than maxAge.
func (d *Dumper) NeedsRun(maxAge time.Duration) bool {
	m, err := backup.ReadMarker(filepath.Join(d.Store.Dir, COMPLETE))
	if err != nil {
		return true
	}
	return d.now().Sub(m.FinishedAt) > maxAge
}

// NextRun is the next time at hour:min UTC strictly after now.
func NextRun(now time.Time, hour, min int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Request asks the running can-core for an immediate dump run by creating path. It is the
// trigger behind `can-core dump-request` (kubectl exec), which needs no new API surface.
func Request(path string) error {
	return os.WriteFile(path, []byte("dump requested\n"), 0o600)
}

// Requested consumes a pending request: it reports true once per Request.
func Requested(path string) bool {
	return os.Remove(path) == nil
}

// Loop runs the nightly schedule until ctx ends: at hour:min UTC, at start-up when no fresh
// marker exists (after startDelay, so the module reconcile can settle), and whenever the request
// file appears.
func (d *Dumper) Loop(ctx context.Context, hour, min int, startDelay time.Duration, requestPath string) {
	catchUp := time.NewTimer(startDelay)
	defer catchUp.Stop()
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	next := time.NewTimer(time.Until(NextRun(d.now(), hour, min)))
	defer next.Stop()
	run := func(why string) {
		res, err := d.Run(ctx)
		switch {
		case errors.Is(err, ErrBusy):
		case err != nil:
			d.log().Error("dump run failed", "reason", why, "error", err)
		default:
			d.log().Info("dump run complete", "reason", why, "run_id", res.RunID, "files", res.Files, "seconds", res.Duration.Seconds())
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-catchUp.C:
			if d.NeedsRun(20 * time.Hour) {
				run("no fresh completion marker")
			}
		case <-next.C:
			run("schedule")
			next.Reset(time.Until(NextRun(d.now(), hour, min)))
		case <-poll.C:
			if requestPath != "" && Requested(requestPath) {
				run("request")
			}
		}
	}
}

// Open opens a stored module dump (the restore hook's GET); only in restore mode.
func (d *Dumper) Open(module, name string) (*os.File, error) {
	if !d.RestoreMode {
		return nil, ErrNotRestoring
	}
	return d.Store.Open(module, name)
}

// BeginForTest opens a run with module's hook window open, without running anything; other
// packages' tests use it to exercise Accept. Production code never calls it.
func (d *Dumper) BeginForTest(module string) error {
	d.begin()
	if err := plainDir(d.Store.Dir); err != nil {
		return err
	}
	if err := d.openRun("test"); err != nil {
		return err
	}
	d.startTurn(module)
	return nil
}
