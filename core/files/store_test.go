package files

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

// ---- access control -----------------------------------------------------------------------------

func TestInvisibleIsIndistinguishableFromMissing(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Payroll")
	f := e.upload(e.alice, sp.ID, "salaries.pdf", pdf("secret"))
	missingSpace, missingFile := "spc_"+strings.Repeat("0", 24), "fil_"+strings.Repeat("0", 24)
	type probe struct {
		name string
		fn   func(id string) error
	}
	probes := []probe{
		{"space", func(id string) error { _, err := e.s.GetSpace(ctx, e.bob, id); return err }},
		{"files of space", func(id string) error { _, err := e.s.ListFiles(ctx, e.bob, ListQuery{SpaceID: id}); return err }},
		{"upload", func(id string) error {
			_, err := e.s.Upload(ctx, e.bob, UploadRequest{SpaceID: id, Name: "x.pdf", Body: bytes.NewReader(pdf(""))})
			return err
		}},
		{"members", func(id string) error { _, err := e.s.ListMembers(ctx, e.bob, id); return err }},
		{"folders", func(id string) error { _, err := e.s.CreateFolder(ctx, e.bob, id, "", "f"); return err }},
	}
	for _, p := range probes {
		a, b := p.fn(sp.ID), p.fn(missingSpace)
		if !errors.Is(a, ErrNotFound) || !errors.Is(b, ErrNotFound) || a.Error() != b.Error() {
			t.Errorf("%s: invisible %v, missing %v: must be the same not-found", p.name, a, b)
		}
	}
	fprobes := []probe{
		{"file", func(id string) error { _, err := e.s.GetFile(ctx, e.bob, id); return err }},
		{"versions", func(id string) error { _, err := e.s.ListVersions(ctx, e.bob, id); return err }},
		{"content", func(id string) error { _, err := e.s.OpenContent(ctx, e.bob, id, 0); return err }},
		{"delete", func(id string) error { return e.s.DeleteFile(ctx, e.bob, id) }},
		{"share", func(id string) error { _, _, err := e.s.CreateShare(ctx, e.bob, id, ShareRequest{}); return err }},
		{"patch", func(id string) error {
			n := "n.pdf"
			_, err := e.s.UpdateFile(ctx, e.bob, id, FilePatch{Name: &n})
			return err
		}},
	}
	for _, p := range fprobes {
		a, b := p.fn(f.ID), p.fn(missingFile)
		if !errors.Is(a, ErrNotFound) || !errors.Is(b, ErrNotFound) || a.Error() != b.Error() {
			t.Errorf("%s: invisible %v, missing %v", p.name, a, b)
		}
	}
	// bob's lists are empty, not an error
	if pg, err := e.s.ListSpaces(ctx, e.bob, "", 0); err != nil || len(pg.Items) != 0 {
		t.Errorf("bob's spaces: %v %v", pg, err)
	}
	if pg, err := e.s.ListFiles(ctx, e.bob, ListQuery{}); err != nil || len(pg.Items) != 0 {
		t.Errorf("bob's files: %v %v", pg, err)
	}
}

