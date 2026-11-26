package people

import (
	"net/http"
	"strings"
	"testing"
)

// The credential can-core copies to the provisioning module reads and writes the directory and
// nothing that erases, holds or lists erasures.
func TestDirectoryTokenIsLimited(t *testing.T) {
	st := &StaticTokens{}
	tok := strings.Repeat("d1r3ct0ry", 4)
	addDirectoryToken(st, tok)
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	p, err := st.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	a := Actor{Subject: p.Subject, Roles: p.Roles, Perms: p.Perms, Restricted: p.Perms != nil}
	for act, want := range map[Action]bool{ActDirRead: true, ActDirUpdate: true, ActErasure: false, ActLegalHold: false, ActRetention: false, ActRevCalibRead: false, ActLeaveRead: false} {
		if got := Decide(a, act, Fact{}); got != want {
			t.Errorf("%s = %v, want %v", act, got, want)
		}
	}
}
