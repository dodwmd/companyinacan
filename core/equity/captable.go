package equity

import (
	"context"
	"database/sql"
	"math/big"
	"sort"
)

// ---- pool -----------------------------------------------------------------------------------

// PoolStatus is the state of a plan's pool as of a date: authorised, what outstanding grants
// reserve, what exercises have issued, and what is left. A cancelled, forfeited or expired
// grant releases its unexercised part back to the pool.
type PoolStatus struct {
	PlanID             string `json:"plan_id"`
	AsOf               string `json:"as_of"`
	Authorised         Qty    `json:"authorised"`
	GrantedOutstanding Qty    `json:"granted_outstanding"`
	Exercised          Qty    `json:"exercised"`
	Available          Qty    `json:"available"`
	PendingApproval    Qty    `json:"pending_approval"`
}

// exercisedBy returns, per grant id, what exercises had issued by asof.
func exercisedBy(ctx context.Context, q querier, entityID, asof string) (map[string]Qty, error) {
	rows, err := q.QueryContext(ctx, `SELECT ref_grant_id, SUM(quantity) FROM ledger WHERE entity_id = ? AND kind = 'exercise' AND effective_date <= ? GROUP BY ref_grant_id`, entityID, asof)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Qty{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = Qty(n)
	}
	return out, rows.Err()
}

// outstandingAt is how many options a grant still has outstanding on asof (granted at the
// approval date, not counted once cancelled, forfeited or expired; after a termination only the
// part vested at that date remains, until the exercise window closes).
func (s *Service) outstandingAt(ctx context.Context, q querier, g *Grant, asof string, exercised Qty) (outstanding, vestedOutstanding Qty, err error) {
	if g.ApprovedOn == "" || g.ApprovedOn > asof || (g.TerminalOn != "" && g.TerminalOn <= asof) || asof > g.ExpiryDate {
		return 0, 0, nil
	}
	cut := asof
	terminated := false
	if g.TerminationDate != "" {
		if asof > windowEnd(g) {
			return 0, 0, nil
		}
		if asof >= g.TerminationDate {
			cut, terminated = g.TerminationDate, true
		}
	}
	in, err := s.vestInputFor(ctx, q, g, cut)
	if err != nil {
		return 0, 0, err
	}
	v, err := Vested(in)
	if err != nil {
		return 0, 0, err
	}
	if terminated { // from the termination date the unvested part is gone
		out := max(v-exercised, 0)
		return out, out, nil
	}
	vo := max(v-exercised, 0)
	return max(g.Quantity-exercised, 0), min(vo, max(g.Quantity-exercised, 0)), nil
}

func (s *Service) poolStatus(ctx context.Context, q querier, planID, asof, excludeGrant string) (*PoolStatus, error) {
	p, err := s.getPlan(ctx, q, planID)
	if err != nil {
		return nil, err
	}
	ps := &PoolStatus{PlanID: p.ID, AsOf: asof}
	if p.AdoptedOn <= asof {
		ps.Authorised = p.Authorised
	}
	ex, err := exercisedBy(ctx, q, p.EntityID, asof)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+grantCols+` FROM grants g WHERE g.plan_id = ? AND g.approved_on IS NOT NULL AND g.id != ? LIMIT 100000`, planID, excludeGrant)
	if err != nil {
		return nil, err
	}
	var gs []*grantRow
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		gs = append(gs, g)
	}
	_ = rows.Close()
	for _, g := range gs {
		out, _, err := s.outstandingAt(ctx, q, &g.Grant, asof, ex[g.ID])
		if err != nil {
			return nil, err
		}
		ps.GrantedOutstanding += out
		ps.Exercised += ex[g.ID]
	}
	var pend sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT SUM(quantity) FROM grants WHERE plan_id = ? AND status = 'pending_approval'`, planID).Scan(&pend); err != nil {
		return nil, err
	}
	ps.PendingApproval = Qty(pend.Int64)
	ps.Available = ps.Authorised - ps.GrantedOutstanding - ps.Exercised
	return ps, nil
}

// Pool returns a plan's pool status as of a date (today by default). Roles that may read
// structure (hr, finance, board, admin): it holds no holder identity.
func (s *Service) Pool(ctx context.Context, a Actor, planID, asof string) (*PoolStatus, error) {
	if err := s.authorize(ctx, a, ActStructureRead, Fact{}, "plan:"+planID); err != nil {
		return nil, err
	}
	if asof == "" {
		asof = s.today()
	}
	if _, err := parseDate(asof); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	return s.poolStatus(ctx, s.db, planID, asof, "")
}

// ---- cap table ------------------------------------------------------------------------------

// CapTable is the ownership picture of an entity as of a date.
type CapTable struct {
	EntityID string       `json:"entity_id"`
	AsOf     string       `json:"as_of"`
	View     string       `json:"view"` // basic | fully-diluted
	Classes  []ClassLine  `json:"classes"`
	Holders  []HolderLine `json:"holders"`
	Pool     []PoolLine   `json:"pool,omitempty"`
	Totals   CapTotals    `json:"totals"`
	Warnings []string     `json:"warnings"`
	Notice   string       `json:"notice"`
}

// ClassLine is one share class.
type ClassLine struct {
	ClassID    string `json:"class_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Authorised Qty    `json:"authorised"`
	Issued     Qty    `json:"issued"`
}

