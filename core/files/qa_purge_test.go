package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// hookBackend lets a test act in the middle of a storage call.
type hookBackend struct {
	Backend
	onPut    func()
	onDelete func() error
}

func (h *hookBackend) Put(ctx context.Context, key string, r io.Reader, size int64, sha string) error {
	if f := h.onPut; f != nil {
		h.onPut = nil
		f()
	}
	return h.Backend.Put(ctx, key, r, size, sha)
}

func (h *hookBackend) Delete(ctx context.Context, key string) error {
	if f := h.onDelete; f != nil {
		if err := f(); err != nil {
			return err
		}
	}
	return h.Backend.Delete(ctx, key)
}

func hookEnv(t *testing.T, mods ...func(*Config, *StoreOptions)) (*env, *hookBackend) {
	t.Helper()
	h := &hookBackend{}
	mods = append(mods, func(_ *Config, o *StoreOptions) { h.Backend = o.Backend; o.Backend = h })
	return newEnv(t, mods...), h
}

// QA blocker 2: the decision to purge is made in one transaction before a blob is touched; from
// then on nothing may hold, restore, version or share the file.
func TestPurgeClaimsTheFileBeforeTouchingBlobs(t *testing.T) {
	e, h := hookEnv(t)
	sp := e.space(e.alice, "Docs")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf("a"))
	if err := e.s.DeleteFile(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	e.advance(40 * 24 * time.Hour)
	tried := 0
	h.onDelete = func() error {
		tried++
		if err := e.s.SetHold(ctx, e.root, f.ID, true, nil); !errors.Is(err, ErrConflict) {
			t.Errorf("a hold during the purge = %v, want conflict: it can no longer be honoured", err)
		}
		if err := e.s.RestoreFile(ctx, e.alice, f.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("restore during the purge = %v", err)
		}
		if _, err := e.s.UploadVersion(ctx, e.alice, f.ID, bytes.NewReader(pdf("v2"))); !errors.Is(err, ErrNotFound) {
			t.Errorf("a new version during the purge = %v", err)
		}
		if _, _, err := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("a share during the purge = %v", err)
		}
		return nil
	}
	rep, err := e.s.RunRetention(ctx)
	if err != nil || rep.Purged != 1 || tried != 1 || len(e.blobs()) != 0 {
		t.Fatalf("sweep: %+v %v tried=%d blobs=%d", rep, err, tried, len(e.blobs()))
	}
}

func TestAVersionUploadedDuringAnErasureLeavesNoBlob(t *testing.T) {
	e, h := hookEnv(t)
	sp := e.space(e.alice, "Docs")
	const subject = "cand_0123456789abcdef"
	f, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "r.pdf", Subject: subject, Body: bytes.NewReader(pdf("1"))})
	if err != nil {
		t.Fatal(err)
	}
	// the erasure runs while the second version's bytes are on their way to the backend
	h.onPut = func() {
		if _, err := e.s.EraseSubject(ctx, e.root, subject); err != nil {
			t.Errorf("erase: %v", err)
		}
	}
	if _, err := e.s.UploadVersion(ctx, e.alice, f.ID, bytes.NewReader(pdf("2"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("the version was accepted after the erasure: %v", err)
	}
	if len(e.blobs()) != 0 || e.count(`SELECT COUNT(*) FROM versions WHERE (purged_at IS NULL)`) != 0 {
		t.Errorf("orphans: %d blobs", len(e.blobs()))
	}
}

func TestAnInterruptedPurgeIsResumed(t *testing.T) {
	e, h := hookEnv(t)
	sp := e.space(e.alice, "Docs")
	const subject = "cand_0123456789abcdef"
	f, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "r.pdf", Subject: subject, Body: bytes.NewReader(pdf("1"))})
	if err != nil {
		t.Fatal(err)
	}
	h.onDelete = func() error { return errors.New("storage down") }
	if _, err := e.s.EraseSubject(ctx, e.root, subject); err == nil {
		t.Fatal("an erasure that could not delete the blob reported success")
	}
	// the intent is durable: invisible, tombstoned, and finished by the next sweep
	if _, err := e.s.GetFile(ctx, e.alice, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a file being purged is visible: %v", err)
	}
	if es, _ := e.s.ListErasures(ctx, e.root, 0); len(es) != 2 || es[0].Subject != subject {
		t.Errorf("tombstones after the claim: %+v", es)
	}
	h.onDelete = nil
	rep, err := e.s.RunRetention(ctx)
	if err != nil || rep.Purged != 1 || len(e.blobs()) != 0 || e.count(`SELECT COUNT(*) FROM files WHERE (status = 'purged')`) != 1 {
		t.Fatalf("resume: %+v %v blobs=%d", rep, err, len(e.blobs()))
	}
}

// QA blocker 3: a retention purge of one file must not tombstone the whole data subject, or a
// restore would destroy the subject's other files.
func TestRetentionTombstonesTheFileNotTheSubject(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "Docs")
	const subject = "pers_0123456789abcdef"
	up := func(n string) *FileView {
		f, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: n, Subject: subject, Body: bytes.NewReader(pdf(n))})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	gone, kept := up("old.pdf"), up("contract.pdf")
	if err := e.s.DeleteFile(ctx, e.alice, gone.ID); err != nil {
		t.Fatal(err)
	}
	e.advance(40 * 24 * time.Hour)
	if _, err := e.s.RunRetention(ctx); err != nil {
		t.Fatal(err)
	}
	es, _ := e.s.ListErasures(ctx, e.root, 0)
	if len(es) != 2 {
		t.Fatalf("tombstones: %+v", es)
	}
	for _, x := range es {
		if x.Subject != gone.ID {
			t.Errorf("tombstone for %q, want only the purged file %q", x.Subject, gone.ID)
		}
		// the replay a restore performs
		if err := e.s.PurgeSubject(ctx, e.root, x.Subject, x.Class); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.GetFile(ctx, e.alice, kept.ID); err != nil {
		t.Errorf("the subject's other file was destroyed by the replay: %v", err)
	}
}