func TestRolesGateActionsInsideASpace(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Team")
	f := e.upload(e.alice, sp.ID, "plan.pdf", pdf("v1"))
	e.member(e.alice, sp.ID, "user", "user:bob", "viewer")
	e.member(e.alice, sp.ID, "user", "user:carol", "editor")

	if _, err := e.s.GetFile(ctx, e.bob, f.ID); err != nil {
		t.Errorf("viewer read: %v", err)
	}
	if c, err := e.s.OpenContent(ctx, e.bob, f.ID, 0); err != nil {
		t.Errorf("viewer download: %v", err)
	} else {
		_ = c.Body.Close()
	}
	_, err := e.s.Upload(ctx, e.bob, UploadRequest{SpaceID: sp.ID, Name: "b.pdf", Body: bytes.NewReader(pdf(""))})
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer upload = %v, want forbidden", err)
	}
	if err := e.s.DeleteFile(ctx, e.bob, f.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer delete = %v", err)
	}
	if _, _, err := e.s.CreateShare(ctx, e.bob, f.ID, ShareRequest{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer share = %v", err)
	}
	if err := e.s.SetMember(ctx, e.carol, sp.ID, Member{Kind: "user", ID: "user:bob", Role: "editor"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("editor changing the access list = %v", err)
	}
	if _, err := e.s.ListMembers(ctx, e.carol, sp.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("editor listing members = %v", err)
	}
	if _, err := e.s.Upload(ctx, e.carol, UploadRequest{SpaceID: sp.ID, Name: "c.pdf", Body: bytes.NewReader(pdf(""))}); err != nil {
		t.Errorf("editor upload: %v", err)
	}
	// the permission is a second gate: a read-only token cannot write even as a manager
	ro := &Principal{Subject: "user:alice", Perms: []string{PermRead}}
	if _, err := e.s.Upload(ctx, ro, UploadRequest{SpaceID: sp.ID, Name: "d.pdf", Body: bytes.NewReader(pdf(""))}); !errors.Is(err, ErrForbidden) {
		t.Errorf("files.read token uploading = %v", err)
	}
	if _, err := e.s.CreateSpace(ctx, ro, "x"); !errors.Is(err, ErrForbidden) {
		t.Errorf("files.read token creating a space = %v", err)
	}
	none := &Principal{Subject: "user:alice"}
	if _, err := e.s.GetFile(ctx, none, f.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("no permission = %v", err)
	}
	// a viewer's view omits the fields of the higher roles
	vv, _ := e.s.GetFile(ctx, e.bob, f.ID)
	if vv.SHA256 != "" || vv.Subject != "" || vv.LegalHold != nil || vv.CreatedBy != "" || vv.Scan != "" {
		t.Errorf("viewer sees editor/manager fields: %+v", vv)
	}
	ev, _ := e.s.GetFile(ctx, e.carol, f.ID)
	if ev.SHA256 == "" || ev.CreatedBy == "" || ev.Subject != "" || ev.LegalHold != nil {
		t.Errorf("editor view: %+v", ev)
	}
	mv, _ := e.s.GetFile(ctx, e.alice, f.ID)
	if mv.LegalHold == nil || mv.Role != "manager" {
		t.Errorf("manager view: %+v", mv)
	}
	// denied attempts are audited
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE (outcome = 'denied')`) == 0 {
		t.Error("no denial was audited")
	}
}

func TestGroupGrantsAndLastManager(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Eng")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf(""))
	e.member(e.alice, sp.ID, "group", "grp-eng", "viewer")
	inGroup := &Principal{Subject: "user:dave", Perms: []string{PermRead}, Groups: []string{"grp-eng"}}
	if _, err := e.s.GetFile(ctx, inGroup, f.ID); err != nil {
		t.Errorf("a group member could not read: %v", err)
	}
	outside := &Principal{Subject: "user:dave", Perms: []string{PermRead}, Groups: []string{"grp-other"}}
	if _, err := e.s.GetFile(ctx, outside, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a non-member = %v", err)
	}
	// the strongest of user and group roles wins
	e.member(e.alice, sp.ID, "user", "user:dave", "editor")
	g := &Principal{Subject: "user:dave", Perms: []string{PermWrite}, Groups: []string{"grp-eng"}}
	if _, err := e.s.Upload(ctx, g, UploadRequest{SpaceID: sp.ID, Name: "d.pdf", Body: bytes.NewReader(pdf(""))}); err != nil {
		t.Errorf("editor via user grant: %v", err)
	}
	// nobody can leave a space without a manager
	if err := e.s.RemoveMember(ctx, e.alice, sp.ID, "user", "user:alice"); !errors.Is(err, ErrConflict) {
		t.Errorf("removing the last manager = %v", err)
	}
	if err := e.s.SetMember(ctx, e.alice, sp.ID, Member{Kind: "user", ID: "user:alice", Role: "viewer"}); !errors.Is(err, ErrConflict) {
		t.Errorf("demoting the last manager = %v", err)
	}
	for _, bad := range []Member{{"user", "alice", "viewer"}, {"user", "user:x", "owner"}, {"team", "x", "viewer"}, {"group", "has space", "viewer"}, {"user", "user:x/../y", "viewer"}} {
		if err := e.s.SetMember(ctx, e.alice, sp.ID, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("member %+v = %v, want invalid", bad, err)
		}
	}
}

// ---- upload -------------------------------------------------------------------------------------

func TestUploadChecksAndRandomKeys(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *StoreOptions) { c.Limits.MaxFileBytes = 1 << 16 })
	sp := e.space(e.alice, "Docs")

	f := e.upload(e.alice, sp.ID, "../../../etc/passwd.pdf", pdf("x"))
	if f.Name != "passwd.pdf" {
		t.Errorf("name = %q: the path part must be dropped", f.Name)
	}
	bl := e.blobs()
	if len(bl) != 1 {
		t.Fatalf("blobs: %v", bl)
	}
	key := filepath.Base(bl[0])
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(key) || strings.Contains(bl[0], "passwd") {
		t.Errorf("blob path %q is not a random key", bl[0])
	}
	if f.ContentType != "application/pdf" || f.Size != int64(len(pdf("x"))) || f.Version != 1 || f.SHA256 != hexSHA(pdf("x")) {
		t.Errorf("view: %+v", f)
	}
	// the database holds no content and no plaintext name
	raw := e.rawDB()
	if bytes.Contains(raw, []byte("passwd")) || bytes.Contains(raw, pdf("x")) {
		t.Error("the database file holds a plaintext name or the content")
	}

	cases := []struct {
		name string
		file string
		body []byte
		want error
	}{
		{"too large", "big.pdf", append(pdf(""), bytes.Repeat([]byte("a"), 1<<16)...), ErrTooLarge},
		{"executable renamed", "tool.pdf", []byte("MZ\x90\x00\x03"), ErrType},
		{"svg", "x.svg", []byte("<svg/>"), ErrType},
		{"no extension", "README", []byte("hello"), ErrType},
		{"empty", "e.txt", []byte{}, ErrInvalid},
		{"empty name", "", pdf(""), ErrInvalid},
		{"only a path", "../", pdf(""), ErrInvalid},
	}
	for _, c := range cases {
		_, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: c.file, Body: bytes.NewReader(c.body)})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if len(e.blobs()) != 1 || e.staged() != 0 || e.count(`SELECT COUNT(*) FROM pending_blobs`) != 0 {
		t.Errorf("a refused upload left something behind: %d blobs, %d staged, %d pending", len(e.blobs()), e.staged(), e.count(`SELECT COUNT(*) FROM pending_blobs`))
	}
	for _, bad := range []UploadRequest{
		{SpaceID: sp.ID, Name: "a.pdf", Ref: "no-colon"},
		{SpaceID: sp.ID, Name: "a.pdf", Subject: "bad subject!"},
		{SpaceID: sp.ID, Name: "a.pdf", FolderID: "fld_" + strings.Repeat("a", 24)},
		{SpaceID: sp.ID, Name: strings.Repeat("a", 300) + ".pdf"},
		{SpaceID: sp.ID, Name: "a.pdf", Description: strings.Repeat("d", 2000)},
	} {
		bad.Body = bytes.NewReader(pdf(""))
		if _, err := e.s.Upload(ctx, e.alice, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestUploadBodyErrorsLeaveNothing(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	_, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.pdf", Body: errReader{errors.New("connection reset")}})
	if err == nil {
		t.Fatal("a failing body succeeded")
	}
	body := io.MultiReader(bytes.NewReader(pdf("part")), errReader{errors.New("reset")})
	if _, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.pdf", Body: body}); err == nil {
		t.Fatal("a body that broke half way succeeded")
	}
	if len(e.blobs()) != 0 || e.staged() != 0 || e.count(`SELECT COUNT(*) FROM files`) != 0 {
		t.Errorf("leftovers: %d blobs, %d staged, %d files", len(e.blobs()), e.staged(), e.count(`SELECT COUNT(*) FROM files`))
	}
}

func TestScannerFailsClosed(t *testing.T) {
	// the default: required, and nothing real is wired
	e := newEnv(t, func(c *Config, o *StoreOptions) {
		t := true
		c.Scanner.Required = &t
		o.Scanner = NoopScanner{}
	})
	sp := e.space(e.alice, "Docs")
	if _, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.pdf", Body: bytes.NewReader(pdf(""))}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("required scanner not wired = %v, want unavailable", err)
	}
	// a scanner that cannot scan
	e = newEnv(t, func(c *Config, o *StoreOptions) {
		t := true
		c.Scanner.Required = &t
		o.Scanner = &FakeScanner{Err: errors.New("clamd down")}
	})
	sp = e.space(e.alice, "Docs")
	if _, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.pdf", Body: bytes.NewReader(pdf(""))}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("scanner error = %v", err)
	}
	if len(e.blobs()) != 0 || e.staged() != 0 || e.count(`SELECT COUNT(*) FROM files`) != 0 {
		t.Error("an unscanned upload was stored")
	}
	// with the requirement off and no scanner the file is kept and labelled
	e = newEnv(t, func(_ *Config, o *StoreOptions) { o.Scanner = NoopScanner{} })
	sp = e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf(""))
	if v, _ := e.s.GetFile(ctx, e.alice, f.ID); v.Scan != "skipped" {
		t.Errorf("scan = %q, want skipped", v.Scan)
	}
}

func TestInfectedUploadIsRefusedAuditedAndAnnounced(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	_, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.txt", Body: strings.NewReader("hello EICAR-TEST hello")})
	if !errors.Is(err, ErrInfected) {
		t.Fatalf("infected upload = %v", err)
	}
	if len(e.blobs()) != 0 || e.staged() != 0 || e.count(`SELECT COUNT(*) FROM files`) != 0 {
		t.Error("an infected file was kept")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'file.infected')`) != 1 {
		t.Error("the detection was not audited")
	}
	var payload string
	if err := e.s.db.QueryRow(`SELECT payload FROM outbox WHERE (type = 'file.infected.v1')`).Scan(&payload); err != nil {
		t.Fatalf("no file.infected.v1 event: %v", err)
	}
	if strings.Contains(payload, "a.txt") || strings.Contains(payload, "hello") {
		t.Errorf("the event carries a name or content: %s", payload)
	}
}

