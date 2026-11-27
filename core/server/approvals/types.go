// Package approvals is can-core's approval engine (SPEC §3.7, epic E11a): one place where
// "request, approve, record" happens for purchase orders, expenses, timesheets, offers and the
// like. Modules declare the approval types they use (module.yaml `approvals`), the operator sets
// who approves in company.yaml (`approvals:` policies), a module creates a request through the
// module API and receives a signed `approval.decided.v1` webhook when it ends.
//
// Rules the engine enforces, whatever the policy says:
//   - separation of duties: a requester can never approve or reject their own request, not
//     directly and not through a delegation, and one person counts once per request;
//   - fail closed: a request is refused when no step can be given an approver, and a later step
//     that cannot be staffed rejects the request instead of leaving it open;
//   - every vote, comment, cancellation, expiry and delegation is audited in the same
//     transaction as the change (an audit failure rolls the change back);
//   - events and notifications carry identifiers only.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dodwmd/companyinacan/core/server/notify"
)

// Errors returned by the engine; the API layers map them to HTTP statuses.
var (
	ErrInvalid             = errors.New("invalid approval request")
	ErrNotFound            = errors.New("no such approval request")
	ErrNoPolicy            = errors.New("no approval policy for this type")
	ErrNoApprover          = errors.New("no approver can be found for a step")
	ErrIdempotencyConflict = errors.New("idempotency key already used with a different request")
	ErrNotPending          = errors.New("the request is no longer pending")
	// ErrRateLimited: the module opened too many requests lately (cancelled ones count too).
	ErrRateLimited  = errors.New("this module has opened too many approval requests; retry later")
	ErrNotEligible  = errors.New("you are not an approver of an open step of this request")
	ErrSelfApproval = errors.New("a requester cannot decide their own request")
	ErrAlreadyVoted = errors.New("you have already voted on this request")
	ErrNoManager    = errors.New("requester has no manager")
	ErrResolver     = errors.New("reporting line lookup failed")
)

// Limits.
const (
	MaxCommentLen   = 2000
	MaxPayloadBytes = 64 << 10
	MaxTitleLen     = 300
	MaxDecisions    = 200
	TickBatch       = 100
	MaxDelegation   = 90 * 24 * time.Hour
)

// ReportingLine resolves a subject's manager. The real implementation calls the `directory`
// capability; tests use a fake.
type ReportingLine interface {
	// ManagerOf returns the manager's subject, or ErrNoManager.
	ManagerOf(ctx context.Context, subject string) (string, error)
}

// NoReportingLine is used until a `directory` provider exists: no requester has a manager, so
// steps that need one use their fallback or refuse the request.
type NoReportingLine struct{}

// ManagerOf implements ReportingLine.
func (NoReportingLine) ManagerOf(context.Context, string) (string, error) { return "", ErrNoManager }

// Notifier delivers notifications (notify.Service satisfies it).
type Notifier interface {
	Notify(ctx context.Context, n notify.Notice) (int64, bool, error)
}

// Request is an approval request.
type Request struct {
	ID            string          `json:"id"`
	ModuleID      string          `json:"module_id"`
	Type          string          `json:"type"`
	Requester     string          `json:"requester"`
	Ref           string          `json:"ref,omitempty"`
	Title         string          `json:"title,omitempty"`
	Amount        int64           `json:"amount"`
	Status        string          `json:"status"`
	OutcomeReason string          `json:"outcome_reason,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	DecidedAt     *time.Time      `json:"decided_at,omitempty"`
}

// Step is one step of a request's chain.
type Step struct {
	Idx       int      `json:"idx"`
	Name      string   `json:"name"`
	Stage     int      `json:"stage"`
	Quorum    int      `json:"quorum"`
	Status    string   `json:"status"` // waiting, active, approved, rejected, skipped
	Roles     []string `json:"roles,omitempty"`
	Users     []string `json:"users,omitempty"`
	Approvals int      `json:"approvals"`
}

// Decision is one entry of the history.
type Decision struct {
	ID         int64     `json:"id"`
	StepIdx    int       `json:"step_idx"`
	Actor      string    `json:"actor"`
	OnBehalfOf string    `json:"on_behalf_of,omitempty"`
	Kind       string    `json:"kind"`
	Comment    string    `json:"comment,omitempty"`
	At         time.Time `json:"at"`
}

// Detail is a request with its steps and history.
type Detail struct {
	Request   Request    `json:"request"`
	Steps     []Step     `json:"steps"`
	Decisions []Decision `json:"decisions"`
}

// CreateRequest is what a module submits.
type CreateRequest struct {
	ModuleID  string
	Type      string
	Requester string // subject: user:<name> or party:<id>
	Ref       string // the module's own id of the thing being approved
	Title     string
	Amount    int64 // minor units; compared with step thresholds
	Payload   json.RawMessage
	// IdempotencyKey makes a retry return the original request.
	IdempotencyKey string
	// DefaultChain is the module manifest's default_chain, used when the company set no policy.
	DefaultChain []string
	// Manager, when set, is the requester's manager as the owner of the reporting lines knows it (only
	// the `directory` provider may say so, see moduleapi): it stands for `manager_of_requester` in
	// this request's copy of the policy.
	Manager string
}

// Delegation lets Delegate act for Delegator during [From, To).
type Delegation struct {
	ID        int64     `json:"id"`
	Delegator string    `json:"delegator"`
	Delegate  string    `json:"delegate"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
}
