package provision

import (
	"fmt"
	"sync"
	"testing"
)

// Writes need a version by default: a stale copy cannot overwrite a newer change.
func TestSCIMIfMatchIsRequiredByDefault(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	e.scim.Cfg.RequireIfMatch = nil // what a config that does not mention it yields
	resp, m, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "A"}))
	e.wantError(resp, m, 428, "")
	resp, g, _ := e.do("POST", "/scim/v2/Groups", e.write, "", map[string]any{"displayName": "G"})
	if resp.StatusCode != 201 {
		t.Fatalf("create %d", resp.StatusCode)
	}
	resp, m, _ = e.do("DELETE", "/scim/v2/Groups/"+g["id"].(string), e.write, "", nil)
	e.wantError(resp, m, 428, "")
	resp, u, _ := e.do("GET", "/scim/v2/Users/"+alice, e.read, "", nil)
	etag := resp.Header.Get("ETag")
	_ = u
	if resp, _, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "A"}), "If-Match", etag); resp.StatusCode != 200 {
		t.Fatalf("with the version: %d", resp.StatusCode)
	}
}

// Two concurrent PATCHes with the same version: exactly one wins, the other gets 412.
func TestSCIMConcurrentPatchesWithOneVersionHaveOneWinner(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{RequireIfMatch: boolPtr(true)})
	resp, _, _ := e.do("GET", "/scim/v2/Users/"+alice, e.read, "", nil)
	etag := resp.Header.Get("ETag")
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": fmt.Sprintf("Name %d", i)}), "If-Match", etag)
			codes[i] = r.StatusCode
		}()
	}
	wg.Wait()
	ok, stale := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 412:
			stale++
		}
	}
	if ok != 1 || stale != len(codes)-1 || len(e.dir.updates) != 1 {
		t.Fatalf("codes %v, directory updates %d", codes, len(e.dir.updates))
	}
}

// A directory bigger than max_scan can still be searched by login: userName eq stops at the match.
func TestSCIMUserNameLookupWorksInALargeDirectory(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{MaxScan: 150})
	for i := 0; i < 400; i++ {
		e.dir.put(Person{ID: fmt.Sprintf("pers_%024x", 0x1000+i), Kind: "employee", Status: "active", GivenName: "X", FamilyName: "Y", WorkEmail: fmt.Sprintf("x%d@example.com", i)})
	}
	_, m, raw := e.do("GET", "/scim/v2/Users?filter="+urlQ(`userName eq "x399@example.com"`), e.read, "", nil)
	if m["totalResults"] != float64(1) {
		t.Fatalf("lookup by userName: %s", raw)
	}
	_, m, _ = e.do("GET", "/scim/v2/Users?filter="+urlQ(`emails.value eq "x300@example.com"`), e.read, "", nil)
	if m["totalResults"] != float64(1) {
		t.Fatalf("lookup by email: %v", m)
	}
	// a non-unique filter on the same directory is still refused
	resp, em, _ := e.do("GET", "/scim/v2/Users?filter="+urlQ(`name.familyName eq "Y"`), e.read, "", nil)
	e.wantError(resp, em, 400, "tooMany")
}
