package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// QA minor 10: sweep, verify-audit and rotate-keys run next to a live server and must not clear
// its staging or temporary files.
func TestMaintenanceCommandsLeaveUploadsInFlightAlone(t *testing.T) {
	dir := t.TempDir()
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "keyring"), testKeyring)
	cfg := filepath.Join(dir, "files.yaml")
	write(cfg, "db_path: "+filepath.Join(dir, "f.db")+"\nkeyring_path: "+filepath.Join(dir, "keyring")+"\nstaging_dir: "+filepath.Join(dir, "stage")+
		"\nbackend: { local: { dir: "+filepath.Join(dir, "blobs")+" } }\n")
	inflight := []string{filepath.Join(dir, "stage", "up-live"), filepath.Join(dir, "blobs", ".tmp", "put-live")}
	for _, p := range inflight {
		write(p, "partial")
	}
	env := func(k string) string {
		return map[string]string{"CAN_FILES_API_TOKEN": "0123456789abcdefghijklmnopqrstuvwxyz", "CAN_FILES_LINK_SECRET": "zyxwvutsrqponmlkjihgfedcba9876543210", "CAN_FILES_CONFIG": cfg}[k]
	}
	for _, cmd := range []string{"sweep", "verify-audit", "rotate-keys"} {
		var o, e bytes.Buffer
		if c := Main([]string{cmd}, env, &o, &e); c != 0 {
			t.Fatalf("%s: %d %s", cmd, c, e.String())
		}
		for _, p := range inflight {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s removed %s", cmd, p)
			}
		}
	}
}

// QA minor 11 and 12: no row limit on an erasure, and held files cannot starve the sweep.
func TestPurgesAreNotLimitedOrStarvedByHeldFiles(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "S")
	insert := func(n int, subject string, hold int, status string) {
		t.Helper()
		old := e.now().AddDate(0, 0, -400).Unix()
		for i := 0; i < n; i++ {
			if _, err := e.s.db.Exec(`INSERT INTO files (id, space_id, ext, subject, status, current_version, legal_hold, created_by, created_at, updated_at, deleted_at)
				VALUES (?,?, 'pdf', ?, ?, 1, ?, 'user:alice', ?, ?, ?)`, newID("fil"), sp.ID, subject, status, hold, old, old, old+int64(hold^1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert(2*purgeBatch+100, "", 1, "deleted") // held, and older than anything else
	insert(3, "", 0, "deleted")
	rep, err := e.s.RunRetention(ctx)
	if err != nil || rep.Purged != 3 || rep.SkippedHold != 2*purgeBatch+100 {
		t.Fatalf("held files starved the sweep: %+v %v", rep, err)
	}
	insert(5100, "pers_many", 0, "active")
	er, err := e.s.EraseSubject(ctx, e.root, "pers_many")
	if err != nil || er.Files != 5100 {
		t.Fatalf("erasure: %+v %v", er, err)
	}
}

// QA minor 13: a version stored while scanning was optional must not be served once it is required.
func TestUnscannedVersionsAreNotServedWhenScanningIsRequired(t *testing.T) {
	e := newEnv(t, func(_ *Config, o *StoreOptions) { o.Scanner = NoopScanner{} })
	sp := e.space(e.alice, "S")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf(""))
	tok, _, err := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	e.s.cfg.Scanner.Required = &yes
	if _, err := e.s.OpenContent(ctx, e.alice, f.ID, 0); !errors.Is(err, ErrConflict) {
		t.Errorf("download of an unscanned version = %v", err)
	}
	if _, err := e.s.OpenShare(ctx, tok, "1.1.1.1"); err == nil {
		t.Error("a share served an unscanned version")
	}
}

type gateScanner struct{ in, release chan struct{} }

func (g gateScanner) Scan(_ context.Context, r io.Reader) (ScanVerdict, error) {
	_, _ = io.Copy(io.Discard, r)
	g.in <- struct{}{}
	<-g.release
	return ScanClean, nil
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// QA minor 15: concurrent uploads are bounded, and a full quota refuses before staging anything.
func TestUploadsAreBoundedAndQuotaIsCheckedBeforeStaging(t *testing.T) {
	g := gateScanner{in: make(chan struct{}), release: make(chan struct{})}
	e := newEnv(t, func(c *Config, o *StoreOptions) { c.Limits.MaxConcurrentUploads = 1; o.Scanner = g })
	sp := e.space(e.alice, "S")
	first := make(chan error, 1)
	go func() {
		_, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.pdf", Body: bytes.NewReader(pdf("1"))})
		first <- err
	}()
	<-g.in
	body := &countingReader{r: bytes.NewReader(pdf("2"))}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := e.s.Upload(wctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "b.pdf", Body: body}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a second concurrent upload = %v, want unavailable", err)
	}
	if body.n != 0 || e.staged() != 1 {
		t.Errorf("the refused upload was read (%d bytes) or staged (%d files)", body.n, e.staged())
	}
	close(g.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}

	q := newEnv(t, func(c *Config, _ *StoreOptions) {
		c.Limits.MaxFileBytes, c.Limits.QuotaBytesPerOwner = 1000, 1000
	})
	sp = q.space(q.alice, "S")
	q.upload(q.alice, sp.ID, "a.txt", []byte(strings.Repeat("a", 900)))
	for name, size := range map[string]int64{"declared over what is left": 500, "unknown length but the quota is full": -1} {
		if size < 0 {
			q.upload(q.alice, sp.ID, "fill.txt", []byte(strings.Repeat("a", 100)))
		}
		body := &countingReader{r: strings.NewReader(strings.Repeat("b", 500))}
		_, err := q.s.Upload(ctx, q.alice, UploadRequest{SpaceID: sp.ID, Name: "b.txt", Body: body, Size: size})
		if !errors.Is(err, ErrQuota) || body.n != 0 {
			t.Errorf("%s: %v after reading %d bytes", name, err, body.n)
		}
	}
}
