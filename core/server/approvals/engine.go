package approvals

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Roles is the part of rbac.Service the engine needs.
type Roles interface {
	HasRole(ctx context.Context, subject, role string) (bool, error)
	SubjectsWithRole(ctx context.Context, role string, limit int) ([]string, error)
	SubjectAssignments(ctx context.Context, subject string) ([]rbac.Assignment, error)
}

// Perms answers permission checks (rbac.Service).
type Perms interface {
	Check(ctx context.Context, subject, permission, resource string) (bool, error)
}

// Appender writes audit entries inside a transaction.
type Appender interface {
	AppendTx(ctx context.Context, tx *sql.Tx, e audit.Entry) (audit.Record, error)
}

// Publisher publishes events (events.Bus).
type Publisher interface {
	PublishTo(ctx context.Context, source, eventType string, data json.RawMessage, onlyModule string) (events.Envelope, error)
}

// Deps are the engine's collaborators. Lines, Notifier and Bus may be nil.
type Deps struct {
	Roles    Roles
	Perms    Perms
	Lines    ReportingLine
	Notifier Notifier
	Bus      Publisher
	Log      *slog.Logger
	Now      func() time.Time
}

// Engine is the approval engine.
type Engine struct {
	db    *store.DB
	audit Appender
	d     Deps
	// mu serialises mutations: can-core is one process with one SQLite connection, and every
	// decision reads state, resolves approvers (further queries) and then writes in a transaction.
	mu     sync.Mutex
	failed map[string]time.Time // timer rows that failed, skipped until the time shown
}

// New returns an Engine.
func New(db *store.DB, a Appender, d Deps) *Engine {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Lines == nil {
		d.Lines = NoReportingLine{}
	}
	return &Engine{db: db, audit: a, d: d}
}

func (e *Engine) now() time.Time { return e.d.Now() }

var (
	typeRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	idRe   = regexp.MustCompile(`^apr_[0-9a-f]{24}$`)
)

// ValidID reports whether s looks like a request id.
func ValidID(s string) bool { return idRe.MatchString(s) }

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "apr_" + hex.EncodeToString(b[:])
}

type approverSet struct {
	Roles []string `json:"roles,omitempty"`
	Users []string `json:"users,omitempty"`
}

