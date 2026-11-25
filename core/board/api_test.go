package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type apiEnv struct {
	*env
	srv *httptest.Server
}

const (
	tokSec   = "tok-sec-0000000000000000000"
	tokD1    = "tok-d1-00000000000000000000"
	tokD2    = "tok-d2-00000000000000000000"
	tokD3    = "tok-d3-00000000000000000000"
	tokEmp   = "tok-emp-0000000000000000000"
	tokAdm   = "tok-adm-0000000000000000000"
	tokInteg = "tok-integ-00000000000000000"
	tokOther = "tok-other-0000000000000000"
)

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	e := newEnv(t).base()
	st := &StaticTokens{}
	st.Add(tokSec, Principal{Subject: e.sec.Subject, Roles: e.sec.Roles, PersonID: e.sec.PersonID})
	st.Add(tokD1, Principal{Subject: e.d1.Subject, Roles: e.d1.Roles, PersonID: e.d1.PersonID})
	st.Add(tokD2, Principal{Subject: e.d2.Subject, Roles: e.d2.Roles, PersonID: e.d2.PersonID})
	st.Add(tokD3, Principal{Subject: e.d3.Subject, Roles: e.d3.Roles, PersonID: e.d3.PersonID})
	st.Add(tokEmp, Principal{Subject: e.emp.Subject, Roles: e.emp.Roles, PersonID: e.emp.PersonID})
	st.Add(tokAdm, Principal{Subject: e.adm.Subject, Roles: e.adm.Roles})
	st.Add(tokInteg, Principal{Subject: e.integ.Subject, Roles: e.integ.Roles})
	st.Add(tokOther, Principal{Subject: "nobody", Roles: []string{"owner", "superuser"}}) // unknown and reserved roles are dropped
	api := &API{Svc: e.s, Auth: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &apiEnv{env: e, srv: srv}
}

func (a *apiEnv) do(tok, method, path string, body any, hdr ...string) (int, []byte, http.Header) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func TestAPIAuthHeadersAndNoOracle(t *testing.T) {
	a := newAPIEnv(t)
	if code, _, _ := a.do("", "GET", "/healthz", nil); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	mt := a.meeting(pD1, pD2, pD3)
	res := a.proposed(mt, a.sec)
	for _, p := range []string{"/v1/bodies", "/v1/resolutions/" + res.ID, "/v1/legal-entities"} {
		if code, _, h := a.do("", "GET", p, nil); code != 401 || h.Get("WWW-Authenticate") == "" {
			t.Errorf("%s without a token = %d", p, code)
		}
		if code, _, _ := a.do("wrong", "GET", p, nil); code != 401 {
			t.Errorf("%s with a wrong token = %d", p, code)
		}
	}
	// a token whose roles are all unknown or reserved has no access
	if code, _, _ := a.do(tokOther, "GET", "/v1/resolutions/"+res.ID, nil); code != 404 {
		t.Errorf("unknown role = %d", code)
	}
	// the answer for an invisible record is byte-identical to the answer for a missing one
	_, missing, h1 := a.do(tokEmp, "GET", "/v1/resolutions/res_ffffffffffffffffffffffff", nil)
	code, hidden, h2 := a.do(tokEmp, "GET", "/v1/resolutions/"+res.ID, nil)
	if code != 404 || !bytes.Equal(missing, hidden) || h1.Get("Content-Type") != h2.Get("Content-Type") {
		t.Errorf("existence oracle: %d %q vs %q", code, hidden, missing)
	}
	_, malformed, _ := a.do(tokEmp, "GET", "/v1/resolutions/whatever", nil)
	if !bytes.Equal(malformed, missing) {
		t.Error("a malformed id answers differently from a missing one")
	}
	// security headers
	_, _, h := a.do(tokSec, "GET", "/v1/bodies", nil)
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") == "" {
		t.Errorf("%v", h)
	}
	// strict bodies
	if code, _, _ := a.do(tokSec, "POST", "/v1/bodies", `{"name":"x","kind":"board","surprise":1}`); code != 400 {
		t.Errorf("unknown field = %d", code)
	}
	if code, _, _ := a.do(tokSec, "POST", "/v1/bodies", `{"name":"x","kind":"board"} {"again":1}`); code != 400 {
		t.Errorf("two values = %d", code)
	}
	if code, _, _ := a.do(tokSec, "POST", "/v1/bodies", strings.Repeat("a", maxBody+10)); code != 400 {
		t.Errorf("huge body = %d", code)
	}
}