// ClassHolding is shares of one class held.
type ClassHolding struct {
	ClassID string `json:"class_id"`
	Shares  Qty    `json:"shares"`
}

// HolderLine is one stakeholder's position. Names appear only when asked for and permitted.
type HolderLine struct {
	StakeholderID      string         `json:"stakeholder_id"`
	Kind               string         `json:"kind"`
	Name               string         `json:"name,omitempty"`
	Shares             []ClassHolding `json:"shares"`
	BasicShares        Qty            `json:"basic_shares"`
	BasicPPM           int64          `json:"basic_ownership_ppm"`
	OptionsOutstanding *Qty           `json:"options_outstanding,omitempty"`
	OptionsVested      *Qty           `json:"options_vested,omitempty"`
	FullyDiluted       *Qty           `json:"fully_diluted_shares,omitempty"`
	FullyDilutedPPM    *int64         `json:"fully_diluted_ownership_ppm,omitempty"`
}

// PoolLine is the unallocated pool of one plan (a line of the fully diluted view).
type PoolLine struct {
	PlanID             string `json:"plan_id"`
	Name               string `json:"name"`
	Authorised         Qty    `json:"authorised"`
	GrantedOutstanding Qty    `json:"granted_outstanding"`
	Exercised          Qty    `json:"exercised"`
	Available          Qty    `json:"available"`
	FullyDilutedPPM    int64  `json:"fully_diluted_ownership_ppm"`
}

// CapTotals are the table's totals.
type CapTotals struct {
	BasicShares        Qty `json:"basic_shares"`
	OptionsOutstanding Qty `json:"options_outstanding,omitempty"`
	PoolAvailable      Qty `json:"pool_available,omitempty"`
	FullyDiluted       Qty `json:"fully_diluted_shares,omitempty"`
}

// allocatePPM splits 1,000,000 parts among the values in proportion, by the largest remainder
// method: the parts always add up to exactly 1,000,000 (or all are zero when the total is zero),
// each part is within one of its exact share, and the order of equal remainders is by index.
func allocatePPM(values []Qty) []int64 {
	out := make([]int64, len(values))
	var total int64
	for _, v := range values {
		if v > 0 {
			total += int64(v)
		}
	}
	if total == 0 {
		return out
	}
	type rem struct {
		i int
		r *big.Int
	}
	rems := make([]rem, 0, len(values))
	var given int64
	for i, v := range values {
		if v <= 0 {
			continue
		}
		n := new(big.Int).Mul(big.NewInt(int64(v)), big.NewInt(1_000_000))
		q, r := new(big.Int).QuoRem(n, big.NewInt(total), new(big.Int))
		out[i] = q.Int64()
		given += out[i]
		rems = append(rems, rem{i, r})
	}
	sort.SliceStable(rems, func(x, y int) bool { return rems[x].r.Cmp(rems[y].r) > 0 })
	for k := int64(0); k < 1_000_000-given; k++ {
		out[rems[k].i]++
	}
	return out
}

type holderAgg struct {
	kind                  string
	shares                map[string]Qty
	options, optionVested Qty
}

