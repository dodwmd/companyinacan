package files

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// One blob that cannot be deleted must not stop the rest of the retention work.
func TestAnUndeletableBlobDoesNotStopRetention(t *testing.T) {
	e, h := hookEnv(t)
	sp := e.space(e.alice, "Docs")
	var ids []string
	for _, n := range []string{"a.pdf", "b.pdf", "c.pdf"} {
		f := e.upload(e.alice, sp.ID, n, pdf(n))
		if err := e.s.DeleteFile(ctx, e.alice, f.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
	}
	var stuckKey string
	if err := e.s.db.QueryRow(`SELECT blob_key FROM versions WHERE file_id = ?`, ids[0]).Scan(&stuckKey); err != nil {
		t.Fatal(err)
	}
	// two orphan blobs, one of them undeletable too
	for _, k := range []string{"stuck-orphan", "good-orphan"} {
		if _, err := e.s.db.Exec(`INSERT INTO pending_blobs (blob_key, created_at) VALUES (?,?)`, k, ts(e.now())); err != nil {
			t.Fatal(err)
		}
	}
	h.failKey = func(k string) bool { return k == stuckKey || k == "stuck-orphan" }
	e.advance(40 * 24 * time.Hour)
	for pass, want := range []RetentionReport{{Purged: 2, Orphans: 1}, {}} {
		rep, err := e.s.RunRetention(ctx)
		if err == nil || !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), ids[0]) || !strings.Contains(err.Error(), "stuck-orphan") {
			t.Fatalf("pass %d: both failures must be reported: %v", pass, err)
		}
		if rep.Purged != want.Purged || rep.Orphans != want.Orphans {
			t.Fatalf("pass %d: %+v, want %+v", pass, rep, want)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM files WHERE status = 'purged'`); n != 2 {
		t.Fatalf("%d purged", n)
	}
	// The storage recovers: the next sweep finishes what was left.
	h.failKey = nil
	rep, err := e.s.RunRetention(ctx)
	if err != nil || rep.Purged != 1 || rep.Orphans != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestDeletingAFileRevokesItsLinksForGood(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf("a"))
	other := e.upload(e.alice, sp.ID, "b.pdf", pdf("b"))
	tok, sh, err := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	keep, _, err := e.s.CreateShare(ctx, e.alice, other.ID, ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteFile(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM shares WHERE id = ? AND revoked_at IS NOT NULL`, sh.ID); n != 1 {
		t.Fatal("the link was not revoked when the file was deleted")
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'share.revoke' AND resource = ?`, "file:"+f.ID); n != 1 {
		t.Fatalf("%d audit entries for the revocation", n)
	}
	if err := e.s.RestoreFile(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.OpenShare(ctx, tok, "203.0.113.1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restoring the file revived its link: %v", err)
	}
	c, err := e.s.OpenShare(ctx, keep, "203.0.113.1")
	if err != nil {
		t.Fatalf("another file's link: %v", err)
	}
	_ = c.Body.Close()
}

func TestOneSubjectCannotTakeEveryUploadSlot(t *testing.T) {
	e := newEnv(t)
	var held []func()
	for i := 0; i < e.s.cfg.Limits.MaxConcurrentUploadsPerSubject; i++ {
		release, err := e.s.uploadSlot(e.alice.Subject)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	sp := e.space(e.alice, "Docs")
	if _, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.pdf", Body: bytes.NewReader(pdf("a"))}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("over the per-subject cap: %v", err)
	}
	bobs := e.space(e.bob, "Bob")
	e.upload(e.bob, bobs.ID, "b.pdf", pdf("b")) // somebody else still has a slot
	held[0]()
	e.upload(e.alice, sp.ID, "a.pdf", pdf("a"))

	for yaml, want := range map[string]int{"": 2, "limits: {max_concurrent_uploads: 1}": 1, "limits: {max_concurrent_uploads: 9}": 4} {
		c, err := ParseConfig([]byte(yaml))
		if err != nil || c.Limits.MaxConcurrentUploadsPerSubject != want {
			t.Errorf("%q: %v", yaml, err)
		}
	}
	if _, err := ParseConfig([]byte("limits: {max_concurrent_uploads: 4, max_concurrent_uploads_per_subject: 4}")); err == nil {
		t.Error("a per-subject cap that is not below the global one was accepted")
	}
}

func startServer(t *testing.T, api *API, mod func(*http.Server)) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(api.Handler())
	srv.Config = newServer(api.Handler())
	mod(srv.Config)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// slowBody delivers its bytes after a pause longer than the server's ReadTimeout.
type slowBody struct {
	r     io.Reader
	pause time.Duration
}

func (s *slowBody) Read(p []byte) (int, error) {
	time.Sleep(s.pause)
	s.pause = 0
	return s.r.Read(p)
}

func TestTheServerBoundsReadsButUploadsKeepTheirOwnDeadline(t *testing.T) {
	e := newEnv(t, func(_ *Config, o *StoreOptions) { o.Scanner = slowScanner{400 * time.Millisecond} })
	api := &API{Store: e.s, Auth: StaticToken{Token: apiToken}}
	if got := newServer(api.Handler()).ReadTimeout; got != 30*time.Second {
		t.Fatalf("ReadTimeout %v", got)
	}
	srv := startServer(t, api, func(s *http.Server) { s.ReadTimeout = 200 * time.Millisecond })
	svc := &Principal{Subject: "svc:static", Perms: []string{PermAdmin}}
	sp := e.space(svc, "S")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/spaces/"+sp.ID+"/files?name=slow.pdf", &slowBody{r: bytes.NewReader(pdf("y")), pause: 500 * time.Millisecond})
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a slow upload within its own deadline: %d", resp.StatusCode)
	}
}
