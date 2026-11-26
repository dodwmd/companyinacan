package users

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func init() { auth.TestingParams() }

// What matters is how many passwords are compared, not only what the answers say: requests sent side
// by side must not get more than the limit compared.
func TestLinkComparesNoMorePasswordsThanItsLimit(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	hash, err := auth.HashPassword("the account password")
	if err != nil {
		t.Fatal(err)
	}
	root, err := db.CreateAdmin(ctx, "root", hash, auth.NewTOTPSecret())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAdmin(ctx, "dave", hash, auth.NewTOTPSecret()); err != nil {
		t.Fatal(err)
	}
	al := audit.New(db)
	var compared atomic.Int32
	release := make(chan struct{})
	s := &Service{DB: db, Audit: al, Auth: auth.NewService(db, al, auth.Options{}), Now: func() time.Time { return now }}
	// No comparison finishes until every request has had the chance to start one.
	s.compare = func(context.Context, string, string) (bool, error) {
		compared.Add(1)
		<-release
		return false, nil
	}
	l, err := s.SendLink(ctx, Actor{User: root, Session: store.Session{CreatedAt: now}}, "dave", store.LinkMFA, "", false)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(l.Path, SetupPath+"?token=")
	setup, err := s.OpenLink(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.DecodeTOTPSecret(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	code := auth.Code(key, auth.SHA1, 6, 30, now)

	const n = 80
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.UseLink(ctx, token, "a guess at the password", code, "10.0.0.1")
		}()
	}
	// Every request that is not comparing has been refused once n-limit answers are in.
	for range n - maxLinkFailures {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrBadLink) {
				t.Errorf("refused request: %v", err)
			}
		case <-time.After(20 * time.Second):
			close(release)
			t.Fatalf("%d passwords were being compared at once, want %d", compared.Load(), maxLinkFailures)
		}
	}
	close(release)
	wg.Wait()
	if got := compared.Load(); got != maxLinkFailures {
		t.Fatalf("%d passwords compared, want %d", got, maxLinkFailures)
	}
}

// An attempt whose password was never compared (hashing was busy) is given back.
func TestLinkAttemptIsReturnedWhenNothingWasCompared(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hash, _ := auth.HashPassword("the account password")
	root, err := db.CreateAdmin(ctx, "root", hash, auth.NewTOTPSecret())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAdmin(ctx, "dave", hash, auth.NewTOTPSecret()); err != nil {
		t.Fatal(err)
	}
	al := audit.New(db)
	s := &Service{DB: db, Audit: al, Auth: auth.NewService(db, al, auth.Options{})}
	s.compare = func(context.Context, string, string) (bool, error) { return false, auth.ErrBusy }
	l, err := s.SendLink(ctx, Actor{User: root, Session: store.Session{CreatedAt: db.Now()}}, "dave", store.LinkMFA, "", false)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(l.Path, SetupPath+"?token=")
	setup, err := s.OpenLink(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := auth.DecodeTOTPSecret(setup.Secret)
	for range 3 * maxLinkFailures {
		if err := s.UseLink(ctx, token, "the account password", auth.Code(key, auth.SHA1, 6, 30, db.Now()), "10.0.0.1"); !errors.Is(err, auth.ErrBusy) {
			t.Fatalf("busy: %v", err)
		}
	}
	s.compare = nil
	if err := s.UseLink(ctx, token, "the account password", auth.Code(key, auth.SHA1, 6, 30, db.Now()), "10.0.0.1"); err != nil {
		t.Fatalf("a valid password after busy answers: %v", err)
	}
}
