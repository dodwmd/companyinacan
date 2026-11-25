package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resumeFiles(e *env) int {
	ents, _ := os.ReadDir(filepath.Join(e.dir, "resumes"))
	return len(ents)
}

func TestSealedFieldsAreBoundToTheirRow(t *testing.T) {
	s := newScenario(t)
	db := s.app.store.db
	var e1, e2 string
	_ = db.QueryRow(`SELECT email_sealed FROM candidates WHERE id = ?`, s.c1).Scan(&e1)
	_ = db.QueryRow(`SELECT email_sealed FROM candidates WHERE id = ?`, s.c2).Scan(&e2)
	if !strings.HasPrefix(e1, "v1.k1.") || e1 == e2 {
		t.Fatalf("sealed forms: %q %q", e1, e2)
	}
	// swapping ciphertext between rows must not decrypt (row-swap attack)
	if _, err := db.Exec(`UPDATE candidates SET email_sealed = ? WHERE id = ?`, e2, s.c1); err != nil {
		t.Fatal(err)
	}
	if r := s.do(s.recruiter, "GET", "/v1/candidates/"+s.c1, nil); r.Code != 500 || strings.Contains(r.str(), "cand2@example.test") {
		t.Errorf("a value swapped between rows was decrypted: %d %s", r.Code, r.Body)
	}
}

func TestKeyRotation(t *testing.T) {
	s := newScenario(t)
	kr, _ := ParseKeyring("k2 ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100\nk1 " + testKeyHex + "\n")
	s.app.store.keys = kr
	// data under the retired key still reads
	if r := s.want(s.do(s.recruiter, "GET", "/v1/candidates/"+s.c1, nil), 200); r.m(t)["email"] != "cand1@example.test" {
		t.Fatalf("old data unreadable: %s", r.Body)
	}
	n, err := s.app.store.RotateKeys(ctxBG())
	if err != nil || n == 0 {
		t.Fatalf("rotate = %d, %v", n, err)
	}
	if n2, _ := s.app.store.RotateKeys(ctxBG()); n2 != 0 {
		t.Errorf("second rotation re-sealed %d values", n2)
	}
	var sealed string
	_ = s.app.store.db.QueryRow(`SELECT email_sealed FROM candidates WHERE id = ?`, s.c1).Scan(&sealed)
	if !strings.HasPrefix(sealed, "v1.k2.") {
		t.Errorf("not re-sealed: %q", sealed)
	}
	s.app.store.keys, _ = ParseKeyring("k2 ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100\n")
	if r := s.want(s.do(s.recruiter, "GET", "/v1/candidates/"+s.c1, nil), 200); r.m(t)["email"] != "cand1@example.test" {
		t.Error("data unreadable after dropping the retired key")
	}
	// a concurrent write during rotation is never overwritten with stale data
	s.app.store.keys, _ = ParseKeyring("k3 0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20\nk2 ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100\n")
	fired := false
	s.app.store.rotateHook = func() {
		if fired {
			return
		}
		fired = true
		_, _ = s.app.store.db.Exec(`UPDATE candidates SET notes_sealed = ? WHERE id = ?`, mustSeal(t, s.app.store, "candidates", s.c1, "notes_sealed", "FRESH"), s.c1)
	}
	if _, err := s.app.store.RotateKeys(ctxBG()); err != nil {
		t.Fatal(err)
	}
	if r := s.want(s.do(s.recruiter, "GET", "/v1/candidates/"+s.c1, nil), 200); r.m(t)["notes"] != "FRESH" {
		t.Errorf("rotation clobbered a concurrent write: %v", r.m(t)["notes"])
	}
}

func mustSeal(t *testing.T, s *Store, table, id, col, plain string) string {
	t.Helper()
	v, err := s.seal(table, id, col, plain)
	if err != nil {
		t.Fatal(err)
	}
	return v.String
}