func TestValidCredentialsAreNeverThrottledByBadOnes(t *testing.T) {
	a := newAPIEnv(t)
	limited := false
	for i := 0; i < 60; i++ {
		if code, _, _ := a.do("bad-token", "GET", "/v1/bodies", nil); code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("failed authentications were never limited")
	}
	// the same address, immediately after: a valid token still works, every time
	for i := 0; i < 30; i++ {
		if code, _, _ := a.do(tokSec, "GET", "/v1/bodies", nil); code != 200 {
			t.Fatalf("a valid credential was refused (%d) after bad ones from the same address", code)
		}
	}
}

func TestAPIEndToEndFlow(t *testing.T) {
	a := newAPIEnv(t)
	mt := a.meeting(pD1, pD2, pD3)
	var created Resolution
	code, b, _ := a.do(tokD2, "POST", "/v1/bodies/"+a.body.ID+"/resolutions", ResolutionInput{MeetingID: mt.ID, Title: "Sign the lease", Text: "RESOLVED"}, "Idempotency-Key", "k-1")
	if code != 201 {
		t.Fatalf("create = %d %s", code, b)
	}
	_ = json.Unmarshal(b, &created)
	code, _, _ = a.do(tokD2, "POST", "/v1/bodies/"+a.body.ID+"/resolutions", ResolutionInput{MeetingID: mt.ID, Title: "Sign the lease", Text: "RESOLVED"}, "Idempotency-Key", "k-1")
	if code != 200 {
		t.Errorf("replay = %d", code)
	}
	for _, step := range []struct {
		tok, path string
		body      any
		want      int
	}{
		{tokD2, "/v1/resolutions/" + created.ID + "/propose", nil, 200},
		{tokD3, "/v1/resolutions/" + created.ID + "/open-vote", nil, 403}, // not the chair
		{tokD1, "/v1/resolutions/" + created.ID + "/open-vote", nil, 200},
		{tokD1, "/v1/resolutions/" + created.ID + "/vote", VoteInput{Choice: "for"}, 200},
		{tokD3, "/v1/resolutions/" + created.ID + "/vote", VoteInput{Choice: "for"}, 200},
		{tokD3, "/v1/resolutions/" + created.ID + "/vote", VoteInput{Choice: "for"}, 409},
		{tokEmp, "/v1/resolutions/" + created.ID + "/vote", VoteInput{Choice: "for"}, 404},
		{tokD1, "/v1/resolutions/" + created.ID + "/close", nil, 200},
	} {
		if code, b, _ := a.do(step.tok, "POST", step.path, step.body); code != step.want {
			t.Errorf("POST %s = %d %s, want %d", step.path, code, b, step.want)
		}
	}
	code, b, _ = a.do(tokD3, "GET", "/v1/resolutions/"+created.ID, nil)
	var got Resolution
	_ = json.Unmarshal(b, &got)
	if code != 200 || got.Status != ResPassed || got.Result == nil || got.Result.For != 2 {
		t.Errorf("%d %s", code, b)
	}
	code, b, h := a.do(tokD1, "GET", "/v1/bodies/"+a.body.ID+"/resolutions/export.csv", nil)
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") || !strings.Contains(string(b), created.ID) {
		t.Errorf("csv: %d %s", code, b)
	}
	// admin and erasure routes are admin only
	if code, _, _ := a.do(tokSec, "POST", "/v1/people/"+pEm+"/erasure", nil); code != 403 {
		t.Errorf("secretary erasure = %d", code)
	}
	if code, _, _ := a.do(tokAdm, "POST", "/v1/people/"+pEm+"/erasure", nil); code != 200 {
		t.Errorf("admin erasure = %d", code)
	}
	if code, _, _ := a.do(tokAdm, "GET", "/v1/erasures", nil); code != 200 {
		t.Errorf("erasures = %d", code)
	}
}