func TestQuotaAndCountLimits(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *StoreOptions) {
		c.Limits.MaxFileBytes = 1000
		c.Limits.QuotaBytesPerOwner = 1500
		c.Limits.MaxFilesPerSpace = 3
		c.Limits.MaxVersionsPerFile = 2
		c.Limits.MaxSpacesPerOwner = 2
	})
	sp := e.space(e.alice, "A")
	e.member(e.alice, sp.ID, "user", "user:bob", "editor")
	a := e.upload(e.alice, sp.ID, "a.txt", bytes.Repeat([]byte("a"), 900))
	// bob uploads into alice's space: it counts against the owner of the space
	if _, err := e.s.Upload(ctx, e.bob, UploadRequest{SpaceID: sp.ID, Name: "b.txt", Body: bytes.NewReader(bytes.Repeat([]byte("b"), 900))}); !errors.Is(err, ErrQuota) {
		t.Errorf("over quota = %v", err)
	}
	if len(e.blobs()) != 1 || e.count(`SELECT COUNT(*) FROM pending_blobs`) != 0 {
		t.Error("the refused upload left a blob")
	}
	e.upload(e.alice, sp.ID, "c.txt", []byte("small"))
	e.upload(e.alice, sp.ID, "d.txt", []byte("small"))
	if _, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "e.txt", Body: strings.NewReader("small")}); !errors.Is(err, ErrQuota) {
		t.Errorf("file count = %v", err)
	}
	if _, err := e.s.UploadVersion(ctx, e.alice, a.ID, strings.NewReader("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UploadVersion(ctx, e.alice, a.ID, strings.NewReader("v3")); !errors.Is(err, ErrConflict) {
		t.Errorf("version cap = %v", err)
	}
	e.space(e.alice, "B")
	if _, err := e.s.CreateSpace(ctx, e.alice, "C"); !errors.Is(err, ErrQuota) {
		t.Errorf("space cap = %v", err)
	}
	// purged bytes are given back
	if err := e.s.DeleteFile(ctx, e.alice, a.ID); err != nil {
		t.Fatal(err)
	}
	e.advance(31 * 24 * time.Hour)
	if _, err := e.s.RunRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "f.txt", Body: bytes.NewReader(bytes.Repeat([]byte("f"), 900))}); err != nil {
		t.Errorf("after a purge: %v", err)
	}
}

// ---- versions, content, integrity ----------------------------------------------------------------

func TestVersionsAreImmutableAndAddressable(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "contract.pdf", pdf("one"))
	f2, err := e.s.UploadVersion(ctx, e.alice, f.ID, bytes.NewReader(pdf("two two")))
	if err != nil || f2.Version != 2 || f2.Rev <= f.Rev {
		t.Fatalf("v2: %+v %v", f2, err)
	}
	if _, err := e.s.UploadVersion(ctx, e.alice, f.ID, strings.NewReader("not a pdf")); !errors.Is(err, ErrType) {
		t.Errorf("a version of the wrong type = %v", err)
	}
	c, err := e.s.OpenContent(ctx, e.alice, f.ID, 0)
	if err != nil || !bytes.Equal(e.readAll(c), pdf("two two")) {
		t.Errorf("latest: %v", err)
	}
	c, err = e.s.OpenContent(ctx, e.alice, f.ID, 1)
	if err != nil || !bytes.Equal(e.readAll(c), pdf("one")) {
		t.Errorf("v1: %v", err)
	}
	if _, err := e.s.OpenContent(ctx, e.alice, f.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("v3 = %v", err)
	}
	vs, err := e.s.ListVersions(ctx, e.alice, f.ID)
	if err != nil || len(vs) != 2 || vs[0].N != 2 || vs[1].N != 1 {
		t.Errorf("versions: %+v %v", vs, err)
	}
	if len(e.blobs()) != 2 {
		t.Errorf("blobs = %d, a new version must not overwrite the old blob", len(e.blobs()))
	}
	// a viewer cannot add one
	e.member(e.alice, sp.ID, "user", "user:bob", "viewer")
	if _, err := e.s.UploadVersion(ctx, e.bob, f.ID, bytes.NewReader(pdf("x"))); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer version = %v", err)
	}
}