func TestErasureScrubsEverythingAndLeavesTombstones(t *testing.T) {
	s := newScenario(t)
	s.want(s.do(s.recruiter, "PATCH", "/v1/candidates/"+s.c1, map[string]any{"notes": "NOTES-ERASE-ME", "address": "ADDR-ERASE-ME"}), 200)
	s.want(s.do(s.recruiter, "POST", "/v1/applications/"+s.a1+"/reject", map[string]any{"reason": "not_a_fit", "note": "NOTE-REJECT-ME"}), 200)
	s.want(s.do(s.recruiter, "PUT", "/v1/applications/"+s.a1+"/interviewers/ivy", nil), 204) // idempotent
	if resumeFiles(s.env) != 1 {
		t.Fatalf("setup: %d files", resumeFiles(s.env))
	}
	for _, secret := range []string{"cand1@example.test", "NOTES-ERASE-ME", "ADDR-ERASE-ME"} {
		if bytes.Contains(s.rawDBBytes(), []byte(secret)) {
			t.Fatalf("setup: %q is stored in the clear", secret)
		}
	}
	// only an unscoped admin can erase
	for _, tok := range []string{s.recruiter, s.hmAlice, s.ivy} {
		if r := s.do(tok, "POST", "/v1/candidates/"+s.c1+"/erasure", nil); r.Code != 403 {
			t.Errorf("erasure by a non-admin = %d", r.Code)
		}
	}
	// a legal hold blocks it
	s.want(s.do(adminTok, "PUT", "/v1/candidates/"+s.c1+"/legal-hold", map[string]any{"hold": true}), 204)
	if r := s.do(adminTok, "POST", "/v1/candidates/"+s.c1+"/erasure", nil); r.Code != 409 {
		t.Errorf("erasure under legal hold = %d", r.Code)
	}
	s.want(s.do(adminTok, "PUT", "/v1/candidates/"+s.c1+"/legal-hold", map[string]any{"hold": false}), 204)

	// pre-erasure pages must not survive in free pages or the write-ahead log
	r := s.want(s.do(adminTok, "POST", "/v1/candidates/"+s.c1+"/erasure", nil), 200).m(t)
	if r["subject"] != s.c1 || len(r["classes"].([]any)) != 3 {
		t.Errorf("erasure result: %v", r)
	}
	if resumeFiles(s.env) != 0 {
		t.Error("the resume file survived erasure")
	}
	raw := s.rawDBBytes()
	for _, secret := range []string{"cand1@example.test", "NOTES-ERASE-ME", "NOTE-REJECT-ME", "I like building things"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("%q survived erasure in the database or WAL", secret)
		}
	}
	// the sealed ciphertext itself is gone too, not just unreadable
	var n int
	_ = s.app.store.db.QueryRow(`SELECT COUNT(*) FROM candidates WHERE id = ? AND email_sealed IS NULL AND phone_sealed IS NULL AND notes_sealed IS NULL AND address_sealed IS NULL AND name = '[erased]'`, s.c1).Scan(&n)
	if n != 1 {
		t.Error("candidate row not scrubbed")
	}
	got := s.want(s.do(adminTok, "GET", "/v1/candidates/"+s.c1, nil), 200).m(t)
	if got["erased"] != true || got["name"] != "[erased]" || got["email"] != "" {
		t.Errorf("erased candidate: %v", got)
	}
	app := s.want(s.do(adminTok, "GET", "/v1/applications/"+s.a1, nil), 200).m(t)
	if app["candidate_name"] != "[erased]" || app["resume"] != nil || (app["cover_letter"] != nil && app["cover_letter"] != "") {
		t.Errorf("erased application: %v", app)
	}
	if r := s.do(adminTok, "GET", "/v1/applications/"+s.a1+"/resume", nil); r.Code != 404 {
		t.Errorf("resume of an erased candidate = %d", r.Code)
	}
	// the email can apply again as a fresh person
	_, slug := s.j1, s.slug1
	s.want(s.apply(slug, goodApplication("cand1@example.test")), 303)

	// tombstones in the shape the backup ledger takes
	ents := s.want(s.do(adminTok, "GET", "/v1/erasures?after=0", nil), 200).m(t)["items"].([]any)
	if len(ents) != 3 {
		t.Fatalf("tombstones = %v", ents)
	}
	classes := map[string]bool{}
	for _, e := range ents {
		m := e.(map[string]any)
		if m["subject"] != s.c1 || len(m["date"].(string)) != 10 {
			t.Errorf("tombstone %v", m)
		}
		classes[m["class"].(string)] = true
	}
	if !classes[ClassPII] || !classes[ClassResumes] || !classes[ClassEvaluations] {
		t.Errorf("classes = %v", classes)
	}
	// repeating the request is harmless and does not duplicate tombstones
	s.want(s.do(adminTok, "POST", "/v1/candidates/"+s.c1+"/erasure", nil), 200)
	if e2 := s.want(s.do(adminTok, "GET", "/v1/erasures?after=0", nil), 200).m(t)["items"].([]any); len(e2) != 3 {
		t.Errorf("tombstones after repeat = %d", len(e2))
	}
	// erasure is audited, with ids only
	log := s.want(s.do(adminTok, "GET", "/v1/audit?limit=200", nil), 200).str()
	if !strings.Contains(log, `"action":"candidate.erase"`) || strings.Contains(log, "Ada Example") {
		t.Error("erasure not audited properly")
	}
}

