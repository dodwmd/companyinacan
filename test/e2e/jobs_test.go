//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

var timingToken = regexp.MustCompile(`name="_t" value="([^"]+)"`)

func multipartBody(t *testing.T, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("multipart: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// (e) Jobs: a recruiter opens a requisition; the careers page and its JSON-LD are public; a candidate
// applies through the public form; the recruiter sees them in the web UI; an interviewer who is not
// assigned sees nothing, and once assigned still cannot read other people's feedback before submitting
// their own (blind feedback).
func TestJobsRequisitionApplyAndBlindFeedback(t *testing.T) {
	admin := jobsAPI(t)
	u := harness.Unique()
	mint := func(role, sub string) string {
		return admin.Post(t, "/v1/tokens", map[string]any{"kind": "service", "subject": sub + "-" + u, "name": role + " " + u, "role": role}).
			Expect(t, "mint a "+role+" token", 201, 200).Str("secret")
	}
	recTok := mint("recruiter", "rita")
	intTok := mint("interviewer", "ivan")
	rec := admin.WithToken(recTok)
	interviewer := admin.WithToken(intTok)

	title := "E2E Engineer " + u
	job := rec.Post(t, "/v1/jobs", map[string]any{
		"title": title, "department": "Engineering", "location": map[string]any{"city": "Austin", "region": "TX", "country": "US"},
		"remote": "hybrid", "employment_type": "full_time", "hiring_manager": "hm-" + u, "headcount": 1,
		"salary":         map[string]any{"min": 100000, "max": 140000, "currency": "USD", "period": "year"},
		"publish_salary": false,
		"sections":       []map[string]string{{"heading": "About", "body": "Build things with the e2e suite."}},
	}).Expect(t, "create requisition", 201)
	jobID, slug := job.Str("id"), job.Str("slug")
	rec.Post(t, "/v1/jobs/"+jobID+"/transitions", map[string]any{"to": "open"}).Expect(t, "open the requisition", 200)

	// Public careers page, JSON-LD, feeds. No token on any of these.
	list := web.Do(t, harness.Req{Host: "jobs.localhost", Path: "/careers"}).Expect(t, "careers index", 200)
	if !strings.Contains(list.String(), title) {
		t.Fatalf("the careers index does not list %q", title)
	}
	page := web.Do(t, harness.Req{Host: "jobs.localhost", Path: "/careers/" + slug}).Expect(t, "careers page", 200)
	body := page.String()
	if !strings.Contains(body, "application/ld+json") || !strings.Contains(body, `"JobPosting"`) {
		t.Fatalf("the careers page of %s has no JobPosting JSON-LD", slug)
	}
	if ld := between(body, `<script type="application/ld+json">`, "</script>"); ld == "" || !json.Valid([]byte(ld)) {
		t.Fatalf("the JSON-LD block is missing or not valid JSON: %.200s", ld)
	}
	if strings.Contains(body, "100000") {
		t.Fatalf("the careers page shows the salary although publish_salary is false")
	}
	web.Do(t, harness.Req{Host: "jobs.localhost", Path: "/careers/sitemap.xml"}).Expect(t, "careers sitemap", 200)

	// Apply through the public form (timing token, honeypot, same-origin check).
	m := timingToken.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no timing token (_t) in the apply form")
	}
	time.Sleep(3 * time.Second)
	email := "cand-" + u + "@example.test"
	mp, ctype := multipartBody(t, map[string]string{"_t": m[1], "name": "Cass Candidate " + u, "email": email, "phone": "+1 555 0100", "cover_letter": "I like building things.", "consent": "on", "website": ""})
	web.Do(t, harness.Req{Method: "POST", Host: "jobs.localhost", Path: "/careers/" + slug + "/apply", Body: mp,
		Headers: map[string]string{"Content-Type": ctype, "Origin": "http://jobs.localhost:" + env.Port}}).Expect(t, "apply through the public form", 303)
	web.Do(t, harness.Req{Method: "POST", Host: "jobs.localhost", Path: "/careers/" + slug + "/apply", Body: mp,
		Headers: map[string]string{"Content-Type": ctype, "Origin": "https://evil.example"}}).Expect(t, "a foreign-origin application is refused", 403)

	// The recruiter finds the application through the API ...
	apps := rec.Get(t, "/v1/applications?job_id="+jobID).Expect(t, "recruiter lists applications", 200)
	items, _ := apps.JSON()["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected exactly one application for the job (the repeated post must be a duplicate or refused), got %d: %.300s", len(items), apps.String())
	}
	appID, _ := items[0].(map[string]any)["id"].(string)
	if appID == "" {
		t.Fatalf("no application id in %.300s", apps.String())
	}

	// ... and in the web UI, signed in with the same token.
	s := harness.NewSession(env)
	const host = "jobs.localhost"
	hdr := map[string]string{"Origin": "http://" + host + ":" + env.Port, "Sec-Fetch-Site": "same-origin"}
	s.Do(t, harness.Req{Host: host, Path: "/"}).Expect(t, "recruiter UI without a session", 303)
	login := s.Do(t, harness.Req{Host: host, Path: "/login"}).Expect(t, "recruiter UI login page", 200)
	s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/login", Headers: hdr, Body: url.Values{"csrf": {harness.CSRF(login.String())}, "token": {recTok}}}).Expect(t, "recruiter UI sign-in", 303)
	jobPage := s.Do(t, harness.Req{Host: host, Path: "/jobs/" + jobID}).Expect(t, "recruiter UI requisition page", 200)
	if !strings.Contains(jobPage.String(), "Cass Candidate "+u) {
		t.Fatalf("the recruiter UI pipeline does not show the candidate")
	}
	appPage := s.Do(t, harness.Req{Host: host, Path: "/applications/" + appID}).Expect(t, "recruiter UI application page", 200)
	if strings.Contains(appPage.String(), email) {
		t.Fatalf("the application page shows the candidate's email without the explicit reveal")
	}

	// Blind feedback. The interviewer sees nothing until assigned.
	listI := interviewer.Get(t, "/v1/applications").Expect(t, "unassigned interviewer lists applications", 200)
	if strings.Contains(listI.String(), appID) {
		t.Fatalf("an unassigned interviewer sees the application in the list")
	}
	interviewer.Get(t, "/v1/applications/"+appID).Expect(t, "unassigned interviewer cannot open it", 403, 404)
	interviewer.Get(t, "/v1/candidates").Expect(t, "interviewer has no candidate directory", 403, 404)

	admin.Put(t, "/v1/applications/"+appID+"/interviewers/rita-"+u, nil).Expect(t, "assign the recruiter as an interviewer too (scorecards need an assignment)", 200, 201, 204)
	rec.Put(t, "/v1/applications/"+appID+"/scorecards/mine", map[string]any{"rating": 4, "recommendation": "yes", "comments": "strong"}).Expect(t, "recruiter saves a scorecard", 200)
	rec.Post(t, "/v1/applications/"+appID+"/scorecards/mine/submit", nil).Expect(t, "recruiter submits it", 200)
	rec.Put(t, "/v1/applications/"+appID+"/interviewers/ivan-"+u, nil).Expect(t, "assign the interviewer", 200, 201, 204)
	interviewer.Get(t, "/v1/applications/"+appID).Expect(t, "assigned interviewer opens the application", 200)
	cards := interviewer.Get(t, "/v1/applications/"+appID+"/scorecards").Expect(t, "interviewer lists scorecards", 200)
	if strings.Contains(cards.String(), "strong") || strings.Contains(cards.String(), "rita") {
		t.Fatalf("an interviewer who has not submitted sees another person's feedback: %.300s", cards.String())
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	j := strings.Index(s, b)
	if j < 0 {
		return ""
	}
	return s[:j]
}
