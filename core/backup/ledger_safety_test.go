package backup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestLedgerToleratesTornLastLine(t *testing.T) {
	l := newLedger(t)
	_, _, _ = l.Append("s1", "c")
	f, _ := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"seq":2,"subject":"s2","cla`) // crash mid-append, no newline
	_ = f.Close()
	es, err := l.Read()
	if err != nil || len(es) != 1 {
		t.Fatalf("%v %+v", err, es)
	}
	e, added, err := l.Append("s3", "c")
	if err != nil || !added || e.Seq != 2 {
		t.Fatalf("append after torn tail: %+v %v %v", e, added, err)
	}
	if es, err := l.Read(); err != nil || len(es) != 2 || es[1].Subject != "s3" {
		t.Fatalf("%v %+v", err, es)
	}
	// A malformed line that is NOT the last one is corruption, not a torn write.
	_ = os.WriteFile(l.Path, []byte("garbage\n"+`{"seq":1}`+"\n"), 0o600)
	if _, err := l.Read(); err == nil {
		t.Error("corrupt middle line accepted")
	}
}

func TestCheckNotBehind(t *testing.T) {
	snap := MergeLedgers(nil, []Entry{{Subject: "a", Class: "c", Date: "2026-11-01"}, {Subject: "b", Class: "c", Date: "2026-11-02"}})
	bucketOld := MergeLedgers(nil, []Entry{{Subject: "a", Class: "c", Date: "2026-11-01"}})
	if _, err := CheckNotBehind(bucketOld, snap); err == nil || !strings.Contains(err.Error(), "b/c") {
		t.Fatalf("rolled-back bucket accepted: %v", err)
	}
	bucketNew := MergeLedgers(nil, []Entry{{Subject: "a", Class: "c", Date: "2026-11-01"}, {Subject: "b", Class: "c", Date: "2026-11-02"}, {Subject: "z", Class: "c", Date: "2026-11-09"}})
	u, err := CheckNotBehind(bucketNew, snap)
	if err != nil || len(u) != 3 {
		t.Fatalf("%v %+v", err, u)
	}
	if u, err := CheckNotBehind(bucketNew, nil); err != nil || len(u) != 3 {
		t.Fatalf("empty snapshot copy: %v %+v", err, u)
	}
}

func TestS3RefusesPlainHTTPByDefault(t *testing.T) {
	s := &S3Store{Endpoint: "http://s3.example.invalid", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}
	if _, err := s.Get(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
}

func TestS3Only404NoSuchKeyIsNotFound(t *testing.T) {
	body := "<Error><Code>NoSuchBucket</Code></Error>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, http.StatusNotFound)
	}))
	defer srv.Close()
	s := &S3Store{AllowInsecure: true, Endpoint: srv.URL, Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}
	_, err := s.Get(context.Background(), "x")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing bucket must not look like 'no ledger': %v", err)
	}
	// And a ledger fetch therefore fails instead of reporting ErrNoLedger.
	if _, err := FetchLedger(context.Background(), s, "pw-pw-pw-pw-pw-pw-pw"); errors.Is(err, ErrNoLedger) || err == nil {
		t.Fatalf("err = %v", err)
	}
}

func TestS3KeepsEndpointPathPrefix(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	s := &S3Store{AllowInsecure: true, Endpoint: srv.URL + "/s3/v2/", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}
	if _, err := s.Get(context.Background(), "ledger/x"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/s3/v2/b/ledger/x" {
		t.Errorf("path = %q", gotPath)
	}
}