func TestDownloadIsVerifiedAndAudited(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	body := append(pdf(""), bytes.Repeat([]byte("0123456789"), 20000)...)
	f := e.upload(e.alice, sp.ID, "big.pdf", body)
	before := e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'file.download')`)
	c, err := e.s.OpenContent(ctx, e.alice, f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.ContentType != "application/octet-stream" || c.Name != "big.pdf" || c.Size != int64(len(body)) {
		t.Errorf("content: %+v", c)
	}
	if !bytes.Equal(e.readAll(c), body) {
		t.Error("bytes differ")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'file.download')`) != before+1 {
		t.Error("the download was not audited")
	}
	// the stored object is corrupted: same length, one byte changed
	p := e.blobs()[0]
	raw, _ := os.ReadFile(p)
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = e.s.OpenContent(ctx, e.alice, f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = c.Stream(ctx, &out)
	_ = c.Body.Close()
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("a corrupted blob streamed: %v", err)
	}
	if out.Len() >= len(body) {
		t.Error("the whole corrupted object was written to the client")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'file.integrity_failed')`) != 1 {
		t.Error("the integrity failure was not audited")
	}
}

func TestAuditBeforeReturnFailsClosed(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf("x"))
	if _, err := e.s.db.Exec(`CREATE TRIGGER no_more_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'audit down'); END`); err != nil {
		t.Fatal(err)
	}
	if c, err := e.s.OpenContent(ctx, e.alice, f.ID, 0); !errors.Is(err, ErrUnavailable) || c != nil {
		t.Errorf("a download that could not be audited returned bytes: %v", err)
	}
	if _, err := e.s.GetFile(ctx, e.alice, f.ID); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a metadata read that could not be audited = %v", err)
	}
	// writes fail whole: no row, no blob, no event
	blobs, files, events := len(e.blobs()), e.count(`SELECT COUNT(*) FROM files`), e.count(`SELECT COUNT(*) FROM outbox`)
	if _, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "b.pdf", Body: bytes.NewReader(pdf("y"))}); err == nil {
		t.Error("an upload that could not be audited succeeded")
	}
	if err := e.s.DeleteFile(ctx, e.alice, f.ID); err == nil {
		t.Error("a delete that could not be audited succeeded")
	}
	if len(e.blobs()) != blobs || e.count(`SELECT COUNT(*) FROM files`) != files || e.count(`SELECT COUNT(*) FROM outbox`) != events {
		t.Error("a failed write left state behind")
	}
	if v, _ := e.s.db.Query(`SELECT 1`); v != nil {
		_ = v.Close()
	}
	var st string
	if err := e.s.db.QueryRow(`SELECT status FROM files WHERE (id = ?)`, f.ID).Scan(&st); err != nil || st != "active" {
		t.Errorf("status %q %v", st, err)
	}
}

// ---- sealed fields and key rotation ----------------------------------------------------------------

func TestSealedMetadataIsBoundToItsRow(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Very Secret Space")
	a := e.upload(e.alice, sp.ID, "alice-medical.pdf", pdf("a"))
	b := e.upload(e.alice, sp.ID, "other.pdf", pdf("b"))
	raw := e.rawDB()
	for _, plain := range []string{"Very Secret Space", "alice-medical"} {
		if bytes.Contains(raw, []byte(plain)) {
			t.Errorf("%q is stored in plaintext", plain)
		}
	}
	// a ciphertext copied to another row does not open there
	var sealed string
	if err := e.s.db.QueryRow(`SELECT name_sealed FROM files WHERE (id = ?)`, a.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec(`UPDATE files SET name_sealed = ? WHERE (id = ?)`, sealed, b.ID); err != nil {
		t.Fatal(err)
	}
	if v, err := e.s.GetFile(ctx, e.alice, b.ID); err == nil {
		t.Errorf("a moved ciphertext opened: %+v", v)
	}
}

func TestRotateKeysIsCompareAndSwap(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "before.pdf", pdf(""))
	g := e.upload(e.alice, sp.ID, "other.pdf", pdf(""))
	kr, err := ParseKeyring("k2 " + strings.Repeat("ab", 32) + "\n" + testKeyring)
	if err != nil {
		t.Fatal(err)
	}
	e.s.keys = kr
	// a rename lands between the rotation's read and its write
	renamed := false
	e.s.rotateHook = func() {
		if renamed {
			return
		}
		renamed = true
		n := "renamed.pdf"
		if _, err := e.s.UpdateFile(ctx, e.alice, f.ID, FilePatch{Name: &n}); err != nil {
			t.Error(err)
		}
	}
	n, err := e.s.RotateKeys(ctx)
	if err != nil || n == 0 {
		t.Fatalf("rotate: %d %v", n, err)
	}
	for _, id := range []string{f.ID, g.ID} {
		var sealed string
		if err := e.s.db.QueryRow(`SELECT name_sealed FROM files WHERE (id = ?)`, id).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if kr.NeedsRotation(sealed) {
			t.Errorf("%s is still sealed with a retired key", id)
		}
	}
	if v, err := e.s.GetFile(ctx, e.alice, f.ID); err != nil || v.Name != "renamed.pdf" {
		t.Errorf("the concurrent rename was lost: %+v %v", v, err)
	}
	if n, err := e.s.RotateKeys(ctx); err != nil || n != 0 {
		t.Errorf("second rotation: %d %v", n, err)
	}
}

// ---- metadata, folders, listing ------------------------------------------------------------------

func TestUpdateFileUsesOptimisticConcurrencyAndKeepsTheExtension(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf(""))
	n := "renamed.pdf"
	d := "the description"
	v, err := e.s.UpdateFile(ctx, e.alice, f.ID, FilePatch{Name: &n, Description: &d, Rev: f.Rev})
	if err != nil || v.Name != "renamed.pdf" || v.Description != d || v.Rev != f.Rev+1 {
		t.Fatalf("update: %+v %v", v, err)
	}
	if _, err := e.s.UpdateFile(ctx, e.alice, f.ID, FilePatch{Name: &n, Rev: f.Rev}); !errors.Is(err, ErrConflict) {
		t.Errorf("a stale rev = %v", err)
	}
	for _, bad := range []string{"a.exe", "a.png", "noext", "", "../x.pdf/"} {
		if _, err := e.s.UpdateFile(ctx, e.alice, f.ID, FilePatch{Name: &bad}); err == nil {
			t.Errorf("renamed to %q", bad)
		}
	}
	fo, err := e.s.CreateFolder(ctx, e.alice, sp.ID, "", "Contracts")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := e.s.UpdateFile(ctx, e.alice, f.ID, FilePatch{FolderID: &fo.ID}); err != nil || v.FolderID != fo.ID {
		t.Errorf("move: %+v %v", v, err)
	}
	other := e.space(e.alice, "Other")
	of, _ := e.s.CreateFolder(ctx, e.alice, other.ID, "", "Elsewhere")
	if _, err := e.s.UpdateFile(ctx, e.alice, f.ID, FilePatch{FolderID: &of.ID}); err == nil {
		t.Error("moved a file into a folder of another space")
	}
}

func TestFolders(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *StoreOptions) { c.Limits.MaxFolderDepth = 3 })
	sp := e.space(e.alice, "Docs")
	var parent string
	for i := 1; i <= 3; i++ {
		f, err := e.s.CreateFolder(ctx, e.alice, sp.ID, parent, fmt.Sprintf("level %d", i))
		if err != nil {
			t.Fatalf("depth %d: %v", i, err)
		}
		parent = f.ID
	}
	if _, err := e.s.CreateFolder(ctx, e.alice, sp.ID, parent, "too deep"); !errors.Is(err, ErrInvalid) {
		t.Errorf("depth cap = %v", err)
	}
	other := e.space(e.alice, "Other")
	if _, err := e.s.CreateFolder(ctx, e.alice, other.ID, parent, "x"); err == nil {
		t.Error("a parent from another space was accepted")
	}
	fs, err := e.s.ListFolders(ctx, e.alice, sp.ID)
	if err != nil || len(fs) != 3 || fs[0].Name == "" {
		t.Errorf("folders: %v %v", fs, err)
	}
	// a non-empty folder cannot be removed
	up, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, FolderID: parent, Name: "a.pdf", Body: bytes.NewReader(pdf(""))})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteFolder(ctx, e.alice, parent); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting a folder with a file = %v", err)
	}
	_ = e.s.DeleteFile(ctx, e.alice, up.ID)
	if err := e.s.DeleteFolder(ctx, e.alice, parent); !errors.Is(err, ErrConflict) {
		t.Errorf("a folder holding a deleted (restorable) file = %v", err)
	}
}

func TestListingIsBoundedPagedAndScoped(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	hidden := e.space(e.bob, "Bob's")
	e.upload(e.bob, hidden.ID, "bob.pdf", pdf(""))
	for i := 0; i < 7; i++ {
		e.upload(e.alice, sp.ID, fmt.Sprintf("f%d.pdf", i), pdf(""))
	}
	seen := map[string]bool{}
	cur := ""
	pages := 0
	for {
		pg, err := e.s.ListFiles(ctx, e.alice, ListQuery{SpaceID: sp.ID, Cursor: cur, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, f := range pg.Items {
			if seen[f.ID] {
				t.Error("duplicate across pages")
			}
			seen[f.ID] = true
		}
		if pg.Next == "" {
			break
		}
		cur = pg.Next
	}
	if len(seen) != 7 || pages != 3 {
		t.Errorf("saw %d files in %d pages", len(seen), pages)
	}
	// a global list never includes spaces the caller cannot see, even when the OR in the query
	// would otherwise let them through
	all, _ := e.s.ListFiles(ctx, e.alice, ListQuery{})
	if len(all.Items) != 7 {
		t.Errorf("global list = %d", len(all.Items))
	}
	if pg, _ := e.s.ListFiles(ctx, e.alice, ListQuery{Limit: 100000}); len(pg.Items) > 200 {
		t.Error("the page size is not capped")
	}
	if _, err := e.s.ListFiles(ctx, e.alice, ListQuery{Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a bad cursor = %v", err)
	}
	// by reference
	r := &UploadRequest{SpaceID: sp.ID, Name: "resume.pdf", Ref: "jobs:application-1234", Subject: "cand_0123456789abcdef", Body: bytes.NewReader(pdf(""))}
	if _, err := e.s.Upload(ctx, e.alice, *r); err != nil {
		t.Fatal(err)
	}
	pg, _ := e.s.ListFiles(ctx, e.alice, ListQuery{Ref: "jobs:application-1234"})
	if len(pg.Items) != 1 || pg.Items[0].Subject != "cand_0123456789abcdef" {
		t.Errorf("by ref: %+v", pg.Items)
	}
	if pg, _ := e.s.ListFiles(ctx, e.bob, ListQuery{Ref: "jobs:application-1234"}); len(pg.Items) != 0 {
		t.Error("a reference lookup crossed a space boundary")
	}
}

// ---- share links -----------------------------------------------------------------------------------

func TestShareLinks(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "offer.pdf", pdf("offer"))
	tok, sh, err := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{TTL: time.Hour, Label: "for Dana"})
	if err != nil || sh.ExpiresAt != e.now().Add(time.Hour) {
		t.Fatalf("share: %+v %v", sh, err)
	}
	if !strings.HasPrefix(tok, sh.ID+".") {
		t.Errorf("token %q does not carry the share id", tok)
	}
	c, err := e.s.OpenShare(ctx, tok, "1.2.3.4")
	if err != nil || !bytes.Equal(e.readAll(c), pdf("offer")) || c.ContentType != "application/octet-stream" || c.Name != "offer.pdf" {
		t.Fatalf("open share: %v %+v", err, c)
	}
	// the token is never stored or audited; the label is sealed
	raw := string(e.rawDB())
	secretPart := tok[strings.LastIndex(tok, ".")+1:]
	if strings.Contains(raw, secretPart) || strings.Contains(raw, "for Dana") {
		t.Error("the token or the label is stored in the clear")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'share.download') AND (resource = ?)`, "file:"+f.ID) != 1 {
		t.Error("the share download was not audited")
	}
	// tampering of any part is "not found"
	parts := strings.Split(tok, ".")
	for _, bad := range []string{
		tok + "x", parts[0] + "." + parts[1] + ".AAAA", parts[0] + ".9999999999." + parts[2], "shr_" + strings.Repeat("0", 24) + "." + parts[1] + "." + parts[2],
		"", "x", "..", tok[:len(tok)-1],
	} {
		if _, err := e.s.OpenShare(ctx, bad, "9.9.9.9"); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrRate) {
			t.Errorf("token %q = %v", bad, err)
		}
	}
	// expiry
	e.advance(2 * time.Hour)
	if _, err := e.s.OpenShare(ctx, tok, "1.2.3.4"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired = %v", err)
	}
	// ttl bounds
	for _, ttl := range []time.Duration{-time.Hour, time.Second * 30, 24 * 365 * time.Hour} {
		if _, _, err := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{TTL: ttl}); !errors.Is(err, ErrInvalid) {
			t.Errorf("ttl %v = %v", ttl, err)
		}
	}
}

