package people

import (
	"net/http"
	"strings"
	"testing"
)

func newToAPI(t *testing.T) (*apiEnv, *toEnv) {
	x := newToEnv(t)
	st := &StaticTokens{}
	st.Add(hrTok, Principal{Subject: "svc-hr", Roles: []string{RoleHR, RoleAdmin}})
	e := &apiEnv{env: x.env, st: st}
	srvAPI := &API{Svc: x.s, Auth: st, Log: newTestLogger()}
	e.srv = newTestServer(t, srvAPI)
	return e, x
}

func TestAPIReviewsAndLeaveEndToEnd(t *testing.T) {
	e, x := newToAPI(t)
	ann, mgr, ben := e.token("ann", x.annA), e.token("mgr", x.mgrA), e.token("ben", x.benA)

	// Reviews over HTTP.
	code, tpl, _ := e.do(hrTok, "POST", "/v1/review-templates", map[string]any{"name": "t", "questions": testQuestions})
	if code != 201 {
		t.Fatalf("template: %d %v", code, tpl)
	}
	code, cy, _ := e.do(hrTok, "POST", "/v1/review-cycles", map[string]any{"name": "c", "template_id": tpl["id"], "anonymity": "anonymous"})
	if code != 201 {
		t.Fatalf("cycle: %d %v", code, cy)
	}
	cid := cy["id"].(string)
	code, part, _ := e.do(hrTok, "POST", "/v1/review-cycles/"+cid+"/participants", map[string]any{"person_ids": []string{x.ann.ID}})
	if code != 200 {
		t.Fatalf("participants: %d %v", code, part)
	}
	rid := part["review_ids"].([]any)[0].(string)
	if code, _, _ := e.do(hrTok, "POST", "/v1/review-cycles/"+cid+"/state", map[string]any{"state": "open"}); code != 200 {
		t.Fatalf("open: %d", code)
	}
	// 404 for someone who may not know it exists, 403 for someone who does but may not act.
	if code, _, _ := e.do(ben, "GET", "/v1/reviews/"+rid, nil); code != 404 {
		t.Errorf("colleague GET = %d, want 404", code)
	}
	if code, _, _ := e.do(ann, "PUT", "/v1/reviews/"+rid+"/manager", map[string]any{"rating": 3}); code != 403 {
		t.Errorf("subject writing the manager review = %d, want 403", code)
	}
	if code, _, _ := e.do(ben, "PUT", "/v1/reviews/"+rid+"/self", map[string]any{"submit": true}); code != 404 {
		t.Errorf("colleague writing = %d, want 404", code)
	}
	if code, _, body := e.do(ann, "PUT", "/v1/reviews/"+rid+"/self", map[string]any{"answers": map[string]any{"q1": map[string]any{"rating": 4}}, "bogus": 1}); code != 400 {
		t.Errorf("unknown field = %d (%s)", code, body)
	}
	if code, _, body := e.do(ann, "PUT", "/v1/reviews/"+rid+"/self", map[string]any{"answers": map[string]any{"q1": map[string]any{"rating": 4}}, "submit": true}); code != 204 {
		t.Fatalf("self = %d (%s)", code, body)
	}
	// No validators on review responses.
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/reviews/"+rid, nil)
	req.Header.Set("Authorization", "Bearer "+mgr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("ETag") != "" || resp.Header.Get("Last-Modified") != "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v status %d", resp.Header, resp.StatusCode)
	}
	// Malformed ids are not found.
	if code, _, _ := e.do(hrTok, "GET", "/v1/reviews/..%2f..", nil); code != 404 {
		t.Errorf("malformed id = %d", code)
	}

	// Leave over HTTP.
	typeID := x.vac.ID
	code, lr, _ := e.do(ann, "POST", "/v1/leave-requests", map[string]any{"type_id": typeID, "start_date": "2026-04-06", "end_date": "2026-04-07", "note": "trip"})
	if code != 201 {
		t.Fatalf("create: %d %v", code, lr)
	}
	lid := lr["id"].(string)
	if code, _, _ := e.do(ann, "POST", "/v1/leave-requests/"+lid+"/submit", nil); code != 200 {
		t.Fatalf("submit: %d", code)
	}
	if code, _, _ := e.do(ben, "GET", "/v1/leave-requests/"+lid, nil); code != 404 {
		t.Errorf("colleague GET leave = %d, want 404", code)
	}
	if code, _, _ := e.do(ann, "POST", "/v1/leave-requests/"+lid+"/decision", map[string]any{"approve": true}); code != 403 {
		t.Errorf("self-approval = %d, want 403", code)
	}
	if code, _, _ := e.do(mgr, "POST", "/v1/leave-requests/"+lid+"/decision", map[string]any{"approve": true}); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	code, bal, _ := e.do(ann, "GET", "/v1/people/"+x.ann.ID+"/leave-balances", nil)
	if code != 200 || len(bal["items"].([]any)) == 0 {
		t.Fatalf("balances: %d %v", code, bal)
	}
	if code, _, _ := e.do(ben, "GET", "/v1/people/"+x.ann.ID+"/leave-balances", nil); code != 404 {
		t.Errorf("colleague balances = %d, want 404", code)
	}
	if code, _, _ := e.do(ann, "POST", "/v1/timeoff/accruals/run", nil); code != 403 {
		t.Errorf("employee accrual run = %d", code)
	}
	if code, _, _ := e.do(hrTok, "POST", "/v1/timeoff/accruals/run", nil); code != 200 {
		t.Errorf("hr accrual run = %d", code)
	}
	code, cal, body := e.do(ben, "GET", "/v1/timeoff/calendar?from=2026-04-01&to=2026-04-30&team_id=all", nil)
	if code != 200 {
		t.Fatalf("calendar: %d %s", code, body)
	}
	if strings.Contains(body, "vacation") || strings.Contains(body, "trip") {
		t.Fatalf("calendar leaks: %s", body)
	}
	_ = cal

	// Unauthenticated.
	for _, p := range []string{"/v1/reviews", "/v1/leave-requests", "/v1/timeoff/calendar", "/v1/feedback-requests"} {
		if code, _, _ := e.do("", "GET", p, nil); code != 401 {
			t.Errorf("%s without a token = %d", p, code)
		}
	}

	// Valid credentials are never throttled by failed ones from the same address.
	for i := 0; i < 60; i++ {
		e.do("bad-token", "GET", "/v1/reviews", nil)
	}
	if code, _, _ := e.do(ann, "GET", "/v1/reviews", nil); code != 200 {
		t.Errorf("valid token refused after failed attempts: %d", code)
	}
}
