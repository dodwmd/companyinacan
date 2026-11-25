package backup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLedger(t *testing.T) *Ledger {
	t.Helper()
	return &Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl"), Now: func() time.Time { return time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC) }}
}

func TestLedgerAppendAndRead(t *testing.T) {
	l := newLedger(t)
	e1, added, err := l.Append("subj-123", "lead-submission")
	if err != nil || !added || e1.Seq != 1 || e1.Date != "2026-11-25" {
		t.Fatalf("%+v %v %v", e1, added, err)
	}
	if _, added, _ := l.Append("subj-123", "lead-submission"); added {
		t.Error("duplicate erasure appended twice")
	}
	if _, added, _ := l.Append("subj-123", "other-class"); !added {
		t.Error("different class is a different erasure")
	}
	es, err := l.Read()
	if err != nil || len(es) != 2 || es[1].Seq != 2 || es[1].Prev != es[0].Hash {
		t.Fatalf("%+v %v", es, err)
	}
	st, _ := os.Stat(l.Path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	raw, _ := os.ReadFile(l.Path)
	if strings.Contains(string(raw), "@") {
		t.Error("ledger must not carry PII")
	}
}

func TestLedgerRejectsPII(t *testing.T) {
	l := newLedger(t)
	for _, s := range []string{"jane@example.com", "Jane Doe", "", strings.Repeat("a", 200), "a\nb"} {
		if _, _, err := l.Append(s, "c"); err == nil {
			t.Errorf("subject %q accepted", s)
		}
	}
	if _, _, err := l.Append("ok-1", "Bad Class!"); err == nil {
		t.Error("bad class accepted")
	}
}

func TestLedgerDetectsTampering(t *testing.T) {
	l := newLedger(t)
	_, _, _ = l.Append("s1", "c")
	_, _, _ = l.Append("s2", "c")
	raw, _ := os.ReadFile(l.Path)
	for name, mutated := range map[string]string{
		"edit":    strings.Replace(string(raw), `"s1"`, `"s9"`, 1),
		"dropped": strings.SplitAfter(string(raw), "\n")[1],
	} {
		_ = os.WriteFile(l.Path, []byte(mutated), 0o600)
		if _, err := l.Read(); err == nil {
			t.Errorf("%s: tampering not detected", name)
		}
	}
}

func TestLedgerSealOpen(t *testing.T) {
	l := newLedger(t)
	_, _, _ = l.Append("subject-marker-s1", "c")
	es, _ := l.Read()
	sealed, err := SealLedger(es, "restic-password-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "subject-marker-s1") {
		t.Error("plaintext visible")
	}
	got, err := OpenLedger(sealed, "restic-password-1234567890")
	if err != nil || len(got) != 1 || got[0].Subject != "subject-marker-s1" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := OpenLedger(sealed, "another-password-0000000000"); err == nil {
		t.Error("wrong key accepted")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := OpenLedger(sealed, "restic-password-1234567890"); err == nil {
		t.Error("tampered object accepted")
	}
}

func TestMergeLedgers(t *testing.T) {
	a := []Entry{{Subject: "s1", Class: "c", Date: "2026-11-01"}, {Subject: "s2", Class: "c", Date: "2026-11-03"}}
	b := []Entry{{Subject: "s2", Class: "c", Date: "2026-11-02"}, {Subject: "s3", Class: "d", Date: "2026-11-04"}}
	m := MergeLedgers(a, b)
	if len(m) != 3 {
		t.Fatalf("%+v", m)
	}
	if m[1].Subject != "s2" || m[1].Date != "2026-11-02" {
		t.Errorf("earliest date must win: %+v", m[1])
	}
	for i, e := range m {
		if e.Seq != i+1 {
			t.Errorf("seq %d = %d", i, e.Seq)
		}
		if i > 0 && e.Prev != m[i-1].Hash {
			t.Errorf("chain broken at %d", i)
		}
	}
	if err := VerifyChain(m); err != nil {
		t.Fatal(err)
	}
}

func TestSyncLedgerNeverShrinksRemote(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	pw := "restic-password-1234567890"
	remote := MergeLedgers(nil, []Entry{{Subject: "old", Class: "c", Date: "2026-10-01"}})
	sealed, _ := SealLedger(remote, pw)
	_ = store.Put(ctx, LedgerObjectKey, sealed)

	l := newLedger(t)
	_, _, _ = l.Append("new", "c")
	n, err := SyncLedger(ctx, store, l, pw)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	b, _ := store.Get(ctx, LedgerObjectKey)
	got, err := OpenLedger(b, pw)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	// Object lives outside the restic snapshot prefix.
	if strings.HasPrefix(LedgerObjectKey, "restic/") {
		t.Error("ledger key must not be under the restic prefix")
	}
	// A remote that cannot be decrypted must abort, not be overwritten.
	_ = store.Put(ctx, LedgerObjectKey, []byte("garbage"))
	if _, err := SyncLedger(ctx, store, l, pw); err == nil {
		t.Fatal("overwrote an unreadable remote ledger")
	}
}

func TestFetchLedgerMissing(t *testing.T) {
	_, err := FetchLedger(context.Background(), NewMemStore(), "pw-pw-pw-pw-pw-pw-pw")
	if !errors.Is(err, ErrNoLedger) {
		t.Fatalf("err = %v", err)
	}
}

func TestS3StoreSignsAndRoundTrips(t *testing.T) {
	objs := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAFAKE/") || !strings.Contains(auth, "/auto/s3/aws4_request") ||
			!strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") || r.Header.Get("X-Amz-Content-Sha256") == "" {
			t.Errorf("bad signature header: %q", auth)
		}
		if strings.Contains(r.URL.String(), "fakesecret") || strings.Contains(auth, "fakesecret") {
			t.Error("secret leaked")
		}
		switch r.Method {
		case http.MethodPut:
			b := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(b)
			objs[r.URL.Path] = b
		case http.MethodGet:
			b, ok := objs[r.URL.Path]
			if !ok {
				http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		}
	}))
	defer srv.Close()
	s := &S3Store{AllowInsecure: true, Endpoint: srv.URL, Bucket: "b", Region: "auto", AccessKeyID: "AKIAFAKE", SecretAccessKey: "fakesecret", Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	ctx := context.Background()
	if _, err := s.Get(ctx, "ledger/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.Put(ctx, "ledger/x", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "ledger/x")
	if err != nil || string(got) != "hello" || string(objs["/b/ledger/x"]) != "hello" {
		t.Fatalf("%q %v", got, err)
	}
}