// capTable computes the table. It is read-only and takes whatever querier it is given.
func (s *Service) capTable(ctx context.Context, q querier, entityID, asof string, fd bool) (*CapTable, map[string]*holderAgg, error) {
	ct := &CapTable{EntityID: entityID, AsOf: asof, View: "basic", Classes: []ClassLine{}, Holders: []HolderLine{}, Warnings: []string{}, Notice: Notice}
	if fd {
		ct.View = "fully-diluted"
	}
	agg := map[string]*holderAgg{}
	get := func(id string) *holderAgg {
		h := agg[id]
		if h == nil {
			h = &holderAgg{shares: map[string]Qty{}}
			agg[id] = h
		}
		return h
	}
	// classes
	rows, err := q.QueryContext(ctx, `SELECT id, name, kind, authorised FROM share_classes WHERE entity_id = ? ORDER BY created_at, id`, entityID)
	if err != nil {
		return nil, nil, err
	}
	classIdx := map[string]int{}
	for rows.Next() {
		var c ClassLine
		var auth int64
		if err := rows.Scan(&c.ClassID, &c.Name, &c.Kind, &auth); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		c.Authorised = Qty(auth)
		classIdx[c.ClassID] = len(ct.Classes)
		ct.Classes = append(ct.Classes, c)
	}
	_ = rows.Close()
	// holders (the stakeholder kinds first, so people with only options appear too)
	rows, err = q.QueryContext(ctx, `SELECT id, kind FROM stakeholders WHERE entity_id = ? ORDER BY created_at, id LIMIT 20000`, entityID)
	if err != nil {
		return nil, nil, err
	}
	var order []string
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		get(id).kind = kind
		order = append(order, id)
	}
	_ = rows.Close()
	// ledger balances as of the date
	rows, err = q.QueryContext(ctx, `SELECT stakeholder_id, class_id, SUM(quantity) FROM ledger WHERE entity_id = ? AND effective_date <= ? GROUP BY stakeholder_id, class_id`, entityID, asof)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var sid, cid string
		var n int64
		if err := rows.Scan(&sid, &cid, &n); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		get(sid).shares[cid] += Qty(n)
		if i, ok := classIdx[cid]; ok {
			ct.Classes[i].Issued += Qty(n)
		}
		ct.Totals.BasicShares += Qty(n)
	}
	_ = rows.Close()
	classOptions := map[string]Qty{} // options outstanding + pool available, by class
	if fd {
		ex, err := exercisedBy(ctx, q, entityID, asof)
		if err != nil {
			return nil, nil, err
		}
		prows, err := q.QueryContext(ctx, `SELECT id, class_id, name FROM plans WHERE entity_id = ? ORDER BY created_at, id`, entityID)
		if err != nil {
			return nil, nil, err
		}
		type pl struct{ id, class, name string }
		var plans []pl
		for prows.Next() {
			var p pl
			if err := prows.Scan(&p.id, &p.class, &p.name); err != nil {
				_ = prows.Close()
				return nil, nil, err
			}
			plans = append(plans, p)
		}
		_ = prows.Close()
		for _, p := range plans {
			ps, err := s.poolStatus(ctx, q, p.id, asof, "")
			if err != nil {
				return nil, nil, err
			}
			ct.Pool = append(ct.Pool, PoolLine{PlanID: p.id, Name: p.name, Authorised: ps.Authorised, GrantedOutstanding: ps.GrantedOutstanding, Exercised: ps.Exercised, Available: ps.Available})
			ct.Totals.PoolAvailable += ps.Available
			classOptions[p.class] += ps.Available + ps.GrantedOutstanding
			if ps.Available < 0 {
				ct.Warnings = append(ct.Warnings, "plan "+p.id+" has more granted than authorised")
			}
		}
		grows, err := q.QueryContext(ctx, `SELECT `+grantCols+` FROM grants g WHERE g.entity_id = ? AND g.approved_on IS NOT NULL AND g.approved_on <= ? LIMIT 100000`, entityID, asof)
		if err != nil {
			return nil, nil, err
		}
		var gs []*grantRow
		for grows.Next() {
			g, err := scanGrant(grows)
			if err != nil {
				_ = grows.Close()
				return nil, nil, err
			}
			gs = append(gs, g)
		}
		_ = grows.Close()
		for _, g := range gs {
			out, vo, err := s.outstandingAt(ctx, q, &g.Grant, asof, ex[g.ID])
			if err != nil {
				return nil, nil, err
			}
			if out == 0 {
				continue
			}
			h := get(g.StakeholderID)
			h.options += out
			h.optionVested += vo
			ct.Totals.OptionsOutstanding += out
		}
		ct.Totals.FullyDiluted = ct.Totals.BasicShares + ct.Totals.OptionsOutstanding + ct.Totals.PoolAvailable
	}
	for i := range ct.Classes {
		c := &ct.Classes[i]
		if c.Issued > c.Authorised {
			ct.Warnings = append(ct.Warnings, "class "+c.ClassID+" has more shares issued than authorised")
		} else if fd && c.Issued+classOptions[c.ClassID] > c.Authorised {
			ct.Warnings = append(ct.Warnings, "class "+c.ClassID+": issued shares plus options and pool exceed the authorised shares")
		}
	}
	// ordered holder lines (only those with a position)
	basicVals, fdVals := []Qty{}, []Qty{}
	var lines []HolderLine
	for _, id := range order {
		h := agg[id]
		var basic Qty
		hs := []ClassHolding{}
		for _, c := range ct.Classes {
			if n := h.shares[c.ClassID]; n != 0 {
				hs = append(hs, ClassHolding{ClassID: c.ClassID, Shares: n})
				basic += n
			}
		}
		if basic == 0 && h.options == 0 {
			continue
		}
		hl := HolderLine{StakeholderID: id, Kind: h.kind, Shares: hs, BasicShares: basic}
		if fd {
			o, v, tot := h.options, h.optionVested, basic+h.options
			hl.OptionsOutstanding, hl.OptionsVested, hl.FullyDiluted = &o, &v, &tot
			fdVals = append(fdVals, tot)
		}
		basicVals = append(basicVals, basic)
		lines = append(lines, hl)
	}
	bp := allocatePPM(basicVals)
	for i := range lines {
		lines[i].BasicPPM = bp[i]
	}
	if fd {
		for _, p := range ct.Pool {
			fdVals = append(fdVals, max(p.Available, 0))
		}
		fp := allocatePPM(fdVals)
		for i := range lines {
			v := fp[i]
			lines[i].FullyDilutedPPM = &v
		}
		for j := range ct.Pool {
			ct.Pool[j].FullyDilutedPPM = fp[len(lines)+j]
		}
	}
	ct.Holders = lines
	return ct, agg, nil
}

