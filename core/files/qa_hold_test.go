package files

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// QA major 5: a body without legal_hold (even `{}`) lifted a legal hold.
func TestSetHoldNeedsAnExplicitDecision(t *testing.T) {
	a := newAPIEnv(t)
	sp := a.space(a.alice, "S")
	f := a.upload(a.alice, sp.ID, "a.pdf", pdf(""))
	state := func() (hold int, retain bool) {
		a.t.Helper()
		var h, r int
		if err := a.s.db.QueryRow(`SELECT legal_hold, retain_until IS NOT NULL FROM files WHERE (id = ?)`, f.ID).Scan(&h, &r); err != nil {
			t.Fatal(err)
		}
		return h, r == 1
	}
	put := func(body string) int {
		resp, _ := a.do("PUT", "/v1/files/"+f.ID+"/hold", apiToken, strings.NewReader(body), "Content-Type", "application/json")
		return resp.StatusCode
	}
	until := a.now().Add(365 * 24 * time.Hour).Format(time.DateOnly)
	if c := put(`{"legal_hold":true,"retain_until":"` + until + `"}`); c != 204 {
		t.Fatalf("set: %d", c)
	}
	for _, bad := range []string{`{}`, `{"retain_until":null}`, `{"legal_hold":null}`, `{"legal_hold":true,"retain_until":""}`, `{"legal_hold":true,"retain_until":"soon"}`, `{"legal_hold":true,"retain_until":5}`} {
		if c := put(bad); c != 400 {
			t.Errorf("%s: %d, want 400", bad, c)
		}
		if h, r := state(); h != 1 || !r {
			t.Fatalf("%s changed the hold: hold=%d retain=%v", bad, h, r)
		}
	}
	// omitted retain_until leaves the date alone; null clears it
	if c := put(`{"legal_hold":false}`); c != 204 {
		t.Fatalf("lift: %d", c)
	}
	if h, r := state(); h != 0 || !r {
		t.Errorf("lifting the hold must not touch the retention date: hold=%d retain=%v", h, r)
	}
	if c := put(`{"legal_hold":false,"retain_until":null}`); c != 204 {
		t.Fatalf("clear: %d", c)
	}
	if _, r := state(); r {
		t.Error("null did not clear retain_until")
	}
}

// QA major 7: erasure on request destroyed files that had to be kept until a date.
func TestErasureRespectsTheRetentionDate(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "S")
	const subject = "pers_0123456789abcdef"
	f, err := e.s.Upload(ctx, e.alice, UploadRequest{SpaceID: sp.ID, Name: "payslip.pdf", Subject: subject, Body: strings.NewReader("%PDF-1.7 x")})
	if err != nil {
		t.Fatal(err)
	}
	until := e.now().Add(48 * time.Hour)
	if err := e.s.SetHold(ctx, e.root, f.ID, false, &until); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EraseSubject(ctx, e.root, subject); !errors.Is(err, ErrConflict) {
		t.Errorf("erasure before retain_until = %v, want conflict", err)
	}
	if len(e.blobs()) != 1 {
		t.Fatal("a retained file was destroyed")
	}
	e.advance(72 * time.Hour)
	if _, err := e.s.EraseSubject(ctx, e.root, subject); err != nil || len(e.blobs()) != 0 {
		t.Errorf("after the date: %v, blobs %d", err, len(e.blobs()))
	}
}
