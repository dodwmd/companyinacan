package jobs

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ldRE = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)

func TestCareersListAndDetail(t *testing.T) {
	e := newEnv(t, nil)
	id, slug := e.openJob("Senior Engineer", "alice")
	draftID := e.want(e.do(adminTok, "POST", "/v1/jobs", jobBody("Secret Draft Role", "alice")), 201).m(t)["id"].(string)
	_ = draftID

	list := e.want(e.do("", "GET", "/careers", nil), 200)
	if !contains(list.str(), "Senior Engineer", "/careers/"+slug) || strings.Contains(list.str(), "Secret Draft Role") {
		t.Errorf("list page: %s", list.Body)
	}
	if h := list.Hdr; !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("headers: %v", h)
	}
	// filters
	if r := e.want(e.do("", "GET", "/careers?q=nomatch", nil), 200); strings.Contains(r.str(), "Senior Engineer") {
		t.Error("search filter ignored")
	}
	if r := e.want(e.do("", "GET", "/careers?dept=Engineering&remote=hybrid&type=full_time", nil), 200); !strings.Contains(r.str(), "Senior Engineer") {
		t.Error("filters hid a matching job")
	}

	d := e.want(e.do("", "GET", "/careers/"+slug, nil), 200)
	if !contains(d.str(), "<h1>Senior Engineer</h1>", `action="/careers/`+slug+`/apply"`, `name="_t"`, `name="consent"`, `name="website"`, `enctype="multipart/form-data"`) {
		t.Errorf("detail page: %s", d.Body)
	}
	if strings.Contains(d.str(), "noindex") || d.Hdr.Get("X-Robots-Tag") != "" {
		t.Error("an open job must be indexable")
	}
	if strings.Contains(d.str(), "100000") {
		t.Error("unpublished salary is on the page")
	}
	// JSON-LD is valid and complete
	mm := ldRE.FindSubmatch(d.Body)
	if mm == nil {
		t.Fatal("no JSON-LD on the detail page")
	}
	var ld map[string]any
	if err := json.Unmarshal(mm[1], &ld); err != nil {
		t.Fatalf("JSON-LD does not parse: %v\n%s", err, mm[1])
	}
	for _, k := range []string{"title", "description", "datePosted", "validThrough", "hiringOrganization", "jobLocation", "employmentType", "identifier"} {
		if ld[k] == nil {
			t.Errorf("JSON-LD lacks %s", k)
		}
	}
	if ld["baseSalary"] != nil {
		t.Error("JSON-LD publishes an unpublished salary")
	}
	// publishing the salary shows it in both places
	e.want(e.do(adminTok, "PATCH", "/v1/jobs/"+id, map[string]any{"publish_salary": true}), 200)
	d = e.want(e.do("", "GET", "/careers/"+slug, nil), 200)
	if !strings.Contains(d.str(), "100,000") && !strings.Contains(d.str(), "100000") {
		t.Errorf("published salary not shown: %s", d.Body)
	}
	if !strings.Contains(string(ldRE.FindSubmatch(d.Body)[1]), "baseSalary") {
		t.Error("published salary missing from JSON-LD")
	}
	// unknown slug
	if r := e.do("", "GET", "/careers/does-not-exist-123456", nil); r.Code != 404 {
		t.Errorf("unknown slug = %d", r.Code)
	}
	if r := e.do("", "GET", "/careers/..%2f..%2fetc", nil); r.Code != 404 {
		t.Errorf("odd slug = %d", r.Code)
	}
}

func TestClosedAndPausedJobsAreNoindex(t *testing.T) {
	e := newEnv(t, nil)
	id, slug := e.openJob("Engineer", "alice")
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "paused"}), 200)
	for _, state := range []string{"paused", "closed"} {
		if state == "closed" {
			e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "closed", "reason": "filled"}), 200)
		}
		d := e.want(e.do("", "GET", "/careers/"+slug, nil), 200)
		if !strings.Contains(d.str(), `name="robots" content="noindex`) || !strings.Contains(d.Hdr.Get("X-Robots-Tag"), "noindex") {
			t.Errorf("%s job is indexable", state)
		}
		if strings.Contains(d.str(), "application/ld+json") {
			t.Errorf("%s job still emits JobPosting data (Google would keep showing it)", state)
		}
		if strings.Contains(d.str(), `name="_t"`) || strings.Contains(d.str(), "/apply") {
			t.Errorf("%s job offers the apply form", state)
		}
		if r := e.do("", "GET", "/careers", nil); strings.Contains(r.str(), slug) {
			t.Errorf("%s job is listed", state)
		}
		if r := e.do("", "GET", "/careers/sitemap.xml", nil); strings.Contains(r.str(), slug) {
			t.Errorf("%s job is in the sitemap", state)
		}
	}
}

