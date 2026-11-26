package people

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestUIConcurrentRequestsInOneSession(t *testing.T) {
	u := newUIEnv(t)
	_, hr := u.person("Hana", "", RoleEmployee, RoleHR)
	u.ui.RecheckEvery = time.Nanosecond // every request re-authenticates and writes the session
	c := u.login(t, hr)
	cookie := c.cookies[u.ui.sessions.SessionCookieName(nil)]
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				r := httptest.NewRequest("GET", "/", nil)
				r.AddCookie(&http.Cookie{Name: u.ui.sessions.SessionCookieName(nil), Value: cookie})
				u.h.ServeHTTP(httptest.NewRecorder(), r)
			}
		}()
	}
	wg.Wait()
}

func TestUIRevealsOfOthersAreLimited(t *testing.T) {
	u := newUIEnv(t)
	_, hr := u.person("Hana", "", RoleEmployee, RoleHR)
	ann, _ := u.person("Ann", "", RoleEmployee)
	c := u.login(t, hr)
	got429 := false
	for i := 0; i < 30; i++ {
		if c.post("/people/"+ann.ID+"/sensitive", nil).Code == 429 {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("revealing other people's data is not rate limited")
	}
	// one's own reveals are not subject to it
	_, annA := u.person("Bea", "", RoleEmployee)
	cb := u.login(t, annA)
	for i := 0; i < 15; i++ {
		if w := cb.post("/me/sensitive", nil); w.Code == 429 {
			t.Fatal("own reveal limited")
		}
	}
}

func TestUIHRSessionsRecheckFaster(t *testing.T) {
	u := newUIEnv(t)
	if d := u.ui.recheckFor(Principal{Roles: []string{RoleHR}}); d != time.Minute {
		t.Fatalf("hr recheck = %v", d)
	}
	if d := u.ui.recheckFor(Principal{Roles: []string{RoleEmployee}}); d != defaultRecheck {
		t.Fatalf("employee recheck = %v", d)
	}
}

func TestUILeaveTypeShownByName(t *testing.T) {
	x := newToEnv(t)
	u := newUIFor(x.env)
	lr := x.submit(x.annA, x.vac, "2026-03-23", "2026-03-24")
	c := u.login(t, x.annA)
	for _, p := range []string{"/timeoff", "/timeoff/" + lr.ID} {
		w := c.get(p)
		mustHave(t, w, "Vacation")
	}
	mustNot(t, c.get("/timeoff/"+lr.ID), ">vacation<")
}