func TestPurgeSubjectReplaysAfterRestore(t *testing.T) {
	s := newScenario(t)
	// simulate a restore: data present although the ledger says it was erased
	for _, class := range []string{ClassResumes, ClassEvaluations, ClassPII} {
		s.want(s.do(adminTok, "POST", "/v1/purge", map[string]any{"subject": s.c2, "class": class}), 204)
		s.want(s.do(adminTok, "POST", "/v1/purge", map[string]any{"subject": s.c2, "class": class}), 204) // idempotent
	}
	if got := s.want(s.do(adminTok, "GET", "/v1/candidates/"+s.c2, nil), 200).m(t); got["name"] != "[erased]" {
		t.Errorf("purge did not erase: %v", got)
	}
	// unknown subject: no-op; bad input refused
	s.want(s.do(adminTok, "POST", "/v1/purge", map[string]any{"subject": "cand_0000000000000000", "class": ClassPII}), 204)
	for _, b := range []map[string]any{{"subject": "x", "class": ClassPII}, {"subject": s.c1, "class": "everything"}, {"subject": s.c1 + "' OR 1=1", "class": ClassPII}} {
		if r := s.do(adminTok, "POST", "/v1/purge", b); r.Code != 422 {
			t.Errorf("purge %v = %d", b, r.Code)
		}
	}
	// the other candidate is untouched
	if got := s.want(s.do(adminTok, "GET", "/v1/candidates/"+s.c1, nil), 200).m(t); got["name"] != "Ada Example" {
		t.Errorf("purge hit the wrong candidate: %v", got)
	}
}