func (a *approverSet) merge(o approverSet) {
	for _, r := range o.Roles {
		if !contains(a.Roles, r) {
			a.Roles = append(a.Roles, r)
		}
	}
	for _, u := range o.Users {
		if !contains(a.Users, u) {
			a.Users = append(a.Users, u)
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// resolve turns approver specs into a set and reports whether enough people could act: at least
// quorum distinct approvers other than the requester.
func (e *Engine) resolve(ctx context.Context, module string, specs []config.Approver, requester string, quorum int) (approverSet, bool, error) {
	var set approverSet
	people := map[string]bool{}
	big := false
	for _, a := range specs {
		switch {
		case a.Role != "":
			// A bare role name in a policy means the role of the module the request belongs to;
			// other modules' roles are written "module:role".
			role := rbac.QualifyRole(module, a.Role)
			set.merge(approverSet{Roles: []string{role}})
			holders, err := e.d.Roles.SubjectsWithRole(ctx, role, 51)
			if err != nil {
				return set, false, err
			}
			big = big || len(holders) > 50
			for _, h := range holders {
				people[h] = true
			}
		case a.User != "":
			set.merge(approverSet{Users: []string{a.User}})
			people[a.User] = true
		case a.ManagerOfRequester:
			m, err := e.d.Lines.ManagerOf(ctx, requester)
			if errors.Is(err, ErrNoManager) {
				continue
			}
			if err != nil {
				return set, false, fmt.Errorf("%w: %v", ErrResolver, err)
			}
			if m == "" || m == requester || !rbac.ValidSubject(m) {
				continue
			}
			set.merge(approverSet{Users: []string{m}})
			people[m] = true
		}
	}
	delete(people, requester)
	return set, big || len(people) >= quorum, nil
}

func (e *Engine) resolveStep(ctx context.Context, module string, s config.ApprovalStep, requester string) (approverSet, error) {
	q := s.Quorum
	if q < 1 {
		q = 1
	}
	set, ok, err := e.resolve(ctx, module, s.Approvers, requester, q)
	if err != nil {
		return set, err
	}
	if ok {
		return set, nil
	}
	if len(s.Fallback) > 0 {
		set, ok, err = e.resolve(ctx, module, s.Fallback, requester, q)
		if err != nil {
			return set, err
		}
		if ok {
			return set, nil
		}
	}
	return approverSet{}, fmt.Errorf("%w: step %q", ErrNoApprover, s.Name)
}

func stepApplies(s config.ApprovalStep, amount int64) bool {
	if s.When == nil {
		return true
	}
	if s.When.AmountGTE != nil && amount < *s.When.AmountGTE {
		return false
	}
	if s.When.AmountLT != nil && amount >= *s.When.AmountLT {
		return false
	}
	return true
}

type stepRow struct {
	idx, stage, quorum int
	name, status       string
	set                approverSet
}

func requestHash(r CreateRequest) string {
	parts := []any{r.Type, r.Requester, r.Ref, r.Title, r.Amount, string(r.Payload)}
	if r.Manager != "" { // only then, so a repeat of a request made before this field still matches
		parts = append(parts, r.Manager)
	}
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (r *CreateRequest) validate() error {
	if !typeRe.MatchString(r.Type) || r.ModuleID == "" {
		return fmt.Errorf("%w: type", ErrInvalid)
	}
	if !rbac.ValidSubject(r.Requester) {
		return fmt.Errorf("%w: requester must be user:<name> or party:<id>", ErrInvalid)
	}
	if r.Manager != "" && !rbac.ValidSubject(r.Manager) {
		return fmt.Errorf("%w: manager must be user:<name> or party:<id>", ErrInvalid)
	}
	if r.IdempotencyKey == "" || len(r.IdempotencyKey) > 200 {
		return fmt.Errorf("%w: idempotency key is required (at most 200 characters)", ErrInvalid)
	}
	if r.Amount < 0 || len(r.Title) > MaxTitleLen || len(r.Ref) > 200 {
		return fmt.Errorf("%w: amount, title or ref out of range", ErrInvalid)
	}
	if len(r.Payload) > MaxPayloadBytes {
		return fmt.Errorf("%w: payload larger than %d bytes", ErrInvalid, MaxPayloadBytes)
	}
	if len(r.Payload) == 0 {
		r.Payload = json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(r.Payload, &obj) != nil {
		return fmt.Errorf("%w: payload must be a JSON object", ErrInvalid)
	}
	return nil
}

// The most new approval requests one module may open.
const (
	MaxPerModuleHour = 120
	MaxPerModuleDay  = 600
)

// Create opens a request. It is idempotent per (module, key): a repeat of the same request
// returns the original with created=false; the same key with different content is refused.
func (e *Engine) Create(ctx context.Context, cr CreateRequest) (Request, bool, error) {
	if err := cr.validate(); err != nil {
		return Request{}, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	hash := requestHash(cr)
	var existingID, existingHash string
	err := e.db.QueryRowContext(ctx, `SELECT id, request_hash FROM approval_requests WHERE module_id=? AND idem_key=?`, cr.ModuleID, cr.IdempotencyKey).Scan(&existingID, &existingHash)
	switch {
	case err == nil:
		if existingHash != hash {
			return Request{}, false, ErrIdempotencyConflict
		}
		r, err := e.loadRequest(ctx, existingID)
		return r, false, err
	case !errors.Is(err, sql.ErrNoRows):
		return Request{}, false, err
	}
	// Each new request notifies people, so one module may not open them without end. A repeat of an
	// earlier one (above) does not count.
	for _, l := range []struct {
		window time.Duration
		max    int
	}{{time.Hour, MaxPerModuleHour}, {24 * time.Hour, MaxPerModuleDay}} {
		var n int
		if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approval_requests WHERE module_id=? AND created_at>?`, cr.ModuleID, e.now().Add(-l.window).Unix()).Scan(&n); err != nil {
			return Request{}, false, err
		}
		if n >= l.max {
			return Request{}, false, ErrRateLimited
		}
	}
	pol, err := e.policyFor(ctx, cr.Type, cr.DefaultChain)
	if err != nil {
		return Request{}, false, err
	}
	if cr.Manager != "" {
		pol = nameManager(pol, cr.Manager)
	}
	// Build the steps and staff the first stage before writing anything.
	var rows []stepRow
	stage := -1
	for i, s := range pol.Steps {
		if !s.Parallel {
			stage++
		}
		q := s.Quorum
		if q < 1 {
			q = 1
		}
		rows = append(rows, stepRow{idx: i, stage: stage, quorum: q, name: s.Name, status: "waiting"})
		if !stepApplies(s, cr.Amount) {
			rows[i].status = "skipped"
		}
	}
	first := firstStage(rows, -1)
	if first < 0 {
		return Request{}, false, fmt.Errorf("%w: no step of the policy applies to this request", ErrNoApprover)
	}
	for i := range rows {
		if rows[i].stage == first && rows[i].status == "waiting" {
			set, err := e.resolveStep(ctx, cr.ModuleID, pol.Steps[i], cr.Requester)
			if err != nil {
				return Request{}, false, err
			}
			rows[i].set, rows[i].status = set, "active"
		}
	}
	now := e.now()
	var expires, escalate int64
	if pol.ExpiresAfter != "" {
		d, _ := ParseDuration(pol.ExpiresAfter)
		expires = now.Add(d).Unix()
	}
	if pol.EscalateAfter != "" {
		d, _ := ParseDuration(pol.EscalateAfter)
		escalate = now.Add(d).Unix()
	}
	pj, _ := json.Marshal(pol)
	id := newID()
	err = e.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_requests(id, module_id, type, requester, ref, title, amount, payload_json, status, idem_key, request_hash, policy_json, created_at, expires_at, escalate_at)
			VALUES (?,?,?,?,?,?,?,?,'pending',?,?,?,?,?,?)`,
			id, cr.ModuleID, cr.Type, cr.Requester, cr.Ref, cr.Title, cr.Amount, string(cr.Payload), cr.IdempotencyKey, hash, string(pj), now.Unix(), expires, escalate); err != nil {
			return err
		}
		for _, s := range rows {
			sj, _ := json.Marshal(s.set)
			if _, err := tx.ExecContext(ctx, `INSERT INTO approval_steps(request_id, idx, stage, name, quorum, status, approvers) VALUES (?,?,?,?,?,?,?)`,
				id, s.idx, s.stage, s.name, s.quorum, s.status, string(sj)); err != nil {
				return err
			}
		}
		_, err := e.audit.AppendTx(ctx, tx, audit.Entry{Actor: "module:" + cr.ModuleID, Action: "approval.request", Target: id,
			Detail: map[string]any{"type": cr.Type, "requester": cr.Requester, "ref": cr.Ref}})
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") { // lost a race with the same key
			var eid string
			if qerr := e.db.QueryRowContext(ctx, `SELECT id FROM approval_requests WHERE module_id=? AND idem_key=?`, cr.ModuleID, cr.IdempotencyKey).Scan(&eid); qerr == nil {
				r, lerr := e.loadRequest(ctx, eid)
				return r, false, lerr
			}
		}
		return Request{}, false, err
	}
	req, err := e.loadRequest(ctx, id)
	if err != nil {
		return Request{}, false, err
	}
	e.publish(ctx, "approval.requested.v1", req, map[string]any{"amount": req.Amount})
	for _, s := range rows {
		if s.status == "active" {
			e.notifyApprovers(ctx, req, s.idx, s.set, "approval.requested")
		}
	}
	return req, true, nil
}

// firstStage returns the lowest stage above after with a step still to run, or -1.
func firstStage(rows []stepRow, after int) int {
	best := -1
	for _, s := range rows {
		if s.stage > after && s.status != "skipped" && (best == -1 || s.stage < best) {
			best = s.stage
		}
	}
	return best
}

func (e *Engine) publish(ctx context.Context, typ string, r Request, extra map[string]any) {
	if e.d.Bus == nil {
		return
	}
	data := map[string]any{"request_id": r.ID, "module_id": r.ModuleID, "type": r.Type, "ref": r.Ref, "requester": r.Requester}
	for k, v := range extra {
		data[k] = v
	}
	b, _ := json.Marshal(data)
	if _, err := e.d.Bus.PublishTo(ctx, "can-core", typ, b, r.ModuleID); err != nil {
		e.d.Log.Error("publishing approval event failed", "event", typ, "request", r.ID, "error", err)
	}
}

// notifyApprovers tells the people of a step; the requester is never notified as an approver.
func (e *Engine) notifyApprovers(ctx context.Context, r Request, step int, set approverSet, typ string) {
	if e.d.Notifier == nil {
		return
	}
	subjects := map[string]bool{}
	for _, u := range set.Users {
		subjects[u] = true
	}
	for _, role := range set.Roles {
		hs, err := e.d.Roles.SubjectsWithRole(ctx, role, 50)
		if err != nil {
			e.d.Log.Error("resolving approvers to notify failed", "role", role, "error", err)
			continue
		}
		for _, h := range hs {
			subjects[h] = true
		}
	}
	delete(subjects, r.Requester)
	for s := range subjects {
		_, _, err := e.d.Notifier.Notify(ctx, notify.Notice{Subject: s, Type: typ, DedupeKey: fmt.Sprintf("%s:%s:%d", r.ID, typ, step),
			Data: map[string]string{"request_id": r.ID, "approval_type": r.Type}, Link: "/approvals/" + r.ID, Module: r.ModuleID, Platform: true})
		if err != nil {
			e.d.Log.Error("approval notification failed", "request", r.ID, "error", err)
		}
	}
}

func (e *Engine) notifyRequester(ctx context.Context, r Request, outcome string) {
	if e.d.Notifier == nil {
		return
	}
	_, _, err := e.d.Notifier.Notify(ctx, notify.Notice{Subject: r.Requester, Type: "approval.decided", DedupeKey: r.ID + ":decided",
		Data: map[string]string{"request_id": r.ID, "approval_type": r.Type, "outcome": outcome}, Link: "/approvals/" + r.ID, Module: r.ModuleID, Platform: true})
	if err != nil {
		e.d.Log.Error("approval notification failed", "request", r.ID, "error", err)
	}
}

// eligibility ------------------------------------------------------------------------------------

func (e *Engine) holds(ctx context.Context, subject string, set approverSet) (bool, error) {
	if contains(set.Users, subject) {
		return true, nil
	}
	for _, r := range set.Roles {
		ok, err := e.d.Roles.HasRole(ctx, subject, r)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// activeDelegators lists who has delegated to subject right now.
func (e *Engine) activeDelegators(ctx context.Context, subject string) ([]string, error) {
	now := e.now().Unix()
	rows, err := e.db.QueryContext(ctx, `SELECT DISTINCT delegator FROM approval_delegations WHERE delegate=? AND revoked=0 AND from_ts<=? AND to_ts>? LIMIT 20`, subject, now, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// route finds the active step actor may vote on and on whose behalf.
func (e *Engine) route(ctx context.Context, req Request, steps []Step, actor string) (step Step, onBehalf string, err error) {
	delegators, err := e.activeDelegators(ctx, actor)
	if err != nil {
		return Step{}, "", err
	}
	selfBlocked := false
	for _, s := range steps {
		if s.Status != "active" {
			continue
		}
		set := approverSet{Roles: s.Roles, Users: s.Users}
		ok, err := e.holds(ctx, actor, set)
		if err != nil {
			return Step{}, "", err
		}
		if ok {
			return s, "", nil
		}
		for _, d := range delegators {
			ok, err := e.holds(ctx, d, set)
			if err != nil {
				return Step{}, "", err
			}
			if ok && d == req.Requester {
				selfBlocked = true
			} else if ok {
				return s, d, nil
			}
		}
	}
	if selfBlocked {
		return Step{}, "", ErrSelfApproval
	}
	return Step{}, "", ErrNotEligible
}

// Decide records actor's vote (decision is "approve" or "reject") on the request.
func (e *Engine) Decide(ctx context.Context, id, actor, decision, comment string) (Request, error) {
	if decision != "approve" && decision != "reject" {
		return Request{}, fmt.Errorf("%w: decision must be approve or reject", ErrInvalid)
	}
	if len(comment) > MaxCommentLen {
		return Request{}, fmt.Errorf("%w: comment longer than %d characters", ErrInvalid, MaxCommentLen)
	}
	if !rbac.ValidSubject(actor) {
		return Request{}, ErrNotEligible
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.load(ctx, id)
	if err != nil {
		return Request{}, err
	}
	req := d.Request
	if req.Status != "pending" {
		return Request{}, ErrNotPending
	}
	if actor == req.Requester {
		return Request{}, ErrSelfApproval
	}
	step, onBehalf, err := e.route(ctx, req, d.Steps, actor)
	if err != nil {
		return Request{}, err
	}
	who := []any{actor, actor}
	if onBehalf != "" {
		who = []any{actor, onBehalf}
	}
	var voted int
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approval_decisions WHERE request_id=? AND kind IN ('approve','reject') AND (actor IN (?,?) OR on_behalf_of IN (?,?))`,
		append(append([]any{id}, who...), who...)...).Scan(&voted); err != nil {
		return Request{}, err
	}
	if voted > 0 {
		return Request{}, ErrAlreadyVoted
	}

	// Work out the consequence before opening the transaction (resolution queries the database).
	final, reason := "", ""
	var activate []stepRow
	stepDone := false
	if decision == "reject" {
		final = "rejected"
	} else {
		stepDone = step.Approvals+1 >= step.Quorum
		if stepDone {
			stageDone := true
			for _, s := range d.Steps {
				if s.Stage == step.Stage && s.Status != "skipped" && s.Idx != step.Idx && s.Status != "approved" {
					stageDone = false
				}
			}
			if stageDone {
				var pol config.ApprovalPolicy
				if err := json.Unmarshal([]byte(d.policyJSON), &pol); err != nil {
					return Request{}, err
				}
				rows := make([]stepRow, 0, len(d.Steps))
				for _, s := range d.Steps {
					rows = append(rows, stepRow{idx: s.Idx, stage: s.Stage, status: s.Status})
				}
				if next := firstStage(rows, step.Stage); next < 0 {
					final = "approved"
				} else {
					for i, s := range d.Steps {
						if s.Stage != next || s.Status != "waiting" {
							continue
						}
						set, err := e.resolveStep(ctx, req.ModuleID, pol.Steps[i], req.Requester)
						if errors.Is(err, ErrNoApprover) {
							final, reason, activate = "rejected", "no_approver", nil
							break
						}
						if err != nil {
							return Request{}, err
						}
						activate = append(activate, stepRow{idx: s.Idx, set: set})
					}
				}
			}
		}
	}

	now := e.now().Unix()
	err = e.db.InTx(ctx, func(tx *sql.Tx) error {
		var status string
		var expires int64
		if err := tx.QueryRowContext(ctx, `SELECT status, expires_at FROM approval_requests WHERE id=?`, id).Scan(&status, &expires); err != nil {
			return err
		}
		// Expiry holds even if the ticker has not run yet (or is failing).
		if status != "pending" || (expires > 0 && expires <= now) {
			return ErrNotPending
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_decisions(request_id, step_idx, actor, on_behalf_of, kind, comment, at) VALUES (?,?,?,?,?,?,?)`,
			id, step.Idx, actor, onBehalf, decision, comment, now); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrAlreadyVoted
			}
			return err
		}
		switch {
		case decision == "reject":
			if _, err := tx.ExecContext(ctx, `UPDATE approval_steps SET status='rejected' WHERE request_id=? AND idx=?`, id, step.Idx); err != nil {
				return err
			}
		case stepDone:
			if _, err := tx.ExecContext(ctx, `UPDATE approval_steps SET status='approved' WHERE request_id=? AND idx=?`, id, step.Idx); err != nil {
				return err
			}
		}
		for _, a := range activate {
			sj, _ := json.Marshal(a.set)
			if _, err := tx.ExecContext(ctx, `UPDATE approval_steps SET status='active', approvers=? WHERE request_id=? AND idx=?`, string(sj), id, a.idx); err != nil {
				return err
			}
		}
		if final != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE approval_requests SET status=?, outcome_reason=?, decided_at=? WHERE id=?`, final, reason, now, id); err != nil {
				return err
			}
			if reason != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO approval_decisions(request_id, step_idx, actor, kind, comment, at) VALUES (?,?,?,?,?,?)`, id, -1, "system", "system", reason, now); err != nil {
					return err
				}
			}
		}
		detail := map[string]any{"step": step.Name}
		if onBehalf != "" {
			detail["on_behalf_of"] = onBehalf
		}
		if final != "" {
			detail["outcome"] = final
		}
		_, err := e.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "approval." + decision, Target: id, Detail: detail})
		return err
	})
	if err != nil {
		return Request{}, err
	}
	req, err = e.loadRequest(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if final != "" {
		e.finished(ctx, req)
	}
	for _, a := range activate {
		if final == "" {
			e.notifyApprovers(ctx, req, a.idx, a.set, "approval.requested")
		}
	}
	return req, nil
}

