package equity

// The single authorisation layer. Every service method asks Decide or FieldAccess; nothing else
// in the package inspects roles. The functions are pure: the service resolves the relationship
// fact (is this record the caller's own?) from the store, and the policy decides. Anything not
// listed here is denied.

// Roles carried by the caller's token (SSO groups).
const (
	RoleEmployee = "employee" // self-service: their own grants, vesting and exercises
	RoleManager  = "manager"  // same as employee: a manager NEVER sees the equity of their reports
	RoleHR       = "hr"       // drafts and administers grants and stakeholders
	RoleFinance  = "finance"  // cap table, valuations, ledger, exercise payments, exports
	RoleBoard    = "board"    // approves grants; reads the cap table and valuations
	RoleAdmin    = "admin"    // structure, ledger, erasure, retention; cannot approve grants
)

// Action names what a caller wants to do.
type Action string

// Actions.
const (
	ActStructureManage Action = "structure.manage" // entities, share classes, plans
	ActStructureRead   Action = "structure.read"   // entities, classes, plans, pool status
	ActStakeholderRead Action = "stakeholder.read" // a stakeholder record (identity is a separate field)
	ActStakeholderList Action = "stakeholder.list"
	ActStakeholderEdit Action = "stakeholder.write"
	ActGrantRead       Action = "grant.read"       // Fact.Self for own grants
	ActGrantList       Action = "grant.list"       // list across holders
	ActGrantDraft      Action = "grant.draft"      // create, edit, delete a draft, submit, amend
	ActGrantApprove    Action = "grant.approve"    // the board step
	ActGrantAdminister Action = "grant.admin"      // activate, cancel, terminate, record milestones and acceleration
	ActExerciseRequest Action = "exercise.request" // Fact.Self for own grants, or on behalf by hr/finance
	ActExerciseRead    Action = "exercise.read"    // Fact.Self for own
	ActExerciseApprove Action = "exercise.approve"
	ActExercisePay     Action = "exercise.pay"
	ActExerciseIssue   Action = "exercise.issue"
	ActLedgerWrite     Action = "ledger.write"
	ActLedgerRead      Action = "ledger.read"
	ActCapTable        Action = "captable.read"
	ActWhatIf          Action = "captable.whatif"
	ActConvertible     Action = "convertible.manage"
	ActValuation       Action = "valuation.manage"
	ActExport          Action = "export"
	ActLegalHold       Action = "legalhold"
	ActErasure         Action = "erasure"
	ActRetention       Action = "retention"
	ActSelfList        Action = "self.list"
)

// Fact is what the service knows about the relationship between the actor and the record.
type Fact struct {
	Self bool // the record belongs to the actor's own stakeholder (linked through person_id)
}

// Actor is the authenticated caller. PersonID is the directory person id the token carries (it
// links the caller to their own stakeholder); it is empty for service accounts.
type Actor struct {
	Subject  string
	Roles    []string
	PersonID string
	CanCore  bool // see Principal.CanCore
	Service  bool // see Principal.Service
	// NeedPerson is set when a directory is configured: the actions of needsIdentity are then
	// refused to a caller the directory does not link to a person.
	NeedPerson    bool
	DirectoryDown bool // the directory could not say who the caller is
	BreakGlass    bool // a service account inside the operator's break-glass window
}

// Principal is an authenticated caller as the Authenticator reports it.
type Principal struct {
	Subject  string
	Roles    []string
	PersonID string
	// CanCore marks a can-core personal access token: a human caller about whom nothing says which
	// directory person they are.
	CanCore bool
	// Service marks a machine credential (the service account): no person stands behind it.
	Service bool
}

