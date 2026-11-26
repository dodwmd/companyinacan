package equity

// Verified holder identity (specs/051-holder-identity).
//
// Two links are involved:
//   - account -> directory person: the directory's sso_subject. This service only reads it.
//   - holder record -> directory person (or "external holder"): proposed by whoever creates or
//     links the record and confirmed by a second person. Only a confirmed ("established")
//     record lets a grant leave draft, an exercise proceed or the ledger be written.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Identity errors.
var (
	ErrIdentityRequired    = errors.New("this action needs an account linked to a person in the directory (people: sso_subject)")
	ErrIdentityUnavailable = errors.New("the people directory cannot be reached, so the caller or person cannot be verified")
)

// Identity modes.
const (
	ModeDirectory   = "directory"   // callers and holder links are checked against the people directory
	ModeAttestation = "attestation" // no directory: holders are established by a second actor's attestation
)

// serviceSubject reports whether a stored canonical subject is a service account's: no person.
func serviceSubject(s string) bool { return strings.HasPrefix(s, "svc-") }

// Mode is how holder identity is checked.
func (s *Service) Mode() string {
	if s.directory != nil {
		return ModeDirectory
	}
	return ModeAttestation
}

// breakGlass reports whether the operator's break-glass window is open.
func (s *Service) breakGlass() bool { return s.now().Before(s.breakGlassUntil) }

// verifyPersonID checks a person id a caller supplied for a record. Without a directory there
// is nothing to check it against: it stays an unverified label.
func (s *Service) verifyPersonID(ctx context.Context, id string) error {
	if s.directory == nil {
		return nil
	}
	ok, err := s.directory.PersonExists(ctx, id)
	if err != nil {
		return ErrIdentityUnavailable
	}
	if !ok {
		return invalid("person_id is not a person in the directory")
	}
	return nil
}

// approver is the actor the approval engine named as decider, with their directory person.
func (s *Service) approver(ctx context.Context, subject string) (Actor, error) {
	a := Actor{Subject: canonSubject(subject)}
	if s.directory == nil {
		return a, nil
	}
	pid, err := s.personFor(ctx, subject)
	if err != nil {
		return a, ErrIdentityUnavailable
	}
	if pid == "" {
		return a, ErrIdentityRequired
	}
	a.PersonID = pid
	return a, nil
}

// establishedSQL is the condition, over a stakeholders row, for "a second actor confirmed this
// record". With a directory a record that names a person needs that person link confirmed
// against it: an attestation made while there was no directory checked no person_id.
func (s *Service) establishedSQL() string {
	if s.directory != nil {
		return `(CASE WHEN person_id IS NULL THEN external_attested_by IS NOT NULL ELSE person_verified_by IS NOT NULL END)`
	}
	return `(person_verified_by IS NOT NULL OR external_attested_by IS NOT NULL)`
}

// requireEstablished refuses an action on a holder no second actor has confirmed. A service
// account inside the break-glass window is exempt (authorize puts each use on record).
func (s *Service) requireEstablished(ctx context.Context, q querier, a Actor, stakeholderID string) error {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT `+s.establishedSQL()+` FROM stakeholders WHERE id = ?`, stakeholderID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok && !a.BreakGlass {
		return conflict("the holder's identity is not established: someone else must confirm the person link (verify-person) or attest an external holder (attest-external); see GET /v1/identity/review")
	}
	return nil
}

// LinkPerson sets or changes the directory person of a holder record. The result is
// unconfirmed until someone else calls VerifyPerson. Nobody links a record to or away from
// themself.
func (s *Service) LinkPerson(ctx context.Context, a Actor, id, personID string) (*Stakeholder, error) {
	if err := validID("stk", id); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActStakeholderEdit, Fact{}, "stakeholder:"+id); err != nil {
		return nil, err
	}
	if s.directory == nil {
		return nil, conflict("no people directory is configured, so a person link cannot be checked: use attest-external")
	}
	if !personIDRe.MatchString(personID) {
		return nil, invalid("person_id must be a directory person id")
	}
	st, err := s.stakeholder(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if !sodOK(a, personID, st.PersonID) {
		s.auditDeny(ctx, a, "stakeholder.person.link", "stakeholder:"+id, nil)
		return nil, ErrSeparation
	}
	if err := s.verifyPersonID(ctx, personID); err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "stakeholder.person.link", "stakeholder:"+id, []string{"person_id"}, func(ctx context.Context, tx *sqlTx) error {
		cur, err := s.stakeholder(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case cur.Erased:
			return conflict("the stakeholder is erased")
		case !sodOK(a, cur.PersonID):
			return ErrSeparation
		case cur.PersonID == personID:
			return conflict("the stakeholder is already linked to that person: someone else confirms it with verify-person")
		}
		_, err = tx.ExecContext(ctx, `UPDATE stakeholders SET person_id = ?, person_linked_by = ?, person_verified_by = NULL, person_verified_on = NULL,
			external_attested_by = NULL, external_attested_on = NULL, updated_at = ? WHERE id = ?`, personID, canonSubject(a.Subject), s.now().Unix(), id)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return conflict("that person is already a stakeholder of this entity")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.stakeholder(ctx, s.db, id)
}

// VerifyPerson confirms that a record is the directory person it names. The confirmer is not
// whoever created it, linked it or last set its identity. The holder may confirm their own
// record only when people (not a service account) created and linked it: two people then
// still stand behind it, which keeps a company of two from locking itself out.
func (s *Service) VerifyPerson(ctx context.Context, a Actor, id string) (*Stakeholder, error) {
	return s.verifyPerson(ctx, a, id, "")
}

// VerifyShownPerson is VerifyPerson for a confirmer who was shown shown as the linked person: if
// the record has been relinked since, nothing is confirmed.
func (s *Service) VerifyShownPerson(ctx context.Context, a Actor, id, shown string) (*Stakeholder, error) {
	if shown == "" {
		return nil, invalid("person_id: say which person link you are confirming")
	}
	return s.verifyPerson(ctx, a, id, shown)
}

func (s *Service) verifyPerson(ctx context.Context, a Actor, id, shown string) (*Stakeholder, error) {
	if err := validID("stk", id); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActStakeholderEdit, Fact{}, "stakeholder:"+id); err != nil {
		return nil, err
	}
	if s.directory == nil {
		return nil, conflict("no people directory is configured, so a person link cannot be checked: use attest-external")
	}
	if a.Service && !a.BreakGlass {
		return nil, ErrIdentityRequired
	}
	st, err := s.stakeholder(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if st.PersonID == "" {
		return nil, conflict("the stakeholder has no person link: set one, or attest an external holder")
	}
	if shown != "" && st.PersonID != shown {
		return nil, conflict("the person link changed since it was shown to you: check the record again")
	}
	if err := s.verifyPersonID(ctx, st.PersonID); err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "stakeholder.person.verify", "stakeholder:"+id, []string{"person_id"}, func(ctx context.Context, tx *sqlTx) error {
		var created string
		var pid, linked, identity, verified sql.NullString
		var erased sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT created_by, person_id, person_linked_by, identity_by, person_verified_by, erased_at FROM stakeholders WHERE id = ?`, id).
			Scan(&created, &pid, &linked, &identity, &verified, &erased); err != nil {
			return err
		}
		switch {
		case erased.Valid:
			return conflict("the stakeholder is erased")
		case pid.String != st.PersonID:
			return conflict("the person link changed meanwhile")
		case verified.Valid:
			return conflict("the person link is already confirmed")
		case !sodOK(a, created, linked.String, identity.String):
			return ErrSeparation
		case a.PersonID == pid.String && (serviceSubject(created) || serviceSubject(linked.String)):
			return ErrSeparation
		}
		_, err := tx.ExecContext(ctx, `UPDATE stakeholders SET person_verified_by = ?, person_verified_on = ?, external_attested_by = NULL, external_attested_on = NULL, updated_at = ? WHERE id = ?`,
			canonSubject(a.Subject), s.today(), s.now().Unix(), id)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrSeparation) {
			s.auditDeny(ctx, a, "stakeholder.person.verify", "stakeholder:"+id, nil)
		}
		return nil, err
	}
	return s.stakeholder(ctx, s.db, id)
}