func TestResolutionSeamForOtherModules(t *testing.T) {
	a := newAPIEnv(t)
	mt := a.meeting(pD1, pD2, pD3, pD4)
	req := ResolutionRequest{Type: "equity.grant", Ref: "grt_0123456789abcdef01234567", BodyID: a.body.ID, Requester: "hr-alice", Subjects: []string{pD4}}
	// only the integration role may ask, and it can do nothing else
	if _, err := a.s.RequestResolution(a.ctx, a.sec, req); !isErr(err, ErrForbidden) {
		t.Errorf("secretary requesting: %v", err)
	}
	o := m(a.s.RequestResolution(a.ctx, a.integ, req))
	if o.Outcome != "pending" || o.Type != "equity.grant" {
		t.Errorf("%+v", o)
	}
	again := m(a.s.RequestResolution(a.ctx, a.integ, req))
	if again.ResolutionID != o.ResolutionID || a.count(`SELECT COUNT(*) FROM resolutions`) != 1 {
		t.Error("the request was not idempotent")
	}
	for name, f := range map[string]func() error{
		"get":     func() error { _, err := a.s.GetResolution(a.ctx, a.integ, o.ResolutionID); return err },
		"minutes": func() error { _, err := a.s.ListMinutes(a.ctx, a.integ, mt.ID); return err },
	} {
		if err := f(); err == nil {
			t.Errorf("the integration role could %s", name)
		}
	}
	if l := m(a.s.ListBodies(a.ctx, a.integ)); len(l) != 0 {
		t.Error("the integration role listed bodies")
	}
	// the secretary completes and runs it; the grantee who sits on the board is recused automatically
	r := m(a.s.GetResolution(a.ctx, a.sec, o.ResolutionID))
	if r.Status != ResDraft || r.ExternalRef != req.Ref {
		t.Fatalf("%+v", r)
	}
	m(a.s.AmendResolution(a.ctx, a.sec, r.ID, AmendInput{Title: "Approve option grant", Text: "RESOLVED that the grant described in the grant record is approved.", MeetingID: mt.ID}))
	m(a.s.ProposeResolution(a.ctx, a.sec, r.ID))
	v := m(a.s.OpenVote(a.ctx, a.d1, r.ID))
	if v.Eligible != 4 {
		t.Errorf("eligible = %d: five members minus the grantee", v.Eligible)
	}
	if _, err := a.s.CastVote(a.ctx, a.d4, r.ID, VoteInput{Choice: "for"}); !isErr(err, ErrForbidden) {
		t.Errorf("the grantee voting: %v", err)
	}
	m(a.s.CastVote(a.ctx, a.d1, r.ID, VoteInput{Choice: "for"}))
	m(a.s.CastVote(a.ctx, a.d2, r.ID, VoteInput{Choice: "for"}))
	if got := m(a.s.OutcomeByRef(a.ctx, a.integ, "equity.grant", req.Ref)); got.Outcome != "pending" {
		t.Errorf("%+v", got)
	}
	m(a.s.CloseVote(a.ctx, a.d1, r.ID))
	got := m(a.s.OutcomeByRef(a.ctx, a.integ, "equity.grant", req.Ref))
	if got.Outcome != "approved" || got.DecidedOn != "2026-03-10" {
		t.Errorf("%+v", got)
	}
	// the event carries the external type and reference so the requester can correlate; nothing else
	for _, ev := range m(a.s.AllEvents(a.ctx)) {
		if ev.Type == EventResolutionPassed && (ev.Data["external_type"] != "equity.grant" || ev.Data["external_ref"] != req.Ref) {
			t.Errorf("%v", ev.Data)
		}
	}
	// over HTTP: the outcome route belongs to the integration role only
	if code, _, _ := a.do(tokSec, "GET", "/v1/resolution-outcomes?type=equity.grant&ref="+req.Ref, nil); code != 403 {
		t.Errorf("secretary reading outcomes = %d", code)
	}
	if code, _, _ := a.do(tokInteg, "GET", "/v1/resolution-outcomes?type=equity.grant&ref="+req.Ref, nil); code != 200 {
		t.Errorf("integration reading outcomes = %d", code)
	}
	if code, _, _ := a.do(tokInteg, "GET", "/v1/resolution-outcomes?type=equity.grant&ref=unknown", nil); code != 404 {
		t.Errorf("unknown ref = %d", code)
	}
	// a withdrawn request is "cancelled", and the same reference can be requested again
	req2 := ResolutionRequest{Type: "equity.grant", Ref: "grt_aaaaaaaaaaaaaaaaaaaaaaaa", BodyID: a.body.ID, Requester: "hr-alice"}
	o2 := m(a.s.RequestResolution(a.ctx, a.integ, req2))
	m(a.s.WithdrawResolution(a.ctx, a.sec, o2.ResolutionID, ""))
	if got := m(a.s.OutcomeByRef(a.ctx, a.integ, "equity.grant", req2.Ref)); got.Outcome != "cancelled" {
		t.Errorf("%+v", got)
	}
	// the requester cannot be the sole approver: a failed tally gives "rejected"
	req3 := ResolutionRequest{Type: "equity.grant", Ref: "grt_bbbbbbbbbbbbbbbbbbbbbbbb", BodyID: a.body.ID, Requester: "sso-d2"}
	o3 := m(a.s.RequestResolution(a.ctx, a.integ, req3))
	m(a.s.AmendResolution(a.ctx, a.sec, o3.ResolutionID, AmendInput{Title: "t", Text: "x", MeetingID: mt.ID}))
	m(a.s.ProposeResolution(a.ctx, a.sec, o3.ResolutionID))
	m(a.s.OpenVote(a.ctx, a.sec, o3.ResolutionID))
	m(a.s.CastVote(a.ctx, a.d2, o3.ResolutionID, VoteInput{Choice: "for"})) // the requester's own vote
	m(a.s.CastVote(a.ctx, a.d1, o3.ResolutionID, VoteInput{Choice: "abstain"}))
	m(a.s.CloseVote(a.ctx, a.sec, o3.ResolutionID))
	if got := m(a.s.OutcomeByRef(a.ctx, a.integ, "equity.grant", req3.Ref)); got.Outcome != "rejected" {
		t.Errorf("%+v", got)
	}
}

func TestRoutesAreAllDocumentedAndAuthenticated(t *testing.T) {
	a := newAPIEnv(t)
	api := &API{Svc: a.s, Auth: &StaticTokens{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_ = api.Handler()
	if len(api.Routes()) < 60 {
		t.Errorf("%d routes", len(api.Routes()))
	}
	h := api.Handler()
	for i, r := range api.Routes() {
		if r == "GET /healthz" {
			continue
		}
		method, path, _ := strings.Cut(r, " ")
		path = strings.NewReplacer("{id}", "x", "{mid}", "x", "{person}", "x", "{register}", "directors").Replace(path)
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.RemoteAddr = fmt.Sprintf("10.1.%d.%d:4000", i/200, i%200) // one address per request: the failed-auth limiter is per address
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s without a token = %d", r, rec.Code)
		}
	}
}