func TestShareOneTimeRevocationAndErasure(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf("a"))
	tok, _, _ := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{OneTime: true})
	if c, err := e.s.OpenShare(ctx, tok, "1.1.1.1"); err != nil {
		t.Fatal(err)
	} else {
		_ = c.Body.Close()
	}
	if _, err := e.s.OpenShare(ctx, tok, "1.1.1.1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a one-time link worked twice: %v", err)
	}
	// revoke
	tok2, sh2, _ := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{})
	e.member(e.alice, sp.ID, "user", "user:bob", "editor")
	if err := e.s.RevokeShare(ctx, e.carol, sh2.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger revoking = %v", err)
	}
	if err := e.s.RevokeShare(ctx, e.alice, sh2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.OpenShare(ctx, tok2, "1.1.1.1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked link = %v", err)
	}
	// a link to a deleted file stops working, and works again on restore
	tok3, _, _ := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{})
	_ = e.s.DeleteFile(ctx, e.alice, f.ID)
	if _, err := e.s.OpenShare(ctx, tok3, "1.1.1.1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("link to a deleted file = %v", err)
	}
	// listing: creator and manager only
	if _, err := e.s.ListShares(ctx, e.carol, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger listing shares = %v", err)
	}
	shs, err := e.s.ListShares(ctx, e.alice, f.ID)
	if err != nil || len(shs) != 3 {
		t.Errorf("shares: %v %v", shs, err)
	}
	// the cap
	e2 := newEnv(t, func(c *Config, _ *StoreOptions) { c.Share.MaxPerFile = 2 })
	sp2 := e2.space(e2.alice, "D")
	f2 := e2.upload(e2.alice, sp2.ID, "a.pdf", pdf(""))
	for i := 0; i < 2; i++ {
		if _, _, err := e2.s.CreateShare(ctx, e2.alice, f2.ID, ShareRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e2.s.CreateShare(ctx, e2.alice, f2.ID, ShareRequest{}); !errors.Is(err, ErrQuota) {
		t.Errorf("share cap = %v", err)
	}
}

func TestShareFailuresAreThrottledButValidLinksAreNot(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *StoreOptions) { c.Share.FailuresPerMinute = 5 })
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf("a"))
	tok, _, _ := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{})
	limited := false
	for i := 0; i < 10; i++ {
		if _, err := e.s.OpenShare(ctx, "shr_"+strings.Repeat("1", 24)+".1.zzzz", "6.6.6.6"); errors.Is(err, ErrRate) {
			limited = true
		}
	}
	if !limited {
		t.Fatal("repeated failures were never limited")
	}
	// the same address, now over the limit, still gets a valid link
	c, err := e.s.OpenShare(ctx, tok, "6.6.6.6")
	if err != nil {
		t.Fatalf("a valid link from a throttled address: %v", err)
	}
	_ = c.Body.Close()
	// other addresses are unaffected, and a minute later the address recovers
	if _, err := e.s.OpenShare(ctx, "shr_"+strings.Repeat("1", 24)+".1.zzzz", "7.7.7.7"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another address = %v", err)
	}
	e.advance(2 * time.Minute)
	if _, err := e.s.OpenShare(ctx, "shr_"+strings.Repeat("1", 24)+".1.zzzz", "6.6.6.6"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the window = %v", err)
	}
}