func TestExpiredJobStopsAcceptingAndIsNoindex(t *testing.T) {
	e := newEnv(t, nil)
	_, slug := e.openJob("Engineer", "alice")
	e.clk.Add(61 * 24 * time.Hour) // default validity is 60 days
	d := e.want(e.do("", "GET", "/careers/"+slug, nil), 200)
	if !strings.Contains(d.Hdr.Get("X-Robots-Tag"), "noindex") || strings.Contains(d.str(), "application/ld+json") {
		t.Error("an expired job is still advertised")
	}
	if r := e.do("", "GET", "/careers", nil); strings.Contains(r.str(), slug) {
		t.Error("expired job listed")
	}
	r := e.postForm("198.51.100.1:1", "/careers/"+slug+"/apply", map[string]string{"name": "A B", "email": "a@example.test", "consent": "on"}, nil)
	if r.Code == 303 {
		t.Error("an expired job accepted an application")
	}
}

func TestHostileContentIsEscaped(t *testing.T) {
	e := newEnv(t, nil)
	b := jobBody(`<script>alert(1)</script> Eng`, "alice")
	b["department"] = `"><img src=x onerror=alert(2)>`
	b["sections"] = []map[string]string{{"heading": `<b>H</b>`, "body": "</script><script>alert(3)</script>\n\n<img src=x onerror=alert(4)>"}}
	id := e.want(e.do(adminTok, "POST", "/v1/jobs", b), 201).m(t)["id"].(string)
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"}), 200)
	slug := e.want(e.do(adminTok, "GET", "/v1/jobs/"+id, nil), 200).m(t)["slug"].(string)
	for _, path := range []string{"/careers", "/careers/" + slug, "/careers/sitemap.xml", "/careers/jobs.json", "/feeds/generic.xml", "/feeds/indeed.xml"} {
		r := e.want(e.do("", "GET", path, nil), 200)
		body := r.str()
		for _, bad := range []string{"<script>alert", "<img src=x", "<b>H</b>"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains unescaped %q", path, bad)
			}
		}
		if mm := ldRE.FindSubmatch(r.Body); mm != nil && !json.Valid(mm[1]) {
			t.Errorf("%s: JSON-LD invalid", path)
		}
		if strings.Count(strings.ToLower(body), "</script>") > strings.Count(strings.ToLower(body), "<script") {
			t.Errorf("%s: a </script> sequence from data closed a script element", path)
		}
	}
}

func TestSitemapRobotsAndFeeds(t *testing.T) {
	e := newEnv(t, nil)
	_, slug := e.openJob("Engineer", "alice")
	sm := e.want(e.do("", "GET", "/careers/sitemap.xml", nil), 200)
	if !strings.Contains(sm.Hdr.Get("Content-Type"), "xml") || !contains(sm.str(), "https://example.test/careers/"+slug, "<lastmod>") {
		t.Errorf("sitemap: %s", sm.Body)
	}
	rb := e.want(e.do("", "GET", "/robots.txt", nil), 200)
	if !contains(rb.str(), "Sitemap: https://example.test/careers/sitemap.xml", "Disallow: /v1/") {
		t.Errorf("robots: %s", rb.Body)
	}
	for _, p := range []string{"/feeds/generic.xml", "/feeds/indeed.xml"} {
		r := e.want(e.do("", "GET", p, nil), 200)
		if err := xml.Unmarshal(r.Body, new(struct{})); err != nil || !strings.Contains(r.str(), slug) {
			t.Errorf("%s: %v %s", p, err, r.Body)
		}
	}
	js := e.want(e.do("", "GET", "/careers/jobs.json", nil), 200)
	var arr []map[string]any
	js.json(t, &arr)
	if len(arr) != 1 || arr[0]["@type"] != "JobPosting" {
		t.Errorf("jobs.json: %s", js.Body)
	}
	if !strings.Contains(e.do("", "GET", "/careers/assets/careers.css", nil).Hdr.Get("Content-Type"), "text/css") {
		t.Error("stylesheet content type")
	}
}