func TestRetentionTimers(t *testing.T) {
	s := newScenario(t)
	day := 24 * time.Hour
	run := func() *RetentionReport {
		t.Helper()
		rep, err := s.app.store.RunRetention(ctxBG())
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	// active applications are never purged, however old
	s.clk.Add(1000 * day)
	if rep := run(); rep.CandidatesPurged != 0 {
		t.Fatalf("purged candidates with active applications: %+v", rep)
	}
	// (the jobs expired meanwhile; their applications stay active)
	s.want(s.do(s.recruiter, "POST", "/v1/applications/"+s.a1+"/reject", map[string]any{"reason": "not_a_fit"}), 200)
	s.want(s.do(s.recruiter, "PUT", "/v1/candidates/"+s.c2+"/talent-pool", map[string]any{"member": true}), 200)
	s.want(s.do(s.recruiter, "POST", "/v1/applications/"+s.a2+"/reject", map[string]any{"reason": "position_filled"}), 200)
	c1 := s.want(s.do(s.recruiter, "GET", "/v1/candidates/"+s.c1, nil), 200).m(t)
	c2 := s.want(s.do(s.recruiter, "GET", "/v1/candidates/"+s.c2, nil), 200).m(t)
	if c1["retain_until"] == nil || c2["retain_until"] == nil {
		t.Fatalf("retention timers not set: %v %v", c1["retain_until"], c2["retain_until"])
	}
	// rejected: 365 days; talent pool: 730 days
	s.clk.Add(364 * day)
	if rep := run(); rep.CandidatesPurged != 0 {
		t.Errorf("purged early: %+v", rep)
	}
	s.clk.Add(2 * day)
	if rep := run(); rep.CandidatesPurged != 1 {
		t.Errorf("rejected candidate not purged on time: %+v", rep)
	}
	if got := s.want(s.do(adminTok, "GET", "/v1/candidates/"+s.c1, nil), 200).m(t); got["name"] != "[erased]" {
		t.Errorf("c1 after retention: %v", got)
	}
	if got := s.want(s.do(adminTok, "GET", "/v1/candidates/"+s.c2, nil), 200).m(t); got["name"] == "[erased]" {
		t.Error("talent-pool candidate purged at the rejected-candidate period")
	}
	if resumeFiles(s.env) != 0 {
		t.Error("resume file survived retention")
	}
	s.clk.Add(366 * day)
	if rep := run(); rep.CandidatesPurged != 1 {
		t.Errorf("talent pool candidate not purged on time: %+v", rep)
	}
	// legal hold stops retention
	s2 := newScenario(t)
	s2.want(s2.do(s2.recruiter, "POST", "/v1/applications/"+s2.a1+"/reject", map[string]any{"reason": "not_a_fit"}), 200)
	s2.want(s2.do(adminTok, "PUT", "/v1/candidates/"+s2.c1+"/legal-hold", map[string]any{"hold": true}), 204)
	s2.clk.Add(400 * day)
	rep, err := s2.app.store.RunRetention(ctxBG())
	if err != nil || rep.CandidatesPurged != 0 || rep.SkippedLegalHold != 1 {
		t.Errorf("legal hold: %+v %v", rep, err)
	}
}

func TestRetentionWithoutTombstonesAndOtherData(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Retention.EEODays = 30; c.Retention.ClosedJobDays = 10 })
	id, slug := e.openJob("Engineer", "alice")
	f := goodApplication("eeo@example.test")
	f["eeo_gender"] = "female"
	e.want(e.apply(slug, f), 303)
	e.clk.Add(70 * 24 * time.Hour) // the job expires on its own
	rep, err := e.app.store.RunRetention(ctxBG())
	if err != nil || rep.JobsExpired != 1 || rep.EEODeleted != 1 {
		t.Fatalf("report %+v %v", rep, err)
	}
	if m := e.want(e.do(adminTok, "GET", "/v1/jobs/"+id, nil), 200).m(t); m["status"] != "closed" || m["close_reason"] != "expired" {
		t.Errorf("expired job: %v", m)
	}
	if r := e.want(e.do(adminTok, "GET", "/v1/erasures", nil), 200).m(t)["items"].([]any); len(r) != 0 {
		t.Error("retention wrote erasure tombstones (those are for requests)")
	}
}

func TestNoFileBytesInTheDatabaseAndTraversalIsImpossible(t *testing.T) {
	e := newEnv(t, nil)
	_, slug := e.openJob("Engineer", "alice")
	evil := upload{field: "resume", name: "../../../../etc/cron.d/x.pdf", ctype: "application/pdf", data: []byte("%PDF-1.7 TRAVERSAL-MARKER")}
	e.want(e.apply(slug, goodApplication("t@example.test"), evil), 303)
	if bytes.Contains(e.rawDBBytes(), []byte("TRAVERSAL-MARKER")) {
		t.Error("file bytes are in the database")
	}
	ents, _ := os.ReadDir(filepath.Join(e.dir, "resumes"))
	if len(ents) != 1 || strings.Contains(ents[0].Name(), "etc") {
		t.Errorf("files: %v", ents)
	}
	rec := e.mint(RoleRecruiter, "rita")
	id := e.want(e.do(rec, "GET", "/v1/applications", nil), 200).m(t)["items"].([]any)[0].(map[string]any)["id"].(string)
	m := e.want(e.do(rec, "GET", "/v1/applications/"+id, nil), 200).m(t)["resume"].(map[string]any)
	if m["name"] != "x.pdf" {
		t.Errorf("stored name %q", m["name"])
	}
}

func TestInfectedResumeIsNeverServed(t *testing.T) {
	e := newEnv(t, nil)
	_, slug := e.openJob("Engineer", "alice")
	e.want(e.apply(slug, goodApplication("ok@example.test"), upload{field: "resume", name: "cv.txt", ctype: "text/plain", data: []byte("clean text")}), 303)
	rec := e.mint(RoleRecruiter, "rita")
	id := e.want(e.do(rec, "GET", "/v1/applications", nil), 200).m(t)["items"].([]any)[0].(map[string]any)["id"].(string)
	// flag it infected after the fact (a later rescan)
	if _, err := e.app.store.db.Exec(`UPDATE applications SET resume_scan = 'infected' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if r := e.do(rec, "GET", "/v1/applications/"+id+"/resume", nil); r.Code != 404 {
		t.Errorf("infected resume served: %d", r.Code)
	}
}
