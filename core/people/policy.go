package people

// The single authorisation layer. Every handler and service method asks Decide or
// FieldAccess; nothing else in the package inspects roles. The functions are pure: the
// service resolves the relationship facts from the store, and the policy decides. Anything
// not listed here is denied.

// Status is an employment status.
type Status string

// Employment lifecycle: candidate -> onboarding -> active <-> leave -> offboarding -> former.
const (
	StCandidate   Status = "candidate"
	StOnboarding  Status = "onboarding"
	StActive      Status = "active"
	StLeave       Status = "leave"
	StOffboarding Status = "offboarding"
	StFormer      Status = "former"
)

// Roles carried by the caller's token (SSO groups).
const (
	RoleEmployee = "employee"
	RoleManager  = "manager"
	RoleHR       = "hr"
	RoleAdmin    = "admin"
)

// Action names what a caller wants to do.
type Action string

// Actions. Directory.
const (
	ActDirRead       Action = "dir.read"        // basic fields of a person
	ActDirCreate     Action = "dir.create"      // create a person
	ActDirUpdate     Action = "dir.update"      // change basic fields of someone else
	ActDirUpdateSelf Action = "dir.update_self" // change own limited basic fields
	ActDirOrg        Action = "dir.org"         // departments, teams, positions, manager assignment
	ActDirStatus     Action = "dir.status"      // employment status transitions
	ActDirHistory    Action = "dir.history"     // status history of a person
	ActLegalHold     Action = "dir.legal_hold"
	ActErasure       Action = "dir.erasure"
	ActRetention     Action = "dir.retention" // run the retention sweep, list the erasure ledger
	// Checklists.
	ActTemplateManage  Action = "check.template"
	ActChecklistCreate Action = "check.create"
	ActChecklistRead   Action = "check.read"
	ActTaskComplete    Action = "check.complete"
	ActTaskListOwn     Action = "check.list_own"
	ActOneOnOneCreate  Action = "1on1.create"   // start a series with a direct report
	ActOneOnOneRead    Action = "1on1.read"     // shared parts: schedule, agenda, shared notes, actions
	ActOneOnOneWrite   Action = "1on1.write"    // add to the shared parts
	ActOneOnOnePrivate Action = "1on1.private"  // read or write the manager's private notes
	ActGoalCreate      Action = "goal.create"   // Fact.Scope says what kind
	ActGoalRead        Action = "goal.read"     // Fact.Visibility says how visible
	ActGoalWrite       Action = "goal.write"    // update, key results, progress
	ActGoalListOwn     Action = "goal.list_own" // list goals the caller owns
)

// Fact is what the service knows about the relationship between the actor and the thing
// acted on (a person, or the owner/report of a goal or 1:1).
type Fact struct {
	Self     bool   // the actor is the subject person
	Direct   bool   // the actor is the subject's current direct manager
	Chain    bool   // the actor is anywhere in the subject's manager chain (includes Direct)
	SameTeam bool   // actor and subject share a team
	TeamLead bool   // the actor leads the team the subject (or team goal) belongs to
	Assigned bool   // the actor is the assignee of the task
	Status   Status // the subject person's status ("" for team/company goals)

	Scope      string // goal scope: person | team | company
	Visibility string // goal visibility: private | manager | team | company
}

// Actor is the authenticated caller after the service resolved their own person record.
// HasPerson is false for service accounts and operators without a person record.
type Actor struct {
	Subject   string
	Roles     []string
	PersonID  string
	HasPerson bool
	Status    Status
	// Restricted marks a caller whose token carries explicit permissions (a personal access token
	// from can-core): the role says what kind of caller it is, Perms says what it may do. Static
	// service tokens are not restricted.
	Restricted bool
	Perms      []string
}

// permits reports whether a restricted actor holds the permission; an unrestricted one always does.
func (a Actor) permits(perm string) bool {
	if !a.Restricted {
		return true
	}
	for _, p := range a.Perms {
		if p == perm {
			return true
		}
	}
	return false
}

// actionPerm is the permission a restricted caller needs for an action, whatever its role.
var actionPerm = map[Action]string{
	ActDirCreate: "people.write", ActDirUpdate: "people.write", ActDirOrg: "people.write", ActDirStatus: "people.write",
	ActTemplateManage: "people.write",
	ActLegalHold:      "people.admin", ActErasure: "people.admin", ActRetention: "people.admin",
	ActOneOnOneRead: "performance.read", ActGoalRead: "performance.read",
	ActOneOnOneCreate: "performance.write", ActOneOnOneWrite: "performance.write", ActOneOnOnePrivate: "performance.write",
	ActGoalCreate: "performance.write", ActGoalWrite: "performance.write",
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
	return a.has(RoleEmployee) || a.has(RoleManager) || a.has(RoleHR) || a.has(RoleAdmin)
}

// blocked is the fail-closed gate. A caller with no recognised role is blocked, and so is a
// caller whose own person record says they are not (or no longer) staff: a terminated manager
// keeps a valid SSO token for a while and must not keep any access.
func (a Actor) blocked() bool {
	if !a.known() {
		return true
	}
	if a.HasPerson {
		switch a.Status {
		case StOnboarding, StActive, StLeave, StOffboarding:
		default: // candidate, former, unknown
			return true
		}
	}
	return false
}

