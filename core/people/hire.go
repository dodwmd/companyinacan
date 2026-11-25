package people

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// hireActor is the identity of the hand-off from can-core: hr, nothing else, and no person record.
var hireActor = Actor{Subject: "can-core", Roles: []string{RoleHR}}

var hireRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{3,63}$`)

// defaultOnboarding is created once, only when no template starts on entering onboarding, so a hire
// always gets a checklist. HR edits or replaces it like any template.
var defaultOnboarding = TemplateInput{Name: "Onboarding (default)", Kind: "onboarding", AutoStatus: string(StOnboarding), Items: []TemplateItem{
	{Title: "Complete the new-hire details (name, contact, start date)", AssigneeRole: "hr", DueOffsetDays: -7},
	{Title: "Create accounts and access", AssigneeRole: "hr", DueOffsetDays: -2, Action: "identity.provision"},
	{Title: "Plan the first week", AssigneeRole: "manager", DueOffsetDays: -2},
	{Title: "Welcome and introductions", AssigneeRole: "manager", DueOffsetDays: 0},
}}

// StartOnboardingFromHire creates the person for a hire (once per hireRef) in status onboarding,
// which instantiates the onboarding checklist. The hand-off carries no personal data, so the
// record is a placeholder (name "New hire" and the opaque candidate id) that HR completes; the
// name and contact details are fetched from the ats capability by someone allowed to read them.
// created is false when the hire was already started.
func (s *Service) StartOnboardingFromHire(ctx context.Context, hireRef, employmentType string) (personID string, created bool, err error) {
	if !hireRefRe.MatchString(hireRef) {
		return "", false, invalid("hire reference has an invalid format")
	}
	find := func() (string, error) {
		var id string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM people WHERE hire_ref = ?`, hireRef).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return id, err
	}
	if id, err := find(); err != nil || id != "" {
		return id, false, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checklist_templates WHERE auto_status = ?`, string(StOnboarding)).Scan(&n); err != nil {
		return "", false, err
	}
	if n == 0 {
		if _, err := s.CreateTemplate(ctx, hireActor, defaultOnboarding); err != nil && !errors.Is(err, ErrConflict) {
			return "", false, err
		}
	}
	kind := "employee"
	if employmentType == "contract" || employmentType == "contractor" {
		kind = "contractor"
	}
	p, err := s.createPerson(ctx, hireActor, PersonInput{Kind: kind, Status: StOnboarding, GivenName: "New hire", FamilyName: hireRef}, hireRef)
	if errors.Is(err, ErrConflict) { // lost a race with a redelivery
		id, ferr := find()
		return id, false, errors.Join(ferr)
	}
	if err != nil {
		return "", false, err
	}
	return p.ID, true, nil
}

// CanEventHandler handles the events can-core delivers to this module (manifest `subscribes`).
// Errors are returned only for failures worth retrying; a malformed event is dropped.
func (s *Service) CanEventHandler() func(context.Context, modclient.Event) error {
	return func(ctx context.Context, ev modclient.Event) error {
		if ev.Type != "candidate.hired.v1" || ev.Source != "jobs" {
			return nil // only the jobs module can hand off a hire
		}
		var d struct {
			CandidateID    string `json:"candidate_id"`
			EmploymentType string `json:"employment_type"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || !hireRefRe.MatchString(d.CandidateID) {
			return nil
		}
		_, _, err := s.StartOnboardingFromHire(ctx, d.CandidateID, d.EmploymentType)
		var inv *InvalidError
		if errors.As(err, &inv) {
			return nil
		}
		return err
	}
}
