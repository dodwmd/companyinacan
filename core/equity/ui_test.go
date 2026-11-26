package equity

import (
	"encoding/csv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// All names and ids are fake. The UI is exercised through its real handler over the same Service
// the JSON API uses.

type uiEnv struct {
	*env
	st *StaticTokens
	ui *UI
	h  http.Handler
}

func newUIEnv(t *testing.T, opts ...optFn) *uiEnv {
	t.Helper()
	e := newEnv(t, opts...)
	u := &uiEnv{env: e, st: &StaticTokens{}}
	u.ui = &UI{Svc: e.s, Auth: MultiAuth{u.st}, Now: func() time.Time { return e.clock }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	u.h = u.ui.Handler()
	return u
}

func uiToken(a Actor) string { return "pat-" + a.Subject + "-0123456789abcdef" }

func (u *uiEnv) token(a Actor) string {
	u.st.Add(uiToken(a), Principal{Subject: a.Subject, Roles: a.Roles, PersonID: a.PersonID})
	return uiToken(a)
}

type client struct {
	t       *testing.T
	u       *uiEnv
	cookies map[string]string
	csrf    string
	tok     string
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (u *uiEnv) client(t *testing.T) *client {
	return &client{t: t, u: u, cookies: map[string]string{}}
}

func (c *client) do(method, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "192.0.2.10:1234"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	for k, v := range c.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	c.u.h.ServeHTTP(w, r)
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	return w
}

// get expects a status.
func (c *client) get(path string, want int) string {
	c.t.Helper()
	w := c.do("GET", path, nil)
	if w.Code != want {
		c.t.Fatalf("GET %s = %d, want %d: %.1500s", path, w.Code, want, w.Body)
	}
	return w.Body.String()
}

// post adds the session's CSRF token and expects a status.
func (c *client) post(path string, want int, kv ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	form := url.Values{"csrf": {c.csrf}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	w := c.do("POST", path, form, "Sec-Fetch-Site", "same-origin")
	if w.Code != want {
		c.t.Fatalf("POST %s = %d, want %d: %.1500s", path, w.Code, want, w.Body)
	}
	return w
}

// confirm is post for an irreversible action: the box ticked and the token typed again.
func (c *client) confirm(path string, want int, kv ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.post(path, want, append(kv, "confirm", "yes", "token", c.tok)...)
}

func (u *uiEnv) login(t *testing.T, a Actor) *client {
	t.Helper()
	c := u.client(t)
	c.tok = u.token(a)
	m := csrfRe.FindStringSubmatch(c.get("/login", 200))
	w := c.do("POST", "/login", url.Values{"token": {c.tok}, "csrf": {m[1]}}, "Sec-Fetch-Site", "same-origin")
	if w.Code != 303 {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	c.csrf = csrfRe.FindStringSubmatch(c.get("/", 200))[1]
	return c
}

func has(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Fatalf("page lacks %q: %s", w, body)
		}
	}
}

func lacks(t *testing.T, body string, not ...string) {
	t.Helper()
	for _, w := range not {
		if strings.Contains(body, w) {
			t.Fatalf("page must not show %q: %s", w, body)
		}
	}
}

func location(w *httptest.ResponseRecorder) string {
	l, _, _ := strings.Cut(w.Header().Get("Location"), "?")
	return l
}

func newGrantForm(plan, holder string) []string {
	return []string{"plan_id", plan, "stakeholder_id", holder, "type", "NSO", "quantity", "4800", "strike", "1.25", "grant_date", "2026-01-15",
		"vesting_start", "2026-01-15", "expiry_date", "2036-01-15", "months", "48", "cliff_months", "12", "interval", "monthly", "rounding", "floor",
		"ptep_days", "90", "notes", "board minute 14", "idem", "once"}
}

// The whole chain through the pages: draft, submit, approve, activate, request, approve, pay,
// issue; separation of duties and step-up on the way; exact figures.
func TestUIGrantAndExerciseChain(t *testing.T) {
	u := newUIEnv(t)
	u.base()
	hr, board, eve := u.login(t, u.hr), u.login(t, u.board2), u.login(t, u.eve)

	has(t, hr.get("/company/entities/"+u.ent.ID+"/grants/new", 200), "Eve Example", "2026 Plan")
	w := hr.post("/company/entities/"+u.ent.ID+"/grants", 303, newGrantForm(u.plan.ID, u.eveStk.ID)...)
	grant := location(w)
	// the same form sent again is the same grant, not a second one
	if again := location(hr.post("/company/entities/"+u.ent.ID+"/grants", 303, newGrantForm(u.plan.ID, u.eveStk.ID)...)); again != grant {
		t.Fatalf("a replay created %s beside %s", again, grant)
	}
	id := grant[strings.LastIndex(grant, "/")+1:]
	has(t, hr.get(grant, 200), "Draft", "4,800", "1.25 USD", "board minute 14")
	// a holder sees nothing of a draft
	eve.get("/me/grants/"+id, 404)
	hr.post(grant+"/submit", 303)
	has(t, hr.get(grant, 200), "Waiting for approval", "The board has not decided")
	// hr is not the board: no form, and the verb is refused by the Service
	lacks(t, hr.get(grant, 200), `action="`+grant+`/approve"`)
	hr.confirm(grant+"/approve", 403)

	has(t, board.get(grant, 200), `action="`+grant+`/approve"`)
	// step-up: the box and the token, or nothing happens
	board.post(grant+"/approve", 400, "token", board.tok)
	board.post(grant+"/approve", 400, "confirm", "yes", "token", "pat-somebody-else-0123456789abcdef")
	has(t, board.get(grant, 200), "Waiting for approval")
	has(t, board.confirm(grant+"/approve", 200).Body.String(), "Grant approved.", "Approved by the board")
	has(t, hr.confirm(grant+"/activate", 200).Body.String(), "Grant activated.")

	u.advance("2027-07-15")
	hr, eve = u.login(t, u.hr), u.login(t, u.eve) // the old sessions are past their lifetime
	fin, fin2 := u.login(t, u.fin), u.login(t, u.fin2)
	page := eve.get("/me/grants/"+id, 200)
	has(t, page, "1,800", "3,000", "1.25 USD", "Cliff", "not legal, tax or securities advice")
	lacks(t, page, "board minute 14")
	has(t, eve.get("/me", 200), "1,800", "3,000")
	has(t, eve.get("/me/documents", 200), "/me/grants/"+id+"/statement")
	has(t, eve.get("/me/grants/"+id+"/statement", 200), "4,800", "Grant statement")

	review := eve.post("/me/grants/"+id+"/exercise/review", 200, "quantity", "1000.5", "idem", "ex1").Body.String()
	has(t, review, "1,000.5", "1,250.63 USD") // 1000.5 x 1.25 = 1250.625, half away from zero
	eve.post("/me/grants/"+id+"/exercise/review", 409, "quantity", "1801")
	eve.post("/me/grants/"+id+"/exercise/review", 400, "quantity", "1e3")
	eve.post("/me/grants/"+id+"/exercise", 400, "quantity", "1000.5", "idem", "ex1") // not accepted
	eve.post("/me/grants/"+id+"/exercise", 303, "quantity", "1000.5", "idem", "ex1", "accept", "yes")
	eve.post("/me/grants/"+id+"/exercise", 303, "quantity", "1000.5", "idem", "ex1", "accept", "yes")
	ex := m(u.s.MyExercises(u.ctx, u.eve))
	if len(ex) != 1 {
		t.Fatalf("a resent request made %d exercises", len(ex))
	}
	path := "/company/exercises/" + ex[0].ID
	eve.get(path, 403) // the company page is not the holder's
	eve.post(path+"/approve", 403)

	has(t, fin.get("/company/entities/"+u.ent.ID+"/exercises", 200), "Eve Example", "1,250.63 USD")
	fin.post(path+"/approve", 303)
	has(t, fin.get(path, 200), `value="1250.63"`)
	fin.confirm(path+"/payment", 409, "amount", "1250.62", "paid_on", "2027-07-15")
	fin.confirm(path+"/payment", 400, "amount", "1250.625", "paid_on", "2027-07-15")
	has(t, fin.confirm(path+"/payment", 200, "amount", "1250.63", "paid_on", "2027-07-15", "reference", "WIRE-7").Body.String(), "Payment recorded.", "WIRE-7")
	// whoever recorded the payment cannot issue
	has(t, fin.confirm(path+"/issue", 403).Body.String(), "Separation of duties")
	has(t, fin2.confirm(path+"/issue", 200).Body.String(), "Shares issued.")

	has(t, eve.get("/me", 200), "Shares issued", "1,000.5")
	cap := fin.get("/company/entities/"+u.ent.ID+"/captable?view=fully-diluted&names=yes", 200)
	has(t, cap, "Eve Example", "1,000.5", "3,799.5", "100.0000%")
	lacks(t, fin.get("/company/entities/"+u.ent.ID+"/captable?names=no", 200), "Eve Example")
	has(t, hr.get("/company/entities/"+u.ent.ID, 200), "995,200", "1,000.5")
	hr.get("/company/entities/"+u.ent.ID+"/captable", 403) // hr has no money role
	hr.get("/company/entities/"+u.ent.ID+"/exports", 403)
	hr.post("/company/entities/"+u.ent.ID+"/exports", 403, "name", "captable")
	has(t, hr.get("/", 200), "Grants waiting for approval")

	for _, name := range []string{"captable", "grants", "ledger"} {
		w := fin.post("/company/entities/"+u.ent.ID+"/exports", 200, "name", name)
		if ct, cd := w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"); !strings.HasPrefix(ct, "text/csv") || !strings.Contains(cd, "attachment") || !strings.Contains(cd, "equity-"+name+".csv") {
			t.Fatalf("%s export headers: %q %q", name, ct, cd)
		}
		rows, err := csv.NewReader(w.Body).ReadAll()
		if err != nil || len(rows) < 2 {
			t.Fatalf("%s export: %d rows, %v", name, len(rows), err)
		}
	}
	fin.post("/company/entities/"+u.ent.ID+"/exports", 404, "name", "../secrets")
	// an export cannot be started by a link, nor without the session's CSRF token
	fin.get("/company/entities/"+u.ent.ID+"/exports?name=captable", 200)
	if w := fin.do("POST", "/company/entities/"+u.ent.ID+"/exports", url.Values{"name": {"captable"}}, "Sec-Fetch-Site", "same-origin"); w.Code != 403 {
		t.Fatalf("export without csrf = %d", w.Code)
	}
}

// The self-service pages answer for the caller's own confirmed records only, whatever other roles
// the caller has.
func TestUIOwnDataOnly(t *testing.T) {
	u := newUIEnv(t)
	u.base()
	eveGrant := u.active(u.eveStk.ID, q(4800))
	other := u.active(u.founder.ID, q(7771)) // grouped, so no random id can contain the figure

	eve := u.login(t, u.eve)
	has(t, eve.get("/me", 200), "4,800")
	lacks(t, eve.get("/me", 200), "7,771")
	eve.get("/me/grants/"+eveGrant, 200)
	for _, p := range []string{"/me/grants/" + other, "/me/grants/" + other + "/statement", "/me/grants/grt_000000000000000000000000", "/me/grants/nonsense"} {
		eve.get(p, 404)
	}
	eve.post("/me/grants/"+other+"/exercise/review", 404, "quantity", "1")
	eve.post("/me/grants/"+other+"/exercise", 404, "quantity", "1", "idem", "x", "accept", "yes")
	for _, p := range []string{"/company", "/company/entities/" + u.ent.ID, "/company/entities/" + u.ent.ID + "/captable", "/company/entities/" + u.ent.ID + "/stakeholders",
		"/company/stakeholders/" + u.founder.ID, "/company/identity", "/company/entities/" + u.ent.ID + "/grants", "/company/grants/" + other,
		"/company/grants/" + eveGrant, "/company/entities/" + u.ent.ID + "/exercises", "/company/entities/" + u.ent.ID + "/exports"} {
		if w := eve.do("GET", p, nil); w.Code != 403 && w.Code != 404 {
			t.Errorf("employee GET %s = %d", p, w.Code)
		}
		lacks(t, eve.do("GET", p, nil).Body.String(), "Fran Founder", "7,771")
	}
	// somebody else's exercise request
	u.advance("2027-02-01")
	otherEx, _, err := u.s.RequestExercise(u.ctx, u.hr, other, q(5), "", nil)
	u.must(err)
	eve = u.login(t, u.eve)
	eve.post("/me/exercises/"+otherEx+"/cancel", 404)

	// a manager, who may have reports, sees nothing of theirs
	mx := u.login(t, u.max)
	mx.get("/me/grants/"+eveGrant, 404)
	lacks(t, mx.get("/me", 200), "4,800")

	// every other role does not widen the holder's pages
	hr := u.login(t, Actor{Subject: "sso-hr-employee", Roles: []string{RoleEmployee, RoleHR, RoleFinance, RoleAdmin}, PersonID: personMax})
	hr.get("/me/grants/"+eveGrant, 404)
	hr.get("/me/grants/"+other, 404)
	// and without a person there is no self-service at all
	np := u.login(t, Actor{Subject: "sso-nobody", Roles: []string{RoleEmployee}})
	has(t, np.get("/", 200), "not linked to a person")
	np.get("/me", 403)
	np.get("/me/grants/"+eveGrant, 403)

	// a record whose confirmation was withdrawn (its identity changed) shows its holder nothing
	u.must(u.s.SetIdentity(u.ctx, u.hr, u.eveStk.ID, Identity{Name: "Somebody New"}))
	has(t, eve.get("/me", 200), "waiting to be confirmed")
	lacks(t, eve.get("/me", 200), "4,800")
	eve.get("/me/grants/"+eveGrant, 404)
	eve.post("/me/grants/"+eveGrant+"/exercise", 404, "quantity", "1", "idem", "y", "accept", "yes")
}

func TestUIHolderQueueAndRegistration(t *testing.T) {
	u := newUIEnv(t)
	u.base()
	hr, hr2 := u.login(t, u.hr), u.login(t, u.hr2)
	list := "/company/entities/" + u.ent.ID + "/stakeholders"
	has(t, hr.post(list, 400, "kind", "employee", "name", "No Person").Body.String(), "person_id", `value="No Person"`)
	w := hr.post(list, 303, "kind", "advisor", "name", "Ada Advisor", "email", "ada@example.test")
	page := location(w)
	has(t, hr.get("/company/identity", 200), "Ada Advisor", "Not attested")
	has(t, hr.get(page, 200), "ada@example.test", "Waiting for a second person", page+"/attest")
	// who registered the record cannot attest it
	has(t, hr.confirm(page+"/attest", 403).Body.String(), "Separation of duties")
	hr2.post(page+"/attest", 400, "token", hr2.tok) // not ticked
	has(t, hr2.confirm(page+"/attest", 200).Body.String(), "Holder attested as external.", "Confirmed by a second person")
	lacks(t, hr.get("/company/identity", 200), "Ada Advisor")
	// no directory: there is no person link to verify
	hr2.confirm(page+"/verify", 409)
}

func TestUIDirectoryModeVerification(t *testing.T) {
	dir := &fakeDir{subjects: map[string]string{"user:ann": personAnn, "user:ben": personBen, "user:cat": personCat}}
	u := newUIEnv(t, func(o *Options) { o.Directory = dir })
	boss := Actor{Subject: "board-x", Roles: []string{RoleBoard}}
	u.ent = m(u.s.CreateEntity(u.ctx, boss, EntityInput{Name: "Example Pty Ltd", Currency: "USD"}))
	// in directory mode the person comes from the directory, not from the credential
	ann := u.login(t, Actor{Subject: "user:ann", Roles: []string{RoleHR}})
	ben := u.login(t, Actor{Subject: "user:ben", Roles: []string{RoleHR}})
	stranger := u.login(t, Actor{Subject: "user:stranger", Roles: []string{RoleHR}})
	list := "/company/entities/" + u.ent.ID + "/stakeholders"
	page := location(ann.post(list, 303, "kind", "employee", "name", "Cat Holder", "person_id", personCat))
	has(t, stranger.get("/", 200), "not linked to a person")
	stranger.post(list, 403, "kind", "advisor", "name", "X")
	has(t, ben.get("/company/identity", 200), "Cat Holder", "Person link not confirmed")
	has(t, ben.get(page, 200), page+"/verify")
	ann.confirm(page+"/verify", 403)
	has(t, ben.confirm(page+"/verify", 200).Body.String(), "Person link confirmed.")

	dir.down = true
	u.s.subjects.m = nil
	has(t, ann.get("/", 200), "directory cannot be reached")
	ann.post(list, 503, "kind", "advisor", "name", "X")
}

func TestUIServiceAccountAndTokenLogin(t *testing.T) {
	u := newUIEnv(t)
	u.st.Add("svc-token-0123456789abcdefghij", Principal{Subject: "svc-can-core", Roles: []string{RoleHR, RoleFinance, RoleAdmin}, Service: true})
	c := u.client(t)
	csrf := csrfRe.FindStringSubmatch(c.get("/login", 200))[1]
	for _, tok := range []string{"svc-token-0123456789abcdefghij", "unknown-token-0123456789abcdef", ""} {
		if w := c.do("POST", "/login", url.Values{"token": {tok}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin"); w.Code != 401 {
			t.Fatalf("token %q: %d", tok, w.Code)
		}
	}
	off := &UI{Svc: u.s, Auth: MultiAuth{u.st}, NoTokenLogin: true}
	u.h = off.Handler()
	lacks(t, u.client(t).get("/login", 200), `name="token"`)
	if w := c.do("POST", "/login", url.Values{"token": {u.token(u.hr)}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin"); w.Code != 403 {
		t.Fatalf("token login switched off: %d", w.Code)
	}
}

// The UI is off unless configured: the API's routes are all the module serves.
func TestUIOffByDefaultAndMounted(t *testing.T) {
	cfg, err := ParseConfig([]byte("keyring_file: k\n"))
	if err != nil || cfg.UI.Enabled || !cfg.UI.tokenLogin() {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	u := newUIEnv(t)
	api := (&API{Svc: u.s, Auth: u.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	both := withUI(api, u.h)
	for path, want := range map[string]int{"/healthz": 200, "/v1/entities": 401, "/login": 200, "/": 303} {
		for h, code := range map[http.Handler]int{both: want} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != code {
				t.Errorf("%s with the UI = %d, want %d", path, w.Code, code)
			}
		}
	}
	for _, path := range []string{"/login", "/", "/me", "/company"} {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("%s without the UI = %d", path, w.Code)
		}
	}
}

func TestUIExactFigures(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{fmtQty(MaxQty), "100,000,000,000"},
		{fmtQty(q(1_000_000) + 1), "1,000,000.0001"},
		{fmtQty(0), "0"},
		{fmtMoney(maxMinor, "USD"), "11,258,999,068,426.24 USD"},
		{fmtMoney(5, "USD"), "0.05 USD"},
		{fmtMoney(1234567, "JPY"), "1,234,567 JPY"},
		{fmtMoney(1234567, "KWD"), "1,234.567 KWD"},
		{fmtPPM(1), "0.0001%"},
		{fmtPPM(333_334), "33.3334%"},
		{fmtPPM(1_000_000), "100.0000%"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	for _, c := range []struct {
		in, cur string
		want    int64
		ok      bool
	}{
		{"1.25", "USD", 125, true}, {"1.2", "USD", 120, true}, {"7", "USD", 700, true}, {"0", "USD", 0, true}, {"1234567", "JPY", 1234567, true},
		{"0.001", "KWD", 1, true}, {"1.255", "USD", 0, false}, {"1.0", "JPY", 0, false}, {"-1", "USD", 0, false}, {"1,000", "USD", 0, false},
		{"1e3", "USD", 0, false}, {"", "USD", 0, false}, {".5", "USD", 0, false}, {"99999999999999999", "USD", 0, false}, {"11258999068426.25", "USD", 0, false},
		{"1", "XXX", 0, false},
	} {
		got, err := ParseMinor(c.in, c.cur)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseMinor(%q, %s) = %d, %v", c.in, c.cur, got, err)
		}
	}
}
