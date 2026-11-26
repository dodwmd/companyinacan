package equity

// What the web UI (specs/066-equity-ui) needs beyond the verbs of the JSON API. These are Service
// methods like any other: every one asks the policy layer, audits what it reads and narrows, never
// widens, what the API would answer. The ui*.go files hold no authorisation and no SQL.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// Capabilities says which pages and forms to offer a caller. It is presentation only: every
// Service method asks the policy again, with the facts of the record.
type Capabilities struct {
	Self            bool // their own equity
	Structure       bool // entities, classes, pools
	Stakeholders    bool // holder records and the verification queue
	StakeholderEdit bool
	Grants          bool // grants and exercises across holders
	GrantDraft      bool
	GrantApprove    bool
	GrantAdmin      bool
	ExerciseRequest bool // request or withdraw for a holder
	ExerciseApprove bool
	ExercisePay     bool
	ExerciseIssue   bool
	CapTable        bool
	Export          bool
	// NeedsPerson: some of the above is withheld only because the account is not linked to a
	// person in the directory.
	NeedsPerson bool
}

// Capabilities reports what the policy would allow the caller, whoever the record is.
func (s *Service) Capabilities(a Actor) Capabilities {
	may := func(act Action) bool { return Decide(a, act, Fact{}) }
	c := Capabilities{
		Self: may(ActSelfList), Structure: may(ActStructureRead), Stakeholders: may(ActStakeholderList), StakeholderEdit: may(ActStakeholderEdit),
		Grants: may(ActGrantList), GrantDraft: may(ActGrantDraft), GrantApprove: may(ActGrantApprove), GrantAdmin: may(ActGrantAdminister),
		ExerciseRequest: may(ActExerciseRequest), ExerciseApprove: may(ActExerciseApprove), ExercisePay: may(ActExercisePay), ExerciseIssue: may(ActExerciseIssue),
		CapTable: may(ActCapTable), Export: may(ActExport),
	}
	if a.PersonID == "" {
		linked := a
		linked.PersonID, linked.NeedPerson, linked.CanCore = "pers_ffffffffffffffffffffffff", false, false
		for _, act := range []Action{ActSelfList, ActGrantDraft, ActGrantApprove, ActGrantAdminister, ActExerciseRequest, ActExerciseApprove, ActExercisePay, ActExerciseIssue, ActStakeholderEdit} {
			if !may(act) && Decide(linked, act, Fact{}) {
				c.NeedsPerson = true
			}
		}
	}
	return c
}

// HolderNames returns the names of holder records, for lists. It needs the right to read
// identities across holders and audits each record before anything is returned.
func (s *Service) HolderNames(ctx context.Context, a Actor, ids []string) (map[string]string, error) {
	if err := s.authorize(ctx, a, ActStakeholderList, Fact{}, "stakeholder-names"); err != nil {
		return nil, err
	}
	if !FieldAccess(a, false, FieldIdentity, Fact{}) {
		s.auditDeny(ctx, a, "read", "stakeholder-names", []string{FieldIdentity})
		return nil, ErrForbidden
	}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	ctx, cancel := readCtx(ctx)
	defer cancel()
	res := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := validID("stk", id); err != nil {
			return nil, err
		}
		res = append(res, "stakeholder:"+id)
	}
	if len(res) > 0 {
		if err := s.auditReadMany(ctx, a, "read", res, []string{FieldIdentity}); err != nil {
			return nil, err
		}
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		idn, err := s.openIdentity(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out[id] = idn.Name
	}
	return out, nil
}

// ---- self service, strictly the caller's own ------------------------------------------------

// selfOnly is the caller as a holder and nothing else: whatever other roles they carry, a self
// service page shows and does only what a holder may. ok is false when the caller has no self
// service at all (no employee or manager role, or no verified person).
func selfOnly(a Actor) (Actor, bool) {
	var roles []string
	for _, r := range a.Roles {
		if r == RoleEmployee || r == RoleManager {
			roles = append(roles, r)
		}
	}
	a.Roles = roles
	return a, Decide(a, ActSelfList, Fact{})
}