func TestSyndicationLifecycleWithFakeBoard(t *testing.T) {
	e := newEnv(t, nil)
	id, _ := e.openJob("Engineer", "alice")
	if got := e.fake.Calls(); len(got) != 1 || got[0] != "publish:"+id {
		t.Fatalf("fake board calls after open = %v", got)
	}
	e.want(e.do(adminTok, "PATCH", "/v1/jobs/"+id, map[string]any{"title": "Engineer II"}), 200)
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "closed", "reason": "filled"}), 200)
	got := e.fake.Calls()
	if len(got) != 3 || !strings.HasPrefix(got[1], "update:") || !strings.HasPrefix(got[2], "close:") {
		t.Errorf("fake board calls = %v", got)
	}
	st := e.want(e.do(adminTok, "GET", "/v1/jobs/"+id+"/syndication", nil), 200).m(t)["items"].([]any)
	by := map[string]string{}
	for _, it := range st {
		m := it.(map[string]any)
		by[m["board"].(string)] = m["status"].(string)
	}
	if by["fake-board"] != "closed" || by["linkedin"] != "needs_partner_access" || by["indeed"] != "needs_partner_access" || by["glassdoor"] != "needs_partner_access" {
		t.Errorf("board states = %v", by)
	}
	bo := e.want(e.do(adminTok, "GET", "/v1/boards", nil), 200).str()
	if !contains(bo, "needs_partner_access", "google-jobs", `"feed"`) {
		t.Errorf("boards: %s", bo)
	}
}

func TestSyndicationFailureNeverBreaksTheLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	e.fake.FailPublish = os.ErrDeadlineExceeded
	id, _ := e.openJob("Engineer", "alice") // would fail the test if opening failed
	st := e.want(e.do(adminTok, "GET", "/v1/jobs/"+id+"/syndication", nil), 200).str()
	if !contains(st, `"status":"error"`) {
		t.Errorf("failure not recorded: %s", st)
	}
}

