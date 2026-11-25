package board

// The single authorisation layer for both the `board` and `entity` modules. Every service method asks
// Decide or FieldAccess; nothing else in the package inspects roles. The functions are pure: the service
// resolves the relationship facts (is the actor an in-term member of this body? recused on this
// resolution? the subject of this register entry?) from the store, and the policy decides. Anything not
// listed here is denied.
//
// Board materials are confidential (constitution V): a person who cannot see a record gets 404 from the
// service (Decide on the read action is the visibility test), a person who can see it but not do the
// thing gets 403.

// Roles carried by the caller's token (SSO groups). `auditor` is reserved for can-core, so the external
// auditor role is `audit-viewer`.
const (
	RoleDirector    = "director"     // a member of one or more bodies: sees and votes in those bodies only
	RoleOfficer     = "officer"      // an executive invited to meetings; sees what they were invited to
	RoleSecretary   = "secretary"    // the company secretary: runs meetings and keeps the records; never votes
	RoleAdmin       = "admin"        // operates the service: metadata, legal hold, erasure; sees no board content
	RoleObserver    = "observer"     // a listed observer of a body: agenda and approved minutes
	RoleAuditViewer = "audit-viewer" // read-only: decided resolutions, approved minutes, registers
	RoleEmployee    = "employee"     // sees only the action items assigned to them
	RoleIntegration = "integration"  // the can-core service account: asks for resolutions and reads their outcome, nothing else
)

// Action names what a caller wants to do.
type Action string

// Actions. The *Read actions double as the visibility test (404 when false).
const (
	ActBodyManage         Action = "body.manage"
	ActBodyRead           Action = "body.read"
	ActMemberManage       Action = "member.manage"
	ActMeetingSchedule    Action = "meeting.schedule"
	ActMeetingRead        Action = "meeting.read"
	ActAgendaManage       Action = "agenda.manage"
	ActAttendanceRecord   Action = "attendance.record"
	ActPackManage         Action = "pack.manage"
	ActPackRead           Action = "pack.read"
	ActConflictDeclare    Action = "conflict.declare"
	ActResolutionDraft    Action = "resolution.draft"
	ActResolutionRead     Action = "resolution.read"
	ActResolutionEdit     Action = "resolution.edit" // amend and propose: the proposer only
	ActResolutionWithdraw Action = "resolution.withdraw"
	ActVoteOpen           Action = "vote.open"
	ActVoteCast           Action = "vote.cast" // roll call, written consent and secret ballot
	ActVoteClose          Action = "vote.close"
	ActMinutesDraft       Action = "minutes.draft"
	ActMinutesDraftRead   Action = "minutes.draft.read"
	ActMinutesRead        Action = "minutes.read" // approved minutes
	ActMinutesApprove     Action = "minutes.approve"
	ActActionManage       Action = "action.manage"
	ActActionRead         Action = "action.read"
	ActActionUpdate       Action = "action.update"
	ActResolutionRequest  Action = "resolution.request" // integration: ask for a resolution on behalf of another module
	ActResolutionOutcome  Action = "resolution.outcome" // integration: read the outcome of such a request
	ActLegalHold          Action = "legalhold"
	ActErasure            Action = "erasure"
	ActRetention          Action = "retention"
	// entity module
	ActEntityManage   Action = "entity.manage"
	ActEntityRead     Action = "entity.read"
	ActRegisterWrite  Action = "register.write"
	ActRegisterAttest Action = "register.attest"
	ActRegisterRead   Action = "register.read"
	ActFilingManage   Action = "filing.manage"
	ActFilingMark     Action = "filing.mark"
)

// AllActions lists every action (the policy matrix test checks each has a row).
var AllActions = []Action{
	ActBodyManage, ActBodyRead, ActMemberManage, ActMeetingSchedule, ActMeetingRead, ActAgendaManage, ActAttendanceRecord,
	ActPackManage, ActPackRead, ActConflictDeclare, ActResolutionDraft, ActResolutionRead, ActResolutionEdit,
	ActResolutionWithdraw, ActVoteOpen, ActVoteCast, ActVoteClose, ActMinutesDraft, ActMinutesDraftRead, ActMinutesRead,
	ActMinutesApprove, ActResolutionRequest, ActResolutionOutcome, ActActionManage, ActActionRead, ActActionUpdate, ActLegalHold, ActErasure, ActRetention,
	ActEntityManage, ActEntityRead, ActRegisterWrite, ActRegisterAttest, ActRegisterRead, ActFilingManage, ActFilingMark,
}