func (s *Service) self(ctx context.Context, a Actor) (Actor, error) {
	me, ok := selfOnly(a)
	if !ok {
		s.auditDeny(ctx, a, string(ActSelfList), "self", nil)
		if a.DirectoryDown {
			return me, ErrIdentityUnavailable
		}
		return me, ErrForbidden
	}
	return me, nil
}

// ownRecords selects the holder records that are the caller's and that a second actor confirmed:
// a record somebody has linked to the caller but nobody confirmed shows them nothing. It takes one
// argument, the caller's person id.
func (s *Service) ownRecords() string {
	return `(SELECT id FROM stakeholders WHERE person_id = ? AND erased_at IS NULL AND ` + s.establishedSQL() + `)`
}

// Position is one grant with its vesting as of today.
type Position struct {
	Grant   Grant
	Vesting VestingView
}

// MyPositionsPage is a page of the caller's equity.
type MyPositionsPage struct {
	Items []Position
	Next  string
	// Unconfirmed counts records linked to the caller that wait for a second person.
	Unconfirmed int
}

// MyPositions lists the caller's own grants (approved or later) with their vesting today.
func (s *Service) MyPositions(ctx context.Context, a Actor, after int64) (*MyPositionsPage, error) {
	me, err := s.self(ctx, a)
	if err != nil {
		return nil, err
	}
	page, err := s.listGrants(ctx, me, `g.stakeholder_id IN `+s.ownRecords(), []any{me.PersonID}, GrantFilter{After: after, Limit: 50}, true)
	if err != nil {
		return nil, err
	}
	out := &MyPositionsPage{Next: page.Next}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	for i := range page.Items {
		g := page.Items[i]
		v, err := s.vestingView(ctx, s.db, &g, s.today())
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, Position{Grant: g, Vesting: *v})
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM stakeholders WHERE person_id = ? AND erased_at IS NULL AND NOT `+s.establishedSQL(), me.PersonID).Scan(&out.Unconfirmed)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// myGrant loads a grant only when it is the caller's own, on a confirmed record, and past
// approval; anything else is not found, whatever other roles the caller has.
func (s *Service) myGrant(ctx context.Context, a Actor, id string) (Actor, *grantRow, error) {
	me, err := s.self(ctx, a)
	if err != nil {
		return me, nil, err
	}
	if err := validID("grt", id); err != nil {
		return me, nil, err
	}
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantCols+` FROM grants g WHERE g.id = ? AND g.status NOT IN ('draft', 'pending_approval') AND g.stakeholder_id IN `+s.ownRecords(), id, me.PersonID))
	if errors.Is(err, sql.ErrNoRows) {
		s.auditDeny(ctx, a, "grant.read", "grant:"+id, []string{FieldGrantTerms})
		return me, nil, ErrNotFound
	}
	return me, g, err
}

// MyPosition returns one of the caller's own grants with its vesting and its exercise requests.
func (s *Service) MyPosition(ctx context.Context, a Actor, id string) (*Position, []Exercise, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	me, g, err := s.myGrant(ctx, a, id)
	if err != nil {
		return nil, nil, err
	}
	if err := s.auditRead(ctx, me, "grant:"+id, []string{FieldGrantTerms}); err != nil {
		return nil, nil, err
	}
	view, err := s.view(me, g, Fact{Self: true})
	if err != nil {
		return nil, nil, err
	}
	v, err := s.vestingView(ctx, s.db, &g.Grant, s.today())
	if err != nil {
		return nil, nil, err
	}
	ex, err := s.listExercises(ctx, me, `grant_id = ?`, []any{id}, "")
	if err != nil {
		return nil, nil, err
	}
	return &Position{Grant: *view, Vesting: *v}, ex, nil
}

// MyConfirmedExercises lists the caller's own exercise requests, on confirmed records only.
func (s *Service) MyConfirmedExercises(ctx context.Context, a Actor) ([]Exercise, error) {
	me, err := s.self(ctx, a)
	if err != nil {
		return nil, err
	}
	return s.listExercises(ctx, me, `stakeholder_id IN `+s.ownRecords(), []any{me.PersonID}, "")
}

// ExercisePreview is what an exercise of the caller's own grant would be, before it is requested.
type ExercisePreview struct {
	Position Position
	Quantity Qty
	DueMinor int64
}

// PreviewMyExercise checks a quantity against the caller's own grant and computes the amount due
// with the same function the request uses. Nothing is recorded.
func (s *Service) PreviewMyExercise(ctx context.Context, a Actor, grantID string, qty Qty) (*ExercisePreview, error) {
	p, _, err := s.MyPosition(ctx, a, grantID)
	if err != nil {
		return nil, err
	}
	if qty <= 0 {
		return nil, invalid("quantity must be a positive quantity")
	}
	if qty > p.Vesting.Exercisable {
		return nil, conflict("only %s can be exercised now (vested, not yet exercised or requested, inside the exercise window ending %s)", p.Vesting.Exercisable, p.Vesting.WindowEnd)
	}
	due, err := exerciseCost(qty, p.Grant.StrikeMinor)
	if err != nil {
		return nil, err
	}
	return &ExercisePreview{Position: *p, Quantity: qty, DueMinor: due}, nil
}

// RequestMyExercise asks to exercise the caller's own grant, as the holder.
func (s *Service) RequestMyExercise(ctx context.Context, a Actor, grantID string, qty Qty, idemKey string, body []byte) (string, error) {
	me, _, err := s.myGrant(ctx, a, grantID)
	if err != nil {
		return "", err
	}
	id, _, err := s.RequestExercise(ctx, me, grantID, qty, idemKey, body)
	return id, err
}

// CancelMyExercise withdraws one of the caller's own exercise requests and returns its grant.
func (s *Service) CancelMyExercise(ctx context.Context, a Actor, id string) (string, error) {
	me, err := s.self(ctx, a)
	if err != nil {
		return "", err
	}
	e, err := s.loadExercise(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	if _, _, err := s.myGrant(ctx, a, e.GrantID); err != nil {
		return "", err
	}
	if _, err := s.CancelExercise(ctx, me, id); err != nil {
		return "", err
	}
	return e.GrantID, nil
}

// ConfirmationNeeded says how a holder record still has to be confirmed by a second person:
// "verify" (a directory person link), "attest" (no link to check), or "" when it is established
// or erased. Presentation only: VerifyPerson and AttestExternal decide for themselves.
func (s *Service) ConfirmationNeeded(st Stakeholder) string {
	switch {
	case st.Erased:
		return ""
	case s.directory != nil && st.PersonID != "":
		if st.PersonVerified {
			return ""
		}
		return "verify"
	case st.ExternalAttested || st.PersonVerified:
		return ""
	}
	return "attest"
}

// DraftForm returns the entity a grant is about to be drafted for, to a caller who may draft.
// CreateGrant asks again.
func (s *Service) DraftForm(ctx context.Context, a Actor, entityID string) (*Entity, error) {
	return s.formEntity(ctx, a, ActGrantDraft, entityID)
}

// ExportForm returns the entity an export is about to be made of, to a caller who may export.
// ExportCSV asks again.
func (s *Service) ExportForm(ctx context.Context, a Actor, entityID string) (*Entity, error) {
	return s.formEntity(ctx, a, ActExport, entityID)
}

func (s *Service) formEntity(ctx context.Context, a Actor, act Action, entityID string) (*Entity, error) {
	if err := s.authorize(ctx, a, act, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	return s.entity(ctx, s.db, entityID)
}