// Review reasons.
const (
	ReviewUnconfirmedLink = "unconfirmed_person_link"
	ReviewUnattested      = "unattested_holder"
	ReviewErasedHolder    = "erased_holder" // still holds shares: cancel, repurchase or transfer them
)

// ReviewItem is a holder record that needs someone to act. Ids and counts only.
type ReviewItem struct {
	StakeholderID string `json:"stakeholder_id"`
	EntityID      string `json:"entity_id"`
	Kind          string `json:"kind"`
	PersonID      string `json:"person_id,omitempty"`
	Reason        string `json:"reason"`
	OpenGrants    int    `json:"open_grants"`
	LedgerEntries int    `json:"ledger_entries"`
}

func (s *Service) reviewItems(ctx context.Context) ([]ReviewItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.entity_id, s.kind, IFNULL(s.person_id, ''), s.erased_at IS NOT NULL,
		(SELECT COUNT(*) FROM grants g WHERE g.stakeholder_id = s.id AND g.status IN ('draft', 'pending_approval', 'approved', 'active')),
		(SELECT COUNT(*) FROM ledger l WHERE l.stakeholder_id = s.id)
		FROM stakeholders s WHERE (s.erased_at IS NULL AND NOT `+s.establishedSQL()+`)
		OR (s.erased_at IS NOT NULL AND (SELECT IFNULL(SUM(quantity), 0) FROM ledger l WHERE l.stakeholder_id = s.id) > 0)
		ORDER BY s.created_at, s.id LIMIT 5000`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ReviewItem{}
	for rows.Next() {
		var it ReviewItem
		var erased bool
		if err := rows.Scan(&it.StakeholderID, &it.EntityID, &it.Kind, &it.PersonID, &erased, &it.OpenGrants, &it.LedgerEntries); err != nil {
			return nil, err
		}
		it.Reason = ReviewUnattested
		switch {
		case erased:
			it.Reason = ReviewErasedHolder
		case it.PersonID != "":
			it.Reason = ReviewUnconfirmedLink
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// IdentityReview lists the holder records that need a second actor, or hold shares after erasure. Whoever may list
// stakeholders may see it, linked to a person or not, so a locked-out administrator can.
func (s *Service) IdentityReview(ctx context.Context, a Actor) ([]ReviewItem, error) {
	if err := s.authorize(ctx, a, ActStakeholderList, Fact{}, "identity-review"); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	return s.reviewItems(ctx)
}

// WhoAmI is what the service concluded about the caller.
type WhoAmI struct {
	Subject            string `json:"subject"`
	Mode               string `json:"mode"`
	PersonID           string `json:"person_id,omitempty"`
	DirectoryAvailable bool   `json:"directory_available"`
	BreakGlass         bool   `json:"break_glass"`
}

// WhoAmI answers GET /v1/me/identity.
func (s *Service) WhoAmI(a Actor) WhoAmI {
	return WhoAmI{Subject: a.Subject, Mode: s.Mode(), PersonID: a.PersonID, DirectoryAvailable: !a.DirectoryDown, BreakGlass: a.BreakGlass}
}