func TestApplyHappyPathStoresNothingPlaintextAndNoFileBytes(t *testing.T) {
	e := newEnv(t, nil)
	_, slug := e.openJob("Engineer", "alice")
	r := e.apply(slug, goodApplication("ada.private@example.test"), pdf)
	if r.Code != 303 || r.Hdr.Get("Location") != "/careers/applied" {
		t.Fatalf("apply = %d %s", r.Code, r.Body)
	}
	if a := e.want(e.do("", "GET", "/careers/applied", nil), 200); !strings.Contains(a.str(), "noindex") {
		t.Error("thank-you page is indexable")
	}
	raw := e.rawDBBytes()
	for _, secret := range []string{"ada.private@example.test", "+1 555 0100", "I like building things", "RESUME-MARKER-123"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("database file contains %q in the clear", secret)
		}
	}
	// file bytes are on disk, not in the database
	ents, _ := os.ReadDir(filepath.Join(e.dir, "resumes"))
	if len(ents) != 1 {
		t.Fatalf("resume files = %d", len(ents))
	}
	disk, _ := os.ReadFile(filepath.Join(e.dir, "resumes", ents[0].Name()))
	if !bytes.Contains(disk, []byte("RESUME-MARKER-123")) {
		t.Error("resume bytes not stored")
	}
	// recruiter reads it back (audited), the metadata says it was scanned
	rec := e.mint(RoleRecruiter, "rita")
	appID := e.want(e.do(rec, "GET", "/v1/applications", nil), 200).m(t)["items"].([]any)[0].(map[string]any)["id"].(string)
	got := e.want(e.do(rec, "GET", "/v1/applications/"+appID, nil), 200).m(t)
	if got["cover_letter"] != "I like building things." || got["candidate_name"] != "Ada Example" {
		t.Errorf("application: %v", got)
	}
	res := got["resume"].(map[string]any)
	if res["content_type"] != "application/pdf" || res["scan"] != "clean" || res["name"] != "ada.pdf" {
		t.Errorf("resume meta: %v", res)
	}
	if got["resume_key"] != nil || strings.Contains(string(mustJSON(got)), ents[0].Name()) {
		t.Error("the storage key leaked through the API")
	}
	dl := e.want(e.do(rec, "GET", "/v1/applications/"+appID+"/resume", nil), 200)
	if !bytes.Contains(dl.Body, []byte("RESUME-MARKER-123")) || !strings.Contains(dl.Hdr.Get("Content-Disposition"), "attachment") || dl.Hdr.Get("Content-Type") != "application/pdf" || dl.Hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("resume download headers: %v", dl.Hdr)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestApplyRejections(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Apply.MaxResumeBytes = 2048 })
	_, slug := e.openJob("Engineer", "alice")
	count := func() int {
		rec := e.mint(RoleRecruiter, "r"+time.Now().Format("150405.000000"))
		return len(e.want(e.do(rec, "GET", "/v1/applications", nil), 200).m(t)["items"].([]any))
	}
	files := func() int { ents, _ := os.ReadDir(filepath.Join(e.dir, "resumes")); return len(ents) }
	big := upload{field: "resume", name: "big.pdf", ctype: "application/pdf", data: append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("a"), 4096)...)}
	for name, c := range map[string]struct {
		fields map[string]string
		files  []upload
		code   int
	}{
		"no consent":      {map[string]string{"name": "A B", "email": "a@example.test"}, nil, 422},
		"bad email":       {map[string]string{"name": "A B", "email": "nope", "consent": "on"}, nil, 422},
		"no name":         {map[string]string{"email": "a@example.test", "consent": "on"}, nil, 422},
		"newline in name": {map[string]string{"name": "A\r\nBcc: x", "email": "a@example.test", "consent": "on"}, nil, 422},
		"oversize resume": {goodApplication("a@example.test"), []upload{big}, 413},
		"exe as pdf":      {goodApplication("a@example.test"), []upload{{field: "resume", name: "cv.pdf", ctype: "application/pdf", data: []byte("MZ\x90\x00 evil")}}, 422},
		"disallowed type": {goodApplication("a@example.test"), []upload{{field: "resume", name: "cv.exe", ctype: "application/pdf", data: []byte("%PDF-1.7")}}, 422},
		"infected":        {goodApplication("a@example.test"), []upload{{field: "resume", name: "cv.txt", ctype: "text/plain", data: []byte("hello EICAR-TEST hello")}}, 422},
		"two resumes":     {goodApplication("a@example.test"), []upload{pdf, pdf}, 422},
		"unexpected file": {goodApplication("a@example.test"), []upload{{field: "other", name: "cv.pdf", ctype: "application/pdf", data: []byte("%PDF-1.7")}}, 422},
	} {
		r := e.apply(slug, c.fields, c.files...)
		if r.Code != c.code {
			t.Errorf("%s = %d, want %d: %s", name, r.Code, c.code, r.Body)
		}
	}
	if n := count(); n != 0 {
		t.Errorf("%d applications stored from rejected submissions", n)
	}
	if n := files(); n != 0 {
		t.Errorf("%d resume files left behind by rejected submissions", n)
	}
	// scanner failure fails closed
	e.scanner.Err = os.ErrClosed
	if r := e.apply(slug, goodApplication("a@example.test"), pdf); r.Code == 303 {
		t.Error("a resume was accepted although the scanner could not run")
	}
	e.scanner.Err = nil
	if n := files(); n != 0 {
		t.Errorf("%d files left behind after scanner failure", n)
	}
}

