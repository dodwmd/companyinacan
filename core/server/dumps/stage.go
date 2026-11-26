package dumps

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// run is the state of the dump run in progress. Uploads go into its staging directory; the
// published modules/ directory is only replaced, as a whole, when every hook has succeeded.
type run struct {
	id   string
	dir  string // <dumps>/.run-<id>: tmp/ (in-flight uploads) and modules/ (accepted ones)
	turn *turn  // the module whose hook is running; nil between hooks
	// keep stops closeRun from removing the staging directory: it holds the only copy of the
	// previous set (a failed swap that could not be rolled back).
	keep bool
}

// turn is one module's hook: the only time that module's uploads are accepted.
type turn struct {
	module   string
	files    map[string]bool // accepted names
	inflight int
	received int64
}

func (d *Dumper) startTurn(module string) *turn {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := &turn{module: module, files: map[string]bool{}}
	d.cur.turn = t
	return t
}

// endTurn closes the module's window and returns how many files it uploaded. An upload still in
// flight can no longer be committed: commit checks the turn under the same lock.
func (d *Dumper) endTurn() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur == nil || d.cur.turn == nil {
		return 0
	}
	n := len(d.cur.turn.files)
	d.cur.turn = nil
	return n
}

// received counts the bytes of one upload against the module's total for the run.
type received struct {
	d *Dumper
	t *turn
	r io.Reader
}

func (c received) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.d.mu.Lock()
	c.t.received += int64(n)
	over := c.t.received > c.d.Store.maxModuleBytes()
	c.d.mu.Unlock()
	if over {
		return n, ErrTooLarge
	}
	return n, err
}

// Accept stores an upload from the module whose backup hook is running right now. Everything
// else is refused: between runs, an upload would change data under a valid marker.
func (d *Dumper) Accept(module, name string, r io.Reader) error {
	if err := validName(module, name); err != nil {
		return err
	}
	d.mu.Lock()
	cur := d.cur
	if cur == nil || cur.turn == nil || cur.turn.module != module {
		d.mu.Unlock()
		return ErrNoRun
	}
	t := cur.turn
	if !t.files[name] && len(t.files)+t.inflight >= d.Store.maxFiles() {
		d.mu.Unlock()
		return ErrTooMany
	}
	t.inflight++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		t.inflight--
		d.mu.Unlock()
	}()

	tmp, err := writeTemp(filepath.Join(cur.dir, "tmp"), received{d, t, r}, d.Store.limit())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoRun // the run ended and took its staging directory with it
		}
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Re-checked after the transfer: the hook may have ended, or another run may have begun.
	if d.cur != cur || cur.turn != t {
		_ = os.Remove(tmp)
		return ErrNoRun
	}
	dir := filepath.Join(cur.dir, "modules", module)
	if err := plainDir(dir); err != nil {
		return err
	}
	if err := replace(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	t.files[name] = true
	return nil
}

// openRun creates the staging directory of a new run.
func (d *Dumper) openRun(id string) error {
	dir := filepath.Join(d.Store.Dir, ".run-"+id)
	for _, p := range []string{dir, filepath.Join(dir, "tmp"), filepath.Join(dir, "modules")} {
		if err := plainDir(p); err != nil {
			return err
		}
	}
	d.mu.Lock()
	d.cur = &run{id: id, dir: dir}
	d.mu.Unlock()
	return nil
}

// publish swaps the staged module dumps in for the previous set. If the new set cannot be moved
// in, the previous one is put back.
func (d *Dumper) publish() error {
	d.mu.Lock()
	cur := d.cur
	d.mu.Unlock()
	live, old := filepath.Join(d.Store.Dir, "modules"), filepath.Join(cur.dir, "old")
	if err := d.move(live, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := d.move(filepath.Join(cur.dir, "modules"), live); err != nil {
		if rerr := d.move(old, live); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			d.mu.Lock()
			cur.keep = true
			d.mu.Unlock()
			return errors.Join(err, fmt.Errorf("and the previous set could not be put back (kept in %s, restored by the next run): %w", old, rerr))
		}
		return err
	}
	return nil
}

func (d *Dumper) move(from, to string) error {
	if d.rename != nil {
		return d.rename(from, to)
	}
	return os.Rename(from, to)
}

// recoverSwap puts the previous set back if a run died between the two renames of publish. It
// must run before leftovers are cleaned.
func (d *Dumper) recoverSwap() error {
	live := filepath.Join(d.Store.Dir, "modules")
	if _, err := os.Lstat(live); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	olds, err := filepath.Glob(filepath.Join(d.Store.Dir, ".run-*", "old"))
	if err != nil || len(olds) == 0 {
		return err
	}
	sort.Strings(olds) // run ids start with the time: the last is the newest
	d.log().Warn("recovering the previous module dumps from an interrupted run", "from", olds[len(olds)-1])
	return d.move(olds[len(olds)-1], live)
}

// published reports whether the current set holds a dump of module.
func (d *Dumper) published(module string) bool {
	ents, err := os.ReadDir(filepath.Join(d.Store.Dir, "modules", module))
	return err == nil && len(ents) > 0
}

// carryForward puts the module's published files into the staged set, in place of anything its
// hook uploaded in this run. Hard links (not copies, not symlinks) leave the current set whole
// until the swap.
func (d *Dumper) carryForward(module string) (bool, error) {
	d.mu.Lock()
	dir := d.cur.dir
	d.mu.Unlock()
	dst := filepath.Join(dir, "modules", module)
	if err := os.RemoveAll(dst); err != nil {
		return false, err
	}
	if !d.published(module) {
		return false, nil
	}
	src := filepath.Join(d.Store.Dir, "modules", module)
	ents, err := os.ReadDir(src)
	if err != nil {
		return false, err
	}
	if err := plainDir(dst); err != nil {
		return false, err
	}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		if err := os.Link(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return false, err
		}
	}
	return true, nil
}

// closeRun discards the staging directory.
func (d *Dumper) closeRun() {
	d.mu.Lock()
	cur := d.cur
	d.cur = nil
	d.mu.Unlock()
	if cur != nil && !cur.keep {
		if err := os.RemoveAll(cur.dir); err != nil {
			d.log().Warn("could not remove a dump staging directory", "path", cur.dir, "error", err)
		}
	}
}

// Forget deletes a purged module's dumps, published and staged. Older restic snapshots keep them
// until retention expires them.
func (d *Dumper) Forget(module string) error {
	if !moduleRe.MatchString(module) {
		return ErrBadName
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dirs := []string{filepath.Join(d.Store.Dir, "modules", module)}
	if d.cur != nil {
		dirs = append(dirs, filepath.Join(d.cur.dir, "modules", module), filepath.Join(d.cur.dir, "old", module))
	}
	var errs []error
	for _, p := range dirs {
		errs = append(errs, os.RemoveAll(p))
	}
	return errors.Join(errs...)
}