// ---- deletion, retention, holds, erasure ---------------------------------------------------------------

func TestSoftDeleteRestoreRetentionAndHold(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	e.member(e.alice, sp.ID, "user", "user:bob", "viewer")
	f := e.upload(e.alice, sp.ID, "gone.pdf", pdf("gone"))
	if err := e.s.DeleteFile(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetFile(ctx, e.bob, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a viewer sees a deleted file: %v", err)
	}
	if pg, _ := e.s.ListFiles(ctx, e.alice, ListQuery{SpaceID: sp.ID}); len(pg.Items) != 0 {
		t.Error("a deleted file is listed")
	}
	if pg, _ := e.s.ListFiles(ctx, e.alice, ListQuery{SpaceID: sp.ID, Deleted: true}); len(pg.Items) != 1 || pg.Items[0].DeletedAt == nil {
		t.Errorf("trash: %+v", pg.Items)
	}
	if _, err := e.s.ListFiles(ctx, e.bob, ListQuery{SpaceID: sp.ID, Deleted: true}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a viewer listing the trash = %v", err)
	}
	if c, err := e.s.OpenContent(ctx, e.alice, f.ID, 0); !errors.Is(err, ErrNotFound) || c != nil {
		t.Errorf("content of a deleted file = %v", err)
	}
	if err := e.s.RestoreFile(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RestoreFile(ctx, e.alice, f.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("restoring an active file = %v", err)
	}
	// a legal hold blocks deletion and every purge
	if err := e.s.SetHold(ctx, e.alice, f.ID, true, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("a manager placing a hold = %v: holds are for files.admin", err)
	}
	if err := e.s.SetHold(ctx, e.root, f.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteFile(ctx, e.alice, f.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting a file under hold = %v", err)
	}
	if err := e.s.SetHold(ctx, e.root, f.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	// a retention date blocks deletion until it passes
	until := e.now().Add(48 * time.Hour)
	if err := e.s.SetHold(ctx, e.root, f.ID, false, &until); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteFile(ctx, e.alice, f.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting before retain_until = %v", err)
	}
	e.advance(49 * time.Hour)
	if err := e.s.DeleteFile(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	// the sweep leaves it for trash_days, skips a held file, then purges
	e.advance(10 * 24 * time.Hour)
	rep, _ := e.s.RunRetention(ctx)
	if rep.Purged != 0 || len(e.blobs()) != 1 {
		t.Errorf("purged inside the trash window: %+v", rep)
	}
	if err := e.s.SetHold(ctx, e.root, f.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	e.advance(30 * 24 * time.Hour)
	rep, _ = e.s.RunRetention(ctx)
	if rep.Purged != 0 || rep.SkippedHold != 1 || len(e.blobs()) != 1 {
		t.Errorf("a held file was purged: %+v", rep)
	}
	if err := e.s.SetHold(ctx, e.root, f.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := e.s.RunRetention(ctx)
	if err != nil || rep.Purged != 1 || len(e.blobs()) != 0 {
		t.Fatalf("purge: %+v %v blobs=%d", rep, err, len(e.blobs()))
	}
	if _, err := e.s.GetFile(ctx, e.alice, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a purged file = %v", err)
	}
	// a tombstone, so a restore from an older backup cannot resurrect it
	es, err := e.s.ListErasures(ctx, e.root, 0)
	if err != nil || len(es) == 0 {
		t.Fatalf("tombstones: %v %v", es, err)
	}
	for _, x := range es {
		if x.Subject != f.ID || !subjectRe.MatchString(x.Subject) || x.Date == "" {
			t.Errorf("tombstone %+v", x)
		}
	}
	var name sql.NullString
	_ = e.s.db.QueryRow(`SELECT name_sealed FROM files WHERE (id = ?)`, f.ID).Scan(&name)
	if name.Valid {
		t.Error("the sealed name survived the purge")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'file.retention_purge')`) != 1 {
		t.Error("the purge was not audited")
	}
}

func TestEraseSubjectRemovesBlobsAndScrubsTheDatabaseFiles(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Recruiting")
	const subject = "cand_0123456789abcdef"
	var keep *FileView
	for i, n := range []string{"zebra-resume-secretname.pdf", "zebra-cover-secretname.pdf"} {
		f, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: n, Subject: subject, Description: "zebra personal note", Body: bytes.NewReader(pdf(fmt.Sprint(i)))})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := e.s.UploadVersion(ctx, e.alice, f.ID, bytes.NewReader(pdf("second"))); err != nil {
				t.Fatal(err)
			}
		}
	}
	keep = e.upload(e.alice, sp.ID, "unrelated.pdf", pdf("keep"))
	tok, _, _ := e.s.CreateShare(ctx, e.alice, keep.ID, ShareRequest{})
	if len(e.blobs()) != 4 {
		t.Fatalf("blobs = %d", len(e.blobs()))
	}
	if _, err := e.s.EraseSubject(ctx, e.alice, subject); !errors.Is(err, ErrForbidden) {
		t.Errorf("a space manager erasing = %v: erasure is files.admin", err)
	}
	// a legal hold blocks it
	var held string
	_ = e.s.db.QueryRow(`SELECT id FROM files WHERE (subject = ?) LIMIT 1`, subject).Scan(&held)
	_ = e.s.SetHold(ctx, e.root, held, true, nil)
	if _, err := e.s.EraseSubject(ctx, e.root, subject); !errors.Is(err, ErrConflict) {
		t.Errorf("erasure under hold = %v", err)
	}
	if len(e.blobs()) != 4 {
		t.Error("a blocked erasure deleted blobs")
	}
	_ = e.s.SetHold(ctx, e.root, held, false, nil)
	er, err := e.s.EraseSubject(ctx, e.root, subject)
	if err != nil {
		t.Fatal(err)
	}
	if len(er.Classes) != 2 || len(e.blobs()) != 1 {
		t.Errorf("erasure %+v, blobs left %d", er, len(e.blobs()))
	}
	pg, _ := e.s.AdminFiles(ctx, e.root, subject, "", 0)
	for _, f := range pg.Items {
		if f.Status != "purged" {
			t.Errorf("file %+v is not purged", f)
		}
	}
	// nothing identifying is left in the database file or its write-ahead log
	raw := e.rawDB()
	for _, needle := range []string{"zebra", "secretname"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Errorf("%q survived in the database files", needle)
		}
	}
	if fi, err := os.Stat(e.dbPath + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the write-ahead log was not truncated (%d bytes)", fi.Size())
	}
	// tombstones for the backup ledger, one per class, no personal data
	es, _ := e.s.ListErasures(ctx, e.root, 0)
	classes := map[string]bool{}
	for _, x := range es {
		if x.Subject == subject {
			classes[x.Class] = true
		}
	}
	if !classes[ClassContent] || !classes[ClassMetadata] {
		t.Errorf("tombstones: %+v", es)
	}
	// the unrelated file and its link are untouched
	if c, err := e.s.OpenShare(ctx, tok, "1.1.1.1"); err != nil {
		t.Errorf("an unrelated share stopped working: %v", err)
	} else {
		_ = c.Body.Close()
	}
	// idempotent, and the purge hook works by subject and by file id, ignoring holds
	if _, err := e.s.EraseSubject(ctx, e.root, subject); err != nil {
		t.Errorf("repeat erasure: %v", err)
	}
	if err := e.s.PurgeSubject(ctx, e.root, subject, ClassContent); err != nil {
		t.Errorf("purge hook: %v", err)
	}
	_ = e.s.SetHold(ctx, e.root, keep.ID, true, nil)
	if err := e.s.PurgeSubject(ctx, &Principal{Subject: "svc:restore", Restore: true}, keep.ID, ClassContent); err != nil {
		t.Errorf("purge by file id: %v", err)
	}
	if len(e.blobs()) != 0 {
		t.Errorf("blobs left after the ledger replay: %d", len(e.blobs()))
	}
	for _, bad := range [][2]string{{"x y", ClassContent}, {subject, "nonsense"}, {"", ClassContent}} {
		if err := e.s.PurgeSubject(ctx, e.root, bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("purge %v = %v", bad, err)
		}
	}
	if err := e.s.PurgeSubject(ctx, e.alice, subject, ClassContent); !errors.Is(err, ErrForbidden) {
		t.Errorf("purge by a non-admin = %v", err)
	}
}

func TestPendingBlobsAreSweptAfterACrash(t *testing.T) {
	e := newEnv(t)
	key := newBlobKey()
	if err := e.be.Put(ctx, key, strings.NewReader("orphan"), 6, sum([]byte("orphan"))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec(`INSERT INTO pending_blobs (blob_key, created_at) VALUES (?, ?)`, key, e.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RunRetention(ctx); err != nil || len(e.blobs()) != 1 {
		t.Fatalf("a fresh pending blob (an upload in flight) was swept: %v", err)
	}
	e.advance(2 * time.Hour)
	if _, err := e.s.RunRetention(ctx); err != nil || len(e.blobs()) != 0 || e.count(`SELECT COUNT(*) FROM pending_blobs`) != 0 {
		t.Errorf("the orphan survived: %v %d", err, len(e.blobs()))
	}
}

// ---- events, audit chain, admin ------------------------------------------------------------------------

func TestEventsCarryIdsOnly(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Secret Project Name")
	f, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "layoffs-plan.pdf", Description: "confidential words", Ref: "board:pack-7", Body: bytes.NewReader(pdf("x"))})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{Label: "label text"})
	_ = e.s.DeleteFile(ctx, e.alice, f.ID)
	rows, err := e.s.db.Query(`SELECT type, payload FROM outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var typ, payload string
		_ = rows.Scan(&typ, &payload)
		got[typ] = true
		var env struct {
			ID, Type, Source string
			Data             map[string]any
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil || env.Source != "files" || env.Type != typ || !regexp.MustCompile(`^evt_[0-9a-f]{24}$`).MatchString(env.ID) {
			t.Errorf("envelope %s: %v", payload, err)
		}
		for _, bad := range []string{"layoffs", "confidential", "Secret Project", "label text"} {
			if strings.Contains(payload, bad) {
				t.Errorf("%s carries %q", typ, bad)
			}
		}
	}
	for _, want := range []string{"file.uploaded.v1", "file.shared.v1", "file.deleted.v1"} {
		if !got[want] {
			t.Errorf("no %s event", want)
		}
	}
	// no events when the deployment has no can-core
	e2 := newEnv(t, func(_ *Config, o *StoreOptions) { o.EventsOn = false })
	sp2 := e2.space(e2.alice, "S")
	e2.upload(e2.alice, sp2.ID, "a.pdf", pdf(""))
	if e2.count(`SELECT COUNT(*) FROM outbox`) != 0 {
		t.Error("events queued with no publisher")
	}
}

func TestAuditChainIsAppendOnlyAndVerifies(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf(""))
	_, _ = e.s.GetFile(ctx, e.alice, f.ID)
	if err := e.s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec(`UPDATE audit_log SET actor = 'x' WHERE (seq = 1)`); err == nil {
		t.Error("an audit row was updated")
	}
	if _, err := e.s.db.Exec(`DELETE FROM audit_log`); err == nil {
		t.Error("audit rows were deleted")
	}
	// a rewrite behind the triggers is detected by the chain
	_, _ = e.s.db.Exec(`DROP TRIGGER audit_log_no_update`)
	_, _ = e.s.db.Exec(`UPDATE audit_log SET actor = 'mallory' WHERE (seq = 1)`)
	if err := e.s.VerifyAudit(ctx); err == nil {
		t.Error("a rewritten audit row was not detected")
	}
	// the log never holds names or content
	rows, _ := e.s.db.Query(`SELECT actor || action || resource || fields FROM audit_log`)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		if strings.Contains(s, "a.pdf") || strings.Contains(s, "Docs") {
			t.Errorf("audit row carries a name: %s", s)
		}
	}
}

func TestAdminSeesNoNamesAndNeedsThePermission(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	e.upload(e.alice, sp.ID, "private-name.pdf", pdf(""))
	pg, err := e.s.AdminFiles(ctx, e.root, "", "", 0)
	if err != nil || len(pg.Items) != 1 {
		t.Fatalf("admin files: %+v %v", pg, err)
	}
	b, _ := json.Marshal(pg)
	if strings.Contains(string(b), "private-name") || strings.Contains(string(b), "Docs") {
		t.Errorf("the admin listing carries names: %s", b)
	}
	if _, err := e.s.AdminFiles(ctx, e.alice, "", "", 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("a non-admin = %v", err)
	}
	// the admin cannot read content of a space they are not in
	if _, err := e.s.OpenContent(ctx, e.root, pg.Items[0].ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("admin content access = %v: files.admin administers, it does not read", err)
	}
	if _, err := e.s.ListAudit(ctx, e.alice, 0, 10); !errors.Is(err, ErrForbidden) {
		t.Errorf("audit as non-admin = %v", err)
	}
	a, err := e.s.ListAudit(ctx, e.root, 0, 1)
	if err != nil || len(a.Items) != 1 || a.Next == "" {
		t.Errorf("audit page: %+v %v", a, err)
	}
}

func TestOneTimeLinkHasExactlyOneWinnerUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf("a"))
	tok, _, err := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{OneTime: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := e.s.OpenShare(ctx, tok, "5.5.5.5")
			if err == nil {
				wins.Add(1)
				_ = c.Body.Close()
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d concurrent opens of a one-time link succeeded, want exactly 1", wins.Load())
	}
}

func TestUploadHonoursTheDeadline(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.s.Upload(cctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "a.pdf", Body: bytes.NewReader(pdf(strings.Repeat("x", 100000)))}); err == nil {
		t.Error("an upload under a cancelled context succeeded")
	}
	if len(e.blobs()) != 0 || e.staged() != 0 {
		t.Error("a cancelled upload left something behind")
	}
}