// GetCapTable returns the cap table as of a date. view is "basic" or "fully-diluted"; names
// adds holder names (a second, per-field permission). The read is audited per holder before
// anything is returned.
func (s *Service) GetCapTable(ctx context.Context, a Actor, entityID, asof, view string, names bool) (*CapTable, error) {
	if err := s.authorize(ctx, a, ActCapTable, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	if asof == "" {
		asof = s.today()
	}
	if _, err := parseDate(asof); err != nil {
		return nil, err
	}
	if view == "" {
		view = "basic"
	}
	if view != "basic" && view != "fully-diluted" {
		return nil, invalid("view must be basic or fully-diluted")
	}
	if names && !FieldAccess(a, false, FieldIdentity, Fact{}) {
		s.auditDeny(ctx, a, "read", "entity:"+entityID, []string{FieldIdentity})
		return nil, ErrForbidden
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	ct, _, err := s.capTable(ctx, s.db, entityID, asof, view == "fully-diluted")
	if err != nil {
		return nil, err
	}
	fields := []string{FieldHoldings}
	if names {
		fields = append(fields, FieldIdentity)
	}
	res := []string{"captable:" + entityID + "@" + asof}
	for _, h := range ct.Holders {
		res = append(res, "stakeholder:"+h.StakeholderID)
	}
	if err := s.auditReadMany(ctx, a, "read", res, fields); err != nil {
		return nil, err
	}
	if names {
		for i := range ct.Holders {
			id, err := s.openIdentity(ctx, s.db, ct.Holders[i].StakeholderID)
			if err != nil {
				return nil, err
			}
			ct.Holders[i].Name = id.Name
		}
	}
	return ct, nil
}
