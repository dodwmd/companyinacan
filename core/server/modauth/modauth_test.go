package modauth

import (
	"context"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

func setup(t *testing.T) (*Authenticator, *tokenreview.Fake, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := tokenreview.NewFake()
	return New(f, db), f, db
}

func TestAuthenticateMapsServiceAccountToInstalledModule(t *testing.T) {
	ctx := context.Background()
	a, f, db := setup(t)
	_ = db.UpsertModule(ctx, store.Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: store.StateEnabled}, nil, nil)
	f.Add("good", "mod-crm", "crm")
	m, err := a.Authenticate(ctx, "good")
	if err != nil || m.ID != "crm" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestAuthenticateFailures(t *testing.T) {
	ctx := context.Background()
	a, f, db := setup(t)
	_ = db.UpsertModule(ctx, store.Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: store.StateEnabled}, nil, nil)
	_ = db.UpsertModule(ctx, store.Module{ID: "old", Version: "1", Namespace: "mod-old", ServiceAccount: "old", State: store.StateDisabled}, nil, nil)
	f.Add("good", "mod-crm", "crm")
	f.Add("stranger", "default", "default") // valid SA, not an installed module
	f.Add("sameNsOtherSA", "mod-crm", "other")
	f.Add("disabled", "mod-old", "old")

	for _, tc := range []struct {
		token string
		want  error
	}{
		{"", ErrUnauthenticated},
		{"bogus", ErrUnauthenticated},
		{"stranger", ErrUnauthenticated},
		{"sameNsOtherSA", ErrUnauthenticated},
		{"disabled", ErrForbidden},
	} {
		if _, err := a.Authenticate(ctx, tc.token); !errors.Is(err, tc.want) {
			t.Errorf("token %q: got %v want %v", tc.token, err, tc.want)
		}
	}
	if f.Calls != 4 { // the empty token never reaches the reviewer
		t.Errorf("reviewer calls = %d", f.Calls)
	}
}