// Fact is what the service knows about the relationship between the actor and the record.
type Fact struct {
	Member     bool // in-term member of the body (through the person link); for entity records: of any body
	Chair      bool // chair of the body
	Attended   bool // was present at the meeting concerned
	Invited    bool // invited to the meeting (or a meeting of the body) as an attendee
	ObserverOf bool // listed observer of the body
	Recused    bool // recused on this resolution (declared, treatment recuse)
	Conflicted bool // declared an interest in this resolution
	Subject    bool // the record is about the actor (resolution subject, register entry naming them)
	Proposer   bool // proposed this resolution
	Drafter    bool // drafted the minutes, or created the register entry
	Self       bool // the record belongs to the actor (action item assignee, own private data)
	Final      bool // the resolution is decided (passed, failed or withdrawn)
	Secret     bool // the resolution is a secret ballot
}

// Actor is the authenticated caller. PersonID is the directory person id the token carries: it links the
// caller to their memberships. It is empty for service accounts.
type Actor struct {
	Subject  string
	Roles    []string
	PersonID string
}

// Principal is an authenticated caller as the Authenticator reports it.
type Principal struct {
	Subject  string
	Roles    []string
	PersonID string
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
	for _, r := range []string{RoleDirector, RoleOfficer, RoleSecretary, RoleAdmin, RoleObserver, RoleAuditViewer, RoleEmployee, RoleIntegration} {
		if a.has(r) {
			return true
		}
	}
	return false
}

// dir is a director acting as a member of the body concerned. Without the person link there is no
// membership, so a bare director token sees nothing.
func (a Actor) dir(f Fact) bool   { return a.has(RoleDirector) && a.PersonID != "" && f.Member }
func (a Actor) chair(f Fact) bool { return a.dir(f) && f.Chair }
func (a Actor) sec() bool         { return a.has(RoleSecretary) }

// Decide answers whether the actor may perform the action given the facts.
func Decide(a Actor, act Action, f Fact) bool {
	if !a.known() {
		return false
	}
	switch act {
	case ActBodyManage:
		return a.sec() || a.has(RoleAdmin)
	case ActBodyRead, ActMeetingRead:
		return a.sec() || a.has(RoleAdmin) || a.has(RoleAuditViewer) || a.dir(f) ||
			(a.has(RoleOfficer) && f.Invited) || (a.has(RoleObserver) && a.PersonID != "" && f.ObserverOf)
	case ActMemberManage, ActAttendanceRecord, ActPackManage, ActMinutesDraft:
		return a.sec()
	case ActMeetingSchedule, ActAgendaManage, ActVoteOpen, ActVoteClose, ActActionManage:
		return a.sec() || a.chair(f)
	case ActPackRead:
		return a.sec() || a.dir(f) || (a.has(RoleOfficer) && f.Invited)
	case ActConflictDeclare, ActResolutionDraft:
		return a.sec() || a.dir(f)
	case ActResolutionRead:
		return a.sec() || a.has(RoleAdmin) || a.has(RoleAuditViewer) || a.dir(f)
	case ActResolutionEdit:
		return f.Proposer && (a.sec() || a.dir(f))
	case ActResolutionWithdraw:
		return a.sec() || (f.Proposer && a.dir(f))
	case ActVoteCast:
		return a.dir(f) && !f.Recused && !f.Subject
	case ActMinutesDraftRead:
		return a.sec() || (a.dir(f) && (f.Chair || f.Attended))
	case ActMinutesRead:
		return a.sec() || a.has(RoleAuditViewer) || a.dir(f) || (a.has(RoleOfficer) && f.Invited) ||
			(a.has(RoleObserver) && a.PersonID != "" && f.ObserverOf)
	case ActMinutesApprove:
		return a.dir(f) && f.Attended && !f.Drafter
	case ActActionRead:
		return a.sec() || a.dir(f) || (a.PersonID != "" && f.Self)
	case ActActionUpdate:
		return a.sec() || (a.PersonID != "" && f.Self)
	case ActResolutionRequest, ActResolutionOutcome:
		return a.has(RoleIntegration)
	case ActLegalHold, ActErasure, ActRetention:
		return a.has(RoleAdmin)
	case ActEntityManage:
		return a.sec() || a.has(RoleAdmin)
	case ActEntityRead:
		return a.sec() || a.has(RoleAdmin) || a.has(RoleOfficer) || a.has(RoleAuditViewer) || a.dir(f)
	case ActRegisterRead:
		return a.sec() || a.has(RoleOfficer) || a.has(RoleAuditViewer) || a.dir(f)
	case ActRegisterWrite, ActFilingManage:
		return a.sec()
	case ActRegisterAttest:
		// directors and officers attest; nobody attests an entry that names them or that they wrote
		return (a.dir(f) || a.has(RoleOfficer)) && !f.Subject && !f.Drafter
	case ActFilingMark:
		return a.sec() || a.has(RoleOfficer)
	}
	return false
}

