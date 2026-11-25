// Package provision is the in-house provisioning module (epic E13d, specs/028-identity-provision):
// the `identity.provision` capability. It has two halves that share one SQLite database:
//
//   - a SCIM 2.0 server (RFC 7643 and 7644, a documented subset) that exposes the people
//     directory to external applications and identity providers, read-mostly, never with sensitive
//     fields and never with the leave status;
//   - a provisioning engine: joiner, mover and leaver workflows made of ordered, idempotent,
//     retried, compensated steps against Connector adapters, with a state machine persisted in
//     SQLite so a crash resumes where it stopped.
//
// Nothing here talks to a real identity provider or SaaS: the adapters are a SCIM client and a
// signed generic webhook (both SSRF-guarded) plus documented, unverified stubs.
package provision

import (
	"errors"
	"regexp"
	"time"
)

// Version is set at build time with -ldflags "-X .../core/provision.Version=...".
var Version = "dev"

// Kind is a workflow.
type Kind string

// Workflows.
const (
	KindJoiner Kind = "joiner"
	KindMover  Kind = "mover"
	KindLeaver Kind = "leaver"
)

// Valid reports whether k is a known workflow.
func (k Kind) Valid() bool { return k == KindJoiner || k == KindMover || k == KindLeaver }

// Run states.
const (
	RunPending      = "pending"
	RunRunning      = "running"
	RunScheduled    = "scheduled" // waiting for an effective date
	RunAwaiting     = "awaiting_approval"
	RunSucceeded    = "succeeded"
	RunFailed       = "failed"
	RunCompensating = "compensating"
	RunCompensated  = "compensated"
	RunCancelled    = "cancelled"
)

// Step states.
const (
	StepPending     = "pending"
	StepRunning     = "running"
	StepAwaiting    = "awaiting_approval"
	StepDone        = "done"
	StepDead        = "dead" // dead-lettered: retries exhausted or a permanent failure
	StepSkipped     = "skipped"
	StepRejected    = "rejected"
	StepCompPending = "comp_pending"
	StepCompensated = "compensated"
	StepCompFailed  = "comp_failed"
)

// Step operations. The connector named in the step performs them, except opRevokeCore (can-core)
// and opCreateTasks (the people module), which have fixed targets.
const (
	opCreateAccount  = "create_account"
	opUpdateAccount  = "update_account"
	opDisableAccount = "disable_account"
	opDeleteAccount  = "delete_account"
	opAddGroup       = "add_group"
	opRemoveGroup    = "remove_group"
	opRevokeCore     = "revoke_core"
	opCreateTasks    = "create_tasks"

	connCore  = "can-core"
	connTasks = "people-tasks"
)

// Run is a workflow instance for one person. It carries ids and machine values only.
type Run struct {
	ID            string `json:"id"`
	Kind          Kind   `json:"kind"`
	PartyID       string `json:"party_id"`
	State         string `json:"state"`
	Trigger       string `json:"trigger"`
	Urgent        bool   `json:"urgent"`
	DryRun        bool   `json:"dry_run"`
	Privileged    bool   `json:"privileged"`
	EffectiveDate string `json:"effective_date,omitempty"`
	Initiator     string `json:"initiator,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	Steps         []Step `json:"steps,omitempty"`

	key string
}

// Step is one ordered unit of a run.
type Step struct {
	Ord         int    `json:"ord"`
	Op          string `json:"op"`
	Connector   string `json:"connector"`
	Arg         string `json:"arg,omitempty"` // a group name
	State       string `json:"state"`
	Sensitive   bool   `json:"sensitive,omitempty"`
	Optional    bool   `json:"optional,omitempty"`
	CompOp      string `json:"compensation,omitempty"`
	Attempts    int    `json:"attempts"`
	NotBefore   string `json:"not_before,omitempty"`
	NextAt      string `json:"next_attempt_at,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`

	externalID string
	waits      int
	gen        int
	notBefore  int64
	nextAt     int64
}

// Errors of the engine and API.
var (
	ErrNotFound    = errors.New("not found")
	ErrUnavailable = errors.New("a dependency is unavailable")
	ErrConflict    = errors.New("conflict")
	ErrInvalid     = errors.New("invalid request")
	ErrForbidden   = errors.New("forbidden")
)

var (
	partyRe   = regexp.MustCompile(`^pers_[0-9a-f]{24}$`)
	runRe     = regexp.MustCompile(`^run_[0-9a-f]{24}$`)
	groupRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:@/+-]{0,99}$`)
	subjectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,127}$`)
	dateRe    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	keyRe     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	extIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@+=-]{0,255}$`)
)

func ts(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

func terminalRun(state string) bool {
	switch state {
	case RunSucceeded, RunFailed, RunCompensated, RunCancelled:
		return true
	}
	return false
}