// elevated reports whether the actor may use a privileged role. Elevated roles need the
// person to be active: someone on leave or serving out notice keeps self-service only.
// Service accounts without a person record may hold hr or admin, never manager.
func (a Actor) elevated(role string) bool {
	if a.blocked() || !a.has(role) {
		return false
	}
	switch role {
	case RoleHR, RoleAdmin:
		return !a.HasPerson || a.Status == StActive
	case RoleManager:
		return a.HasPerson && a.Status == StActive
	}
	return false
}

func (a Actor) staff() bool           { return !a.blocked() && a.HasPerson && a.has(RoleEmployee) }
func (a Actor) hrAdmin() bool         { return a.elevated(RoleHR) || a.elevated(RoleAdmin) }
func (a Actor) managerOf(f Fact) bool { return a.elevated(RoleManager) && f.Direct }
func (a Actor) chainOf(f Fact) bool   { return a.elevated(RoleManager) && f.Chain }
func (a Actor) selfStaff(f Fact) bool { return a.staff() && f.Self }

func visibleStatus(s Status) bool { return s == StActive || s == StLeave || s == StOnboarding }

// Decide answers whether the actor may perform the action given the facts.
func Decide(a Actor, act Action, f Fact) bool {
	if a.blocked() {
		return false
	}
	if need, ok := actionPerm[act]; ok && !a.permits(need) {
		return false
	}
	switch act {
	case ActDirRead:
		return a.selfStaff(f) || a.hrAdmin() || a.chainOf(f) || (a.staff() && visibleStatus(f.Status))
	case ActDirCreate, ActDirUpdate, ActDirOrg, ActDirStatus, ActLegalHold, ActErasure, ActRetention, ActTemplateManage:
		return a.hrAdmin()
	case ActDirUpdateSelf:
		return a.selfStaff(f)
	case ActDirHistory:
		return a.selfStaff(f) || a.hrAdmin()

	case ActChecklistCreate:
		return a.hrAdmin() || a.managerOf(f)
	case ActChecklistRead:
		return a.selfStaff(f) || a.hrAdmin() || a.managerOf(f)
	case ActTaskComplete:
		return (a.staff() && f.Assigned) || a.hrAdmin()
	case ActTaskListOwn:
		return a.staff()

	case ActOneOnOneCreate, ActOneOnOnePrivate:
		return a.managerOf(f)
	case ActOneOnOneRead, ActOneOnOneWrite:
		return a.selfStaff(f) || a.managerOf(f)

	case ActGoalListOwn:
		return a.staff()
	case ActGoalCreate:
		switch f.Scope {
		case "person":
			return a.selfStaff(f) || a.managerOf(f)
		case "team":
			return a.hrAdmin() || (a.elevated(RoleManager) && f.TeamLead)
		case "company":
			return a.hrAdmin()
		}
		return false
	case ActGoalWrite:
		switch f.Scope {
		case "person":
			return a.selfStaff(f) || a.managerOf(f)
		case "team":
			return a.hrAdmin() || (a.elevated(RoleManager) && f.TeamLead)
		case "company":
			return a.hrAdmin()
		}
		return false
	case ActGoalRead:
		if f.Scope == "person" && a.selfStaff(f) {
			return true // owners always see their own goals
		}
		switch f.Visibility {
		case "private":
			return false
		case "manager":
			return a.chainOf(f) || a.hrAdmin()
		case "team":
			return a.chainOf(f) || a.hrAdmin() || (a.staff() && (f.SameTeam || f.TeamLead))
		case "company":
			return a.staff() || a.hrAdmin()
		}
		return false
	}
	return false
}

// Sensitive fields (data class people-sensitive). Reads and writes are per field.
const (
	FieldDOB        = "date_of_birth"
	FieldNationalID = "national_id"
	FieldAddress    = "home_address"
	FieldPhone      = "personal_phone"
	FieldSalary     = "salary"
)

// SensitiveFields lists every sensitive field in a stable order.
var SensitiveFields = []string{FieldDOB, FieldNationalID, FieldAddress, FieldPhone, FieldSalary}

// IsSensitiveField reports whether name is a sensitive field.
func IsSensitiveField(name string) bool {
	for _, f := range SensitiveFields {
		if f == name {
			return true
		}
	}
	return false
}

// FieldAccess decides per-field read or write access to a sensitive field.
//
// Read: the person themself, hr and admin; managers (direct or skip-level) never.
// Write: hr and admin; the person themself only for contact data (address, phone).
func FieldAccess(a Actor, write bool, field string, f Fact) bool {
	if a.blocked() || !IsSensitiveField(field) {
		return false
	}
	need := "people.sensitive-read"
	if write {
		need = "people.sensitive-write"
	}
	if !a.permits(need) {
		return false
	}
	if a.hrAdmin() {
		return true
	}
	if !a.selfStaff(f) {
		return false
	}
	if !write {
		return true
	}
	return field == FieldAddress || field == FieldPhone
}

// Self-service basic fields: what a person may change about their own directory entry.
var selfEditableBasic = map[string]bool{"preferred_name": true, "work_phone": true}