// finished publishes the decided event and tells the requester.
func (e *Engine) finished(ctx context.Context, r Request) {
	approvers := []string{}
	rows, err := e.db.QueryContext(ctx, `SELECT DISTINCT actor FROM approval_decisions WHERE request_id=? AND kind='approve' ORDER BY actor LIMIT 50`, r.ID)
	if err == nil {
		for rows.Next() {
			var a string
			if rows.Scan(&a) == nil {
				approvers = append(approvers, a)
			}
		}
		_ = rows.Close()
	}
	extra := map[string]any{"outcome": r.Status, "approvers": approvers}
	if r.DecidedAt != nil {
		extra["decided_at"] = r.DecidedAt.UTC().Format(time.RFC3339)
	}
	if r.OutcomeReason != "" {
		extra["reason"] = r.OutcomeReason
	}
	e.publish(ctx, "approval.decided.v1", r, extra)
	e.notifyRequester(ctx, r, r.Status)
}

// Comment adds a comment to the history. Requester, approvers (past or present) and approvals
// admins may comment.
func (e *Engine) Comment(ctx context.Context, id, actor, text string) error {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > MaxCommentLen {
		return fmt.Errorf("%w: a comment is 1 to %d characters", ErrInvalid, MaxCommentLen)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.load(ctx, id)
	if err != nil {
		return err
	}
	if !e.CanView(ctx, d.Detail, actor) {
		return ErrNotEligible
	}
	if len(d.Decisions) >= MaxDecisions {
		return fmt.Errorf("%w: the history is full", ErrInvalid)
	}
	return e.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_decisions(request_id, step_idx, actor, kind, comment, at) VALUES (?,?,?,?,?,?)`, id, -1, actor, "comment", text, e.now().Unix()); err != nil {
			return err
		}
		_, err := e.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "approval.comment", Target: id})
		return err
	})
}

// Cancel withdraws a pending request. The requester, an approvals admin, or the owning module
// (actor "module:<id>") may.
func (e *Engine) Cancel(ctx context.Context, id, actor, reason string) error {
	if len(reason) > MaxCommentLen {
		return fmt.Errorf("%w: reason too long", ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	req, err := e.loadRequest(ctx, id)
	if err != nil {
		return err
	}
	allowed := actor == req.Requester || actor == "module:"+req.ModuleID
	if !allowed && e.d.Perms != nil {
		ok, err := e.d.Perms.Check(ctx, actor, rbac.PermApprovalAdmin, "")
		if err != nil {
			return err
		}
		allowed = ok
	}
	if !allowed {
		return ErrNotEligible
	}
	return e.finish(ctx, req, "cancelled", "cancel", actor, reason)
}

// finish ends a pending request without a vote (cancel, expiry).
func (e *Engine) finish(ctx context.Context, req Request, status, kind, actor, comment string) error {
	now := e.now().Unix()
	err := e.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE approval_requests SET status=?, decided_at=? WHERE id=? AND status='pending'`, status, now, req.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotPending
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_decisions(request_id, step_idx, actor, kind, comment, at) VALUES (?,?,?,?,?,?)`, req.ID, -1, actor, kind, comment, now); err != nil {
			return err
		}
		_, err = e.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "approval." + kind, Target: req.ID, Detail: map[string]any{"outcome": status}})
		return err
	})
	if err != nil {
		return err
	}
	done, err := e.loadRequest(ctx, req.ID)
	if err != nil {
		return err
	}
	e.finished(ctx, done)
	return nil
}

// Tick expires and escalates requests that are due and returns how many it handled. Each call does
// at most TickBatch units of work; call it every minute or so. A row that fails is skipped for
// failBackoff (and the error reported at the end), so one bad row cannot starve the rest.
func (e *Engine) Tick(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if e.failed == nil {
		e.failed = map[string]time.Time{}
	}
	for id, until := range e.failed {
		if !now.Before(until) {
			delete(e.failed, id)
		}
	}
	var errs []error
	fail := func(id string, err error) {
		e.failed[id] = now.Add(failBackoff)
		e.d.Log.Error("approval timer failed", "request", id, "error", err)
		errs = append(errs, err)
	}
	done := 0
	ids, err := e.dueIDs(ctx, `SELECT id FROM approval_requests WHERE status='pending' AND expires_at>0 AND expires_at<=? ORDER BY expires_at LIMIT ?`, now.Unix(), TickBatch+len(e.failed))
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, bad := e.failed[id]; bad || done >= TickBatch {
			continue
		}
		req, err := e.loadRequest(ctx, id)
		if err == nil {
			err = e.finish(ctx, req, "expired", "expire", "system", "")
		}
		if err != nil && !errors.Is(err, ErrNotPending) {
			fail(id, err)
			continue
		}
		done++
	}
	if done < TickBatch {
		ids, err = e.dueIDs(ctx, `SELECT id FROM approval_requests WHERE status='pending' AND escalated=0 AND escalate_at>0 AND escalate_at<=? ORDER BY escalate_at LIMIT ?`, now.Unix(), TickBatch+len(e.failed))
		if err != nil {
			return done, err
		}
		for _, id := range ids {
			if _, bad := e.failed[id]; bad || done >= TickBatch {
				continue
			}
			if err := e.escalate(ctx, id); err != nil {
				fail(id, err)
				continue
			}
			done++
		}
	}
	return done, errors.Join(errs...)
}

const failBackoff = 5 * time.Minute

func (e *Engine) dueIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (e *Engine) escalate(ctx context.Context, id string) error {
	d, err := e.load(ctx, id)
	if err != nil {
		return err
	}
	var pol config.ApprovalPolicy
	if err := json.Unmarshal([]byte(d.policyJSON), &pol); err != nil {
		return err
	}
	extra, _, err := e.resolve(ctx, d.Request.ModuleID, pol.EscalateTo, d.Request.Requester, 1)
	if err != nil {
		return err
	}
	type upd struct {
		idx int
		set approverSet
	}
	var updates []upd
	for _, s := range d.Steps {
		if s.Status == "active" {
			set := approverSet{Roles: append([]string(nil), s.Roles...), Users: append([]string(nil), s.Users...)}
			set.merge(extra)
			updates = append(updates, upd{s.Idx, set})
		}
	}
	err = e.db.InTx(ctx, func(tx *sql.Tx) error {
		for _, u := range updates {
			sj, _ := json.Marshal(u.set)
			if _, err := tx.ExecContext(ctx, `UPDATE approval_steps SET approvers=? WHERE request_id=? AND idx=?`, string(sj), id, u.idx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approval_requests SET escalated=1 WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_decisions(request_id, step_idx, actor, kind, at) VALUES (?,?,?,?,?)`, id, -1, "system", "escalate", e.now().Unix()); err != nil {
			return err
		}
		_, err := e.audit.AppendTx(ctx, tx, audit.Entry{Actor: "system", Action: "approval.escalate", Target: id})
		return err
	})
	if err != nil {
		return err
	}
	for _, u := range updates {
		e.notifyApprovers(ctx, d.Request, u.idx, extra, "approval.escalated")
	}
	return nil
}
