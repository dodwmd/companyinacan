package adminapi

import (
	"context"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// A token carrying equity.board needs a recent sign-in (specs/068-equity-board-role).
func TestBoardTokenNeedsARecentSignIn(t *testing.T) {
	h := newPHarness(t)
	m := store.Module{ID: "equity", Version: "1", Namespace: "mod-equity", ServiceAccount: "equity", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "equity", Permissions: []string{"equity.board", "equity.read"}}}
	if err := h.db.UpsertModule(context.Background(), m, nil, nil); err != nil {
		t.Fatal(err)
	}
	root := h.login("root")
	issue := func(perm string) (int, string) {
		return h.call(root, "POST", "/api/v1/tokens", map[string]any{"module": "equity", "name": "t", "permissions": []string{perm}, "ttl_hours": 24})
	}
	if code, body := issue("equity.board"); code != 201 {
		t.Fatalf("right after signing in = %d %s", code, body)
	}
	*h.now = h.now.Add(11 * time.Minute)
	if code, body := issue("equity.board"); code != 403 {
		t.Fatalf("with an old sign-in = %d %s", code, body)
	}
	if code, body := issue("equity.read"); code != 201 {
		t.Fatalf("another permission with an old sign-in = %d %s", code, body)
	}
}
