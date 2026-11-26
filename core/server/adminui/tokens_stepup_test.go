package adminui

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// A token carrying equity.board needs a recent sign-in (specs/068-equity-board-role).
func TestBoardTokenNeedsARecentSignIn(t *testing.T) {
	p := newTokenUI(t)
	m := store.Module{ID: "equity", Version: "1", Namespace: "mod-equity", ServiceAccount: "equity", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "equity", Permissions: []string{"equity.board", "equity.read"}}}
	if err := p.db.UpsertModule(context.Background(), m, nil, nil); err != nil {
		t.Fatal(err)
	}
	root := p.loginAs("root")
	issue := func(perm string) resp {
		return p.do("POST", "/tokens", url.Values{"csrf": {root.csrf}, "module": {"equity"}, "name": {"t"}, "permissions": {perm}, "ttl_hours": {"24"}}, root.with()...)
	}
	if r := issue("equity.board"); r.StatusCode != 200 || !patTokenRe.MatchString(r.body) {
		t.Fatalf("right after signing in = %d", r.StatusCode)
	}
	*p.now = p.now.Add(11 * time.Minute)
	if r := issue("equity.board"); r.StatusCode != 303 || r.Header.Get("Location") != "/reauth?next=/tokens" {
		t.Fatalf("with an old sign-in = %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := issue("equity.read"); r.StatusCode != 200 {
		t.Fatalf("another permission with an old sign-in = %d", r.StatusCode)
	}
	if v, err := p.ui.PATs.ListFor(context.Background(), "user:root", false); err != nil || len(v) != 2 {
		t.Fatalf("tokens %d %v, want 2", len(v), err)
	}
}