func (a Actor) has(role string) bool {
	for _, r := range a.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func (a Actor) known() bool {
	for _, r := range []string{RoleEmployee, RoleManager, RoleHR, RoleFinance, RoleBoard, RoleAdmin} {
		if a.has(r) {
			return true
		}
	}
	return false
}

// staff is a person acting on their own behalf: employee or manager role plus a linked person.
// The manager role adds nothing here: that is the rule "managers never see equity of reports".
func (a Actor) staff() bool {
	return a.known() && a.PersonID != "" && (a.has(RoleEmployee) || a.has(RoleManager))
}

func (a Actor) selfStaff(f Fact) bool { return a.staff() && f.Self }
func (a Actor) money() bool           { return a.has(RoleFinance) || a.has(RoleBoard) || a.has(RoleAdmin) }
func (a Actor) any() bool             { return a.has(RoleHR) || a.money() }

// needsPerson are the actions whose separation of duties compares the actor's directory person with
// the holder's. A can-core token carries none, so a person holding equity could act on their own grant
// or exercise unnoticed: such a token may not perform them. Grant approval is one of them: without a
// directory nothing could stop a board member approving their own grant.
var needsPerson = map[Action]bool{
	ActGrantDraft: true, ActGrantAdminister: true, ActGrantApprove: true,
	ActExerciseApprove: true, ActExercisePay: true, ActExerciseIssue: true,
}

// needsIdentity are the actions that, with a directory configured, need a caller linked to a
// directory person: everything whose separation of duties compares people, and everything that
// creates or confirms a holder's identity.
var needsIdentity = map[Action]bool{
	ActGrantDraft: true, ActGrantAdminister: true, ActGrantApprove: true,
	ActExerciseRequest: true, ActExerciseApprove: true, ActExercisePay: true, ActExerciseIssue: true,
	ActLedgerWrite: true, ActStakeholderEdit: true, ActErasure: true, ActLegalHold: true,
}

func (a Actor) unidentified(act Action) bool {
	return a.NeedPerson && a.PersonID == "" && needsIdentity[act]
}

// Decide answers whether the actor may perform the action given the facts.
func Decide(a Actor, act Action, f Fact) bool {
	if !a.known() || (a.CanCore && a.PersonID == "" && needsPerson[act]) || a.unidentified(act) {
		return false
	}
	switch act {
	case ActStructureManage:
		return a.has(RoleBoard) || a.has(RoleAdmin)
	case ActStructureRead:
		return a.any()
	case ActStakeholderList, ActStakeholderRead:
		if act == ActStakeholderRead && a.selfStaff(f) {
			return true
		}
		return a.any()
	case ActStakeholderEdit:
		return a.has(RoleHR) || a.has(RoleFinance) || a.has(RoleBoard) || a.has(RoleAdmin)
	case ActGrantRead:
		return a.selfStaff(f) || a.any()
	case ActGrantList:
		return a.any()
	case ActGrantDraft:
		return a.has(RoleHR) || a.has(RoleAdmin)
	case ActGrantApprove:
		return a.has(RoleBoard) // admin is deliberately not enough: the board step is the board's
	case ActGrantAdminister:
		return a.has(RoleHR) || a.has(RoleAdmin)
	case ActExerciseRequest:
		return a.selfStaff(f) || a.has(RoleHR) || a.has(RoleFinance) || a.has(RoleAdmin)
	case ActExerciseRead:
		return a.selfStaff(f) || a.any()
	case ActExerciseApprove:
		return a.has(RoleFinance) || a.has(RoleBoard) || a.has(RoleAdmin)
	case ActExercisePay:
		return a.has(RoleFinance) || a.has(RoleAdmin)
	case ActExerciseIssue:
		return a.has(RoleFinance) || a.has(RoleAdmin) || a.has(RoleHR)
	case ActLedgerWrite:
		return a.has(RoleFinance) || a.has(RoleAdmin)
	case ActLedgerRead, ActCapTable, ActWhatIf, ActConvertible, ActValuation, ActExport:
		return a.money()
	case ActLegalHold, ActErasure, ActRetention:
		return a.has(RoleAdmin)
	case ActSelfList:
		return a.staff()
	}
	return false
}

// Sensitive fields. Reads and writes are per field and audited.
const (
	FieldIdentity    = "stakeholder_identity" // name, email, tax id
	FieldGrantTerms  = "grant_terms"          // quantity, strike, schedule
	FieldGrantNotes  = "grant_notes"          // free-form internal notes
	FieldHoldings    = "holdings"             // what a holder owns
	FieldValuation   = "valuation"
	FieldConvertible = "convertible_terms"
	FieldPaymentRef  = "payment_reference"
)

// SensitiveFields lists every sensitive field in a stable order.
var SensitiveFields = []string{FieldIdentity, FieldGrantTerms, FieldGrantNotes, FieldHoldings, FieldValuation, FieldConvertible, FieldPaymentRef}

// FieldAccess decides per-field read or write access.
//
// Read: identity, terms and holdings by the holder themself (own record only) and by hr
// (identity, terms, notes), finance, board and admin; valuations and convertible terms by
// finance, board and admin only; managers never, whoever the holder reports to.
// Write: hr, finance, board and admin for identity; hr and admin for terms and notes (drafts
// only, enforced by the service); finance and admin for payment references; finance, board and
// admin for valuations and convertibles. The holder cannot write.
func FieldAccess(a Actor, write bool, field string, f Fact) bool {
	if !a.known() {
		return false
	}
	switch field {
	case FieldIdentity:
		if write {
			return a.has(RoleHR) || a.has(RoleFinance) || a.has(RoleBoard) || a.has(RoleAdmin)
		}
		return a.selfStaff(f) || a.any()
	case FieldGrantTerms:
		if write {
			return a.has(RoleHR) || a.has(RoleAdmin)
		}
		return a.selfStaff(f) || a.any()
	case FieldGrantNotes:
		return a.has(RoleHR) || a.has(RoleBoard) || a.has(RoleAdmin)
	case FieldHoldings:
		if write {
			return a.has(RoleFinance) || a.has(RoleAdmin)
		}
		return a.selfStaff(f) || a.money()
	case FieldValuation, FieldConvertible:
		return a.money()
	case FieldPaymentRef:
		return a.has(RoleFinance) || a.has(RoleAdmin)
	}
	return false
}

// IsSensitiveField reports whether name is a sensitive field.
func IsSensitiveField(name string) bool {
	for _, f := range SensitiveFields {
		if f == name {
			return true
		}
	}
	return false
}

// sodOK is the single separation-of-duties rule: two actions in one chain need two different
// people. Subjects are compared canonically (NFKC, invisible characters removed, case-folded),
// and a person is also one person across their subject and their directory person id.
// Everything that must be done by someone else calls this and nothing else.
func sodOK(actor Actor, others ...string) bool {
	me := canonSubject(actor.Subject)
	for _, o := range others {
		if o == "" {
			continue
		}
		if canonSubject(o) == me {
			return false
		}
		if actor.PersonID != "" && o == actor.PersonID {
			return false
		}
	}
	return true
}