func TestApplySpamControls(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.RateLimit.ApplyPerHour = 3 })
	_, slug := e.openJob("Engineer", "alice")
	// honeypot: looks like success, stores nothing
	f := goodApplication("bot@example.test")
	f["website"] = "http://spam.example"
	if r := e.apply(slug, f); r.Code != 303 {
		t.Errorf("honeypot = %d (a bot must not learn it was caught)", r.Code)
	}
	// too fast
	page := e.do("", "GET", "/careers/"+slug, nil)
	tok := string(tokRE.FindSubmatch(page.Body)[1])
	fast := goodApplication("fast@example.test")
	fast["_t"] = tok
	if r := e.postForm("198.51.100.2:1", "/careers/"+slug+"/apply", fast, nil); r.Code != 422 {
		t.Errorf("instant submit = %d", r.Code)
	}
	// forged, missing and replayed-for-another-job tokens
	for name, tk := range map[string]string{"forged": "AAAA.BBBB", "missing": "", "garbage": strings.Repeat("x", 3000)} {
		g := goodApplication("t@example.test")
		g["_t"] = tk
		if r := e.postForm("198.51.100.3:1", "/careers/"+slug+"/apply", g, nil); r.Code != 422 && r.Code != 400 {
			t.Errorf("%s token = %d", name, r.Code)
		}
	}
	e.clk.Add(3 * time.Hour) // past the 2h token lifetime
	late := goodApplication("late@example.test")
	late["_t"] = tok
	if r := e.postForm("198.51.100.4:1", "/careers/"+slug+"/apply", late, nil); r.Code != 422 {
		t.Errorf("expired token = %d", r.Code)
	}
	// cross-origin posts
	page = e.do("", "GET", "/careers/"+slug, nil)
	tok = string(tokRE.FindSubmatch(page.Body)[1])
	e.clk.Add(10 * time.Second)
	co := goodApplication("co@example.test")
	co["_t"] = tok
	if r := e.postForm("198.51.100.5:1", "/careers/"+slug+"/apply", co, map[string]string{"Origin": "https://evil.example"}); r.Code != 403 {
		t.Errorf("cross-origin = %d", r.Code)
	}
	if r := e.postForm("198.51.100.5:1", "/careers/"+slug+"/apply", co, map[string]string{"Origin": "http://example.com"}); r.Code != 303 {
		t.Errorf("same-origin = %d: %s", r.Code, r.Body)
	}
	// per-address rate limit on the public form only
	ip := "198.51.100.77:9"
	codes := []int{}
	for i := 0; i < 5; i++ {
		codes = append(codes, e.applyFrom(ip, slug, goodApplication("rl"+string(rune('a'+i))+"@example.test")).Code)
	}
	if codes[0] != 303 || codes[4] != 429 {
		t.Errorf("rate limit codes = %v", codes)
	}
	if r := e.applyFrom("198.51.100.78:9", slug, goodApplication("other@example.test")); r.Code != 303 {
		t.Errorf("another address was throttled: %d", r.Code)
	}
}

func TestDuplicateApplicationsAreDetectedByEmailHashAndNeverLeak(t *testing.T) {
	e := newEnv(t, nil)
	_, s1 := e.openJob("Engineer", "alice")
	_, s2 := e.openJob("Designer", "bob")
	e.applyOK(s1, "dup@example.test")
	// same person, same job, different case: accepted but not stored again
	r := e.apply(s1, map[string]string{"name": "Someone Else", "email": "DUP@Example.test", "consent": "on"})
	if r.Code != 303 {
		t.Fatalf("duplicate response = %d (must look like success)", r.Code)
	}
	// same person, other job: one candidate, two applications; the profile is not overwritten
	e.applyOK(s2, "dup@example.test")
	rec := e.mint(RoleRecruiter, "rita")
	c := e.want(e.do(rec, "GET", "/v1/candidates?email=dup@example.test", nil), 200).m(t)["items"].([]any)
	if len(c) != 1 {
		t.Fatalf("candidates for one email = %d", len(c))
	}
	cm := c[0].(map[string]any)
	if cm["name"] != "Ada Example" {
		t.Errorf("a later public form overwrote the candidate: %v", cm["name"])
	}
	apps := e.want(e.do(rec, "GET", "/v1/applications?candidate_id="+cm["id"].(string), nil), 200).m(t)["items"].([]any)
	if len(apps) != 2 {
		t.Errorf("applications for the candidate = %d", len(apps))
	}
	// staff adding the same person to the same job get a conflict they may see
	jobs := e.want(e.do(rec, "GET", "/v1/jobs", nil), 200).m(t)["items"].([]any)
	var j1 string
	for _, j := range jobs {
		if j.(map[string]any)["slug"] == s1 {
			j1 = j.(map[string]any)["id"].(string)
		}
	}
	if r := e.do(rec, "POST", "/v1/applications", map[string]any{"job_id": j1, "name": "Ada", "email": "dup@example.test", "consent_recorded": true}); r.Code != 409 {
		t.Errorf("manual duplicate = %d", r.Code)
	}
	if r := e.do(rec, "POST", "/v1/applications", map[string]any{"job_id": j1, "name": "Ada", "email": "new@example.test", "consent_recorded": false}); r.Code != 422 {
		t.Errorf("manual add without consent attestation = %d", r.Code)
	}
	e.want(e.do(rec, "POST", "/v1/applications", map[string]any{"job_id": j1, "name": "Bo Sourced", "email": "new@example.test", "consent_recorded": true, "source": "referral"}), 201)
}