// Sensitive fields. Reads and writes are per field and audited.
const (
	FieldResolutionText = "resolution_text"   // title and text of a resolution
	FieldVoteDetail     = "vote_detail"       // who voted how (never for a secret ballot)
	FieldMinutesDraft   = "minutes_draft"     // minutes before approval
	FieldMinutes        = "minutes"           // approved minutes
	FieldPack           = "board_pack"        // the pack's document list and notes
	FieldConflictNature = "conflict_nature"   // what the interest is
	FieldRegNumber      = "entity_identifier" // registration and tax numbers
	FieldPersonPrivate  = "person_private"    // addresses and dates of birth of individuals
	FieldRegisterDetail = "register_detail"   // names, holdings and charge terms in a statutory register
)

// SensitiveFields lists every sensitive field in a stable order.
var SensitiveFields = []string{FieldResolutionText, FieldVoteDetail, FieldMinutesDraft, FieldMinutes, FieldPack, FieldConflictNature, FieldRegNumber, FieldPersonPrivate, FieldRegisterDetail}

// FieldAccess decides per-field read or write access.
func FieldAccess(a Actor, write bool, field string, f Fact) bool {
	if !a.known() {
		return false
	}
	switch field {
	case FieldResolutionText:
		if write {
			return f.Proposer && (a.sec() || a.dir(f))
		}
		if a.sec() {
			return true
		}
		if a.has(RoleAuditViewer) {
			return f.Final
		}
		// a recused director, or one the resolution is about, does not read the deliberation
		return a.dir(f) && !f.Recused && !f.Subject
	case FieldVoteDetail:
		if write || f.Secret {
			return false // a secret ballot is never readable per voter, by anyone
		}
		if a.sec() {
			return true
		}
		if a.has(RoleAuditViewer) {
			return f.Final
		}
		return a.dir(f) && f.Final && !f.Recused && !f.Subject
	case FieldMinutesDraft:
		if write {
			return a.sec()
		}
		return a.sec() || (a.dir(f) && (f.Chair || f.Attended))
	case FieldMinutes:
		if write {
			return false // approved minutes never change
		}
		return Decide(a, ActMinutesRead, f)
	case FieldPack:
		if write {
			return a.sec()
		}
		return Decide(a, ActPackRead, f)
	case FieldConflictNature:
		if write {
			return a.sec() || a.dir(f)
		}
		return a.sec() || a.chair(f) || (a.PersonID != "" && f.Self)
	case FieldRegNumber:
		if write {
			return a.sec()
		}
		return a.sec() || a.has(RoleAuditViewer) || a.has(RoleOfficer) || a.dir(f)
	case FieldPersonPrivate:
		if write {
			return a.sec()
		}
		return a.sec() || (a.PersonID != "" && f.Self)
	case FieldRegisterDetail:
		if write {
			return a.sec()
		}
		return Decide(a, ActRegisterRead, f)
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

// sodOK is the single separation-of-duties rule: two actions in one chain need two different people.
// Subjects are compared canonically (NFKC, invisible characters removed, case-folded), and a person is
// also one person across their subject and their directory person id. Everything that must be done by
// someone else (minutes approval, the pass of a resolution its proposer cannot carry alone, register
// attestation) calls this and nothing else.
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
