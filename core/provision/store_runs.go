package provision

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

const runCols = `id,kind,party_id,state,trigger,idem_key,urgent,dry_run,privileged,effective_date,initiator,created_at,updated_at`
const stepCols = `ord,op,connector,arg,state,sensitive,optional,comp_op,attempts,not_before,next_at,approval_id,approved,external_id,error_code,completed_at`

type scanner interface{ Scan(...any) error }

func scanRun(r scanner) (Run, error) {
	var x Run
	var kind string
	var urgent, dry, priv, c, u int64
	if err := r.Scan(&x.ID, &kind, &x.PartyID, &x.State, &x.Trigger, &x.key, &urgent, &dry, &priv, &x.EffectiveDate, &x.Initiator, &c, &u); err != nil {
		return x, err
	}
	x.Kind, x.Urgent, x.DryRun, x.Privileged = Kind(kind), urgent != 0, dry != 0, priv != 0
	x.CreatedAt, x.UpdatedAt = ts(c), ts(u)
	return x, nil
}

type stepRow struct {
	Step
	approved bool
}

func scanStep(r scanner) (stepRow, error) {
	var s stepRow
	var sens, opt, appr, done int64
	if err := r.Scan(&s.Ord, &s.Op, &s.Connector, &s.Arg, &s.State, &sens, &opt, &s.CompOp, &s.Attempts, &s.notBefore, &s.nextAt, &s.ApprovalID, &appr, &s.externalID, &s.ErrorCode, &done); err != nil {
		return s, err
	}
	s.Sensitive, s.Optional, s.approved = sens != 0, opt != 0, appr != 0
	s.NotBefore, s.NextAt, s.CompletedAt = ts(s.notBefore), ts(s.nextAt), ts(done)
	return s, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// insertRun writes a run and its steps inside tx.
func (s *Store) insertRun(ctx context.Context, tx *sql.Tx, r Run, steps []Step) error {
	now := s.now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(`+runCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, string(r.Kind), r.PartyID, r.State, r.Trigger, r.key, b2i(r.Urgent), b2i(r.DryRun), b2i(r.Privileged), r.EffectiveDate, r.Initiator, now, now); err != nil {
		return err
	}
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO steps(run_id,ord,op,connector,arg,state,sensitive,optional,comp_op,not_before) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			r.ID, st.Ord, st.Op, st.Connector, st.Arg, st.State, b2i(st.Sensitive), b2i(st.Optional), st.CompOp, st.notBefore); err != nil {
			return err
		}
	}
	return nil
}

// GetRun returns a run with its steps.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	r, _, err := s.getRunRows(ctx, id)
	return r, err
}

func (s *Store) getRunRows(ctx context.Context, id string) (Run, []stepRow, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil, ErrNotFound
	}
	if err != nil {
		return r, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+stepCols+` FROM steps WHERE run_id=? ORDER BY ord`, id)
	if err != nil {
		return r, nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []stepRow
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return r, nil, err
		}
		out = append(out, st)
		r.Steps = append(r.Steps, st.Step)
	}
	return r, out, rows.Err()
}

// RunFilter selects runs for listing.
type RunFilter struct {
	State   string
	PartyID string
	Kind    string
	After   string // run id cursor (ids sort by creation through created_at then id)
	Limit   int
}

// ListRuns returns runs newest first without their steps.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs
		WHERE (?='' OR state=?) AND (?='' OR party_id=?) AND (?='' OR kind=?) AND (?='' OR id < ?)
		ORDER BY id DESC LIMIT ?`, f.State, f.State, f.PartyID, f.PartyID, f.Kind, f.Kind, f.After, f.After, f.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeadLetters lists runs that have a dead-lettered step (retries exhausted or a permanent failure).
func (s *Store) DeadLetters(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE id IN (SELECT run_id FROM steps WHERE state IN ('dead','comp_failed')) ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	_ = rows.Close()
	for i := range out {
		full, err := s.GetRun(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i] = full
	}
	return out, nil
}

// liveRun returns the live run of a kind for a person, if any.
func (s *Store) liveRun(ctx context.Context, party string, kind Kind) (Run, bool, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE party_id=? AND kind=? AND state IN ('pending','running','scheduled','awaiting_approval','compensating')`, party, string(kind)))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// lastRun returns the newest run of a person, if any.
func (s *Store) lastRun(ctx context.Context, party string) (Run, bool, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE party_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, party))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// runByKey returns the run created for an idempotency key.
func (s *Store) runByKey(ctx context.Context, key string) (Run, bool, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE idem_key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// activeRunIDs returns the runs the worker still has to advance, oldest activity first.
func (s *Store) activeRunIDs(ctx context.Context, limit int) ([]string, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE state IN ('pending','running','scheduled','awaiting_approval','compensating') AND wake_at<=?
		ORDER BY examined_at, id LIMIT ?`, s.now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Accounts of a person: connector -> external id, only the active ones.
func (s *Store) activeAccounts(ctx context.Context, party string) (map[string]string, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT connector, external_id FROM accounts WHERE party_id=? AND state='active'`, party)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var c, e string
		if err := rows.Scan(&c, &e); err != nil {
			return nil, err
		}
		out[c] = e
	}
	return out, rows.Err()
}

// accountFor returns the recorded account (any state) of a person at a connector.
func (s *Store) accountFor(ctx context.Context, party, connector string) (ext, state string, err error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	err = s.db.QueryRowContext(ctx, `SELECT external_id, state FROM accounts WHERE party_id=? AND connector=?`, party, connector).Scan(&ext, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return
}

// LinkAccount records an existing downstream account for a person (administrator action).
func (s *Store) LinkAccount(ctx context.Context, actor, party, connector, ext string) error {
	return s.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(party_id,connector,external_id,state,updated_at) VALUES (?,?,?,'active',?)
			ON CONFLICT(party_id,connector) DO UPDATE SET external_id=excluded.external_id, state='active', updated_at=excluded.updated_at`, party, connector, ext, s.now().Unix()); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "account.link", party, map[string]any{"connector": connector})
	})
}

func (s *Store) appliedGroups(ctx context.Context, party string) (map[string][]string, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT connector, grp FROM applied_groups WHERE party_id=? ORDER BY connector, grp`, party)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var c, g string
		if err := rows.Scan(&c, &g); err != nil {
			return nil, err
		}
		out[c] = append(out[c], g)
	}
	return out, rows.Err()
}

// personGroups lists the names of the SCIM groups a person belongs to.
func (s *Store) personGroups(ctx context.Context, party string) ([]string, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT g.display_name FROM groups g JOIN group_members m ON m.group_id=g.id WHERE m.party_id=?`, party)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func (s *Store) isUrgent(ctx context.Context, party string) (bool, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM urgent WHERE party_id=?`, party).Scan(&n)
	return n > 0, err
}

// touchRun records that a run was examined and when it is next worth examining.
func (s *Store) touchRun(ctx context.Context, id string, wake int64) error {
	ctx, cancel := s.read(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET examined_at=?, wake_at=? WHERE id=?`, s.now().UnixNano(), wake, id)
	return err
}
