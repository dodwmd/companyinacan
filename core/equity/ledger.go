package equity

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

// LedgerEntry is one row of the append-only equity ledger. Quantities are signed: a positive
// row adds to the holder's balance, a negative row removes. Errors are corrected by recording
// the opposite entry, never by editing.
type LedgerEntry struct {
	Seq           int64  `json:"seq"`
	ID            string `json:"id"`
	EntityID      string `json:"entity_id"`
	ClassID       string `json:"class_id"`
	StakeholderID string `json:"stakeholder_id"`
	Kind          string `json:"kind"`
	EffectiveDate string `json:"effective_date"`
	Quantity      Qty    `json:"quantity"`
	PriceMinor    *int64 `json:"price_minor,omitempty"`
	Currency      string `json:"currency,omitempty"`
	GroupID       string `json:"group_id"`
	GrantID       string `json:"grant_id,omitempty"`
	ExerciseID    string `json:"exercise_id,omitempty"`
}

type ledgerRow struct {
	entityID, classID, stakeholderID, kind, date string
	qty                                          Qty // signed
	priceMinor                                   *int64
	currency, grantID, exerciseID, groupID, by   string
}

// appendLedger inserts one row inside tx after the invariants hold: the class and holder belong
// to the entity, the date is not in the future, a holder's balance never goes negative on any
// date, and a class never has more shares in issue than authorised.
func (s *Service) appendLedger(ctx context.Context, tx *sqlTx, r ledgerRow) (string, error) {
	if _, err := parseDate(r.date); err != nil {
		return "", err
	}
	if r.date > s.today() {
		return "", invalid("effective_date cannot be in the future")
	}
	c, err := s.getClass(ctx, tx, r.classID)
	if err != nil || c.EntityID != r.entityID {
		return "", invalid("class_id is not a share class of this entity")
	}
	st, err := s.stakeholder(ctx, tx, r.stakeholderID)
	if err != nil || st.EntityID != r.entityID {
		return "", invalid("stakeholder_id is not a stakeholder of this entity")
	}
	if st.Erased && r.qty > 0 {
		return "", conflict("the stakeholder is erased and cannot receive shares")
	}
	if r.qty == 0 {
		return "", invalid("quantity must not be zero")
	}
	if r.qty > 0 && c.Issued+r.qty > c.Authorised {
		return "", conflict("this would put %s shares in issue, above the %s authorised for the class", c.Issued+r.qty, c.Authorised)
	}
	if r.qty < 0 {
		if err := balanceStaysNonNegative(ctx, tx, r); err != nil {
			return "", err
		}
	}
	if r.priceMinor != nil {
		if err := validMinor("price_minor", *r.priceMinor); err != nil {
			return "", err
		}
		if err := validCurrency(r.currency); err != nil {
			return "", err
		}
	}
	if r.groupID == "" {
		r.groupID = newID("led")
	}
	id := newID("led")
	_, err = tx.ExecContext(ctx, `INSERT INTO ledger (id, entity_id, class_id, stakeholder_id, kind, effective_date, quantity, price_minor, currency, group_id, ref_grant_id, ref_exercise_id, recorded_by, recorded_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, r.entityID, r.classID, r.stakeholderID, r.kind, r.date, int64(r.qty), r.priceMinor, r.currency, r.groupID,
		nullStr(r.grantID), nullStr(r.exerciseID), r.by, s.now().Unix())
	return id, err
}

// balanceStaysNonNegative simulates the holder's history with the new (negative) row added and
// requires the running balance to be non-negative after every date, so a backdated removal
// cannot hide a hole later on.
func balanceStaysNonNegative(ctx context.Context, q querier, r ledgerRow) error {
	rows, err := q.QueryContext(ctx, `SELECT effective_date, quantity FROM ledger WHERE stakeholder_id = ? AND class_id = ?`, r.stakeholderID, r.classID)
	if err != nil {
		return err
	}
	type ev struct {
		d string
		q int64
	}
	evs := []ev{{r.date, int64(r.qty)}}
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.d, &e.q); err != nil {
			_ = rows.Close()
			return err
		}
		evs = append(evs, e)
	}
	_ = rows.Close()
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].d < evs[j].d })
	var bal int64
	for i, e := range evs {
		bal += e.q
		if i+1 < len(evs) && evs[i+1].d == e.d {
			continue // settle a whole day at once
		}
		if bal < 0 {
			return conflict("the holder would hold a negative number of shares on %s", e.d)
		}
	}
	return nil
}

// LedgerInput is a manual ledger entry.
type LedgerInput struct {
	ClassID       string `json:"class_id"`
	StakeholderID string `json:"stakeholder_id"`
	ToID          string `json:"to_stakeholder_id"` // transfers
	Quantity      Qty    `json:"quantity"`
	EffectiveDate string `json:"effective_date"`
	PriceMinor    *int64 `json:"price_minor"`
	Currency      string `json:"currency"`
	Kind          string `json:"kind"` // cancellations: cancellation | repurchase
}

func (s *Service) ledgerWrite(ctx context.Context, a Actor, entityID, route, idemKey string, body []byte, fn func(ctx context.Context, tx *sqlTx) (string, error)) (string, bool, error) {
	if err := s.authorize(ctx, a, ActLedgerWrite, Fact{}, "entity:"+entityID); err != nil {
		return "", false, err
	}
	if !FieldAccess(a, true, FieldHoldings, Fact{}) {
		return "", false, ErrForbidden
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return "", false, err
	}
	return s.idempotent(ctx, a, route+":"+entityID, idemKey, body, func(ctx context.Context) (string, error) {
		var id string
		err := s.tx(ctx, func(ctx context.Context, tx *sqlTx) error {
			var err error
			if id, err = fn(ctx, tx); err != nil {
				return err
			}
			setIdem(ctx, id)
			return s.auditIn(ctx, tx, a, route, "ledger:"+id, []string{FieldHoldings})
		})
		if err != nil {
			return "", err
		}
		return id, nil
	})
}

// guardHolders refuses a manual ledger entry whose holder, on either side, is the caller: the same
// sodOK rule as grants, against the stakeholder's directory person id. A caller without a person_id
// (a service account, a can-core personal access token) cannot be compared and is not refused here.
func guardHolders(ctx context.Context, q querier, a Actor, stakeholderIDs ...string) error {
	for _, id := range stakeholderIDs {
		var person string
		err := q.QueryRowContext(ctx, `SELECT IFNULL(person_id, '') FROM stakeholders WHERE id = ?`, id).Scan(&person)
		if errors.Is(err, sql.ErrNoRows) {
			continue // appendLedger refuses an unknown holder
		}
		if err != nil {
			return err
		}
		if !sodOK(a, person) {
			return ErrSeparation
		}
	}
	return nil
}

// IssueNewShares records an issuance of shares to a holder. finance or admin. The returned
// id is the ledger entry.
func (s *Service) IssueNewShares(ctx context.Context, a Actor, entityID string, in LedgerInput, idemKey string, body []byte) (string, bool, error) {
	return s.ledgerWrite(ctx, a, entityID, "ledger.issue", idemKey, body, func(ctx context.Context, tx *sqlTx) (string, error) {
		if in.Quantity <= 0 || in.Quantity > MaxQty {
			return "", invalid("quantity must be a positive quantity")
		}
		if err := guardHolders(ctx, tx, a, in.StakeholderID); err != nil {
			return "", err
		}
		return s.appendLedger(ctx, tx, ledgerRow{entityID: entityID, classID: in.ClassID, stakeholderID: in.StakeholderID, kind: "issuance", date: in.EffectiveDate,
			qty: in.Quantity, priceMinor: in.PriceMinor, currency: in.Currency, by: canonSubject(a.Subject)})
	})
}

// TransferShares moves shares between two holders (two ledger rows sharing a group). finance or admin.
func (s *Service) TransferShares(ctx context.Context, a Actor, entityID string, in LedgerInput, idemKey string, body []byte) (string, bool, error) {
	return s.ledgerWrite(ctx, a, entityID, "ledger.transfer", idemKey, body, func(ctx context.Context, tx *sqlTx) (string, error) {
		if in.Quantity <= 0 || in.Quantity > MaxQty {
			return "", invalid("quantity must be a positive quantity")
		}
		if in.StakeholderID == in.ToID {
			return "", invalid("a transfer needs two different holders")
		}
		if err := guardHolders(ctx, tx, a, in.StakeholderID, in.ToID); err != nil {
			return "", err
		}
		group := newID("led")
		out, err := s.appendLedger(ctx, tx, ledgerRow{entityID: entityID, classID: in.ClassID, stakeholderID: in.StakeholderID, kind: "transfer_out", date: in.EffectiveDate,
			qty: -in.Quantity, priceMinor: in.PriceMinor, currency: in.Currency, groupID: group, by: canonSubject(a.Subject)})
		if err != nil {
			return "", err
		}
		if _, err := s.appendLedger(ctx, tx, ledgerRow{entityID: entityID, classID: in.ClassID, stakeholderID: in.ToID, kind: "transfer_in", date: in.EffectiveDate,
			qty: in.Quantity, priceMinor: in.PriceMinor, currency: in.Currency, groupID: group, by: canonSubject(a.Subject)}); err != nil {
			return "", err
		}
		return out, nil
	})
}

// CancelShares removes shares from a holder (cancellation or repurchase). finance or admin.
func (s *Service) CancelShares(ctx context.Context, a Actor, entityID string, in LedgerInput, idemKey string, body []byte) (string, bool, error) {
	return s.ledgerWrite(ctx, a, entityID, "ledger.cancel", idemKey, body, func(ctx context.Context, tx *sqlTx) (string, error) {
		if in.Quantity <= 0 || in.Quantity > MaxQty {
			return "", invalid("quantity must be a positive quantity")
		}
		kind := in.Kind
		if kind == "" {
			kind = "cancellation"
		}
		if kind != "cancellation" && kind != "repurchase" {
			return "", invalid("kind must be cancellation or repurchase")
		}
		if err := guardHolders(ctx, tx, a, in.StakeholderID); err != nil {
			return "", err
		}
		return s.appendLedger(ctx, tx, ledgerRow{entityID: entityID, classID: in.ClassID, stakeholderID: in.StakeholderID, kind: kind, date: in.EffectiveDate,
			qty: -in.Quantity, priceMinor: in.PriceMinor, currency: in.Currency, by: canonSubject(a.Subject)})
	})
}

// GetLedgerEntry returns one entry (audited before it is returned).
func (s *Service) GetLedgerEntry(ctx context.Context, a Actor, id string) (*LedgerEntry, error) {
	if err := s.authorize(ctx, a, ActLedgerRead, Fact{}, "ledger:"+id); err != nil {
		return nil, err
	}
	if err := validID("led", id); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	e, err := scanLedger(s.db.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "ledger:"+id, []string{FieldHoldings}); err != nil {
		return nil, err
	}
	return e, nil
}

const ledgerCols = `seq, id, entity_id, class_id, stakeholder_id, kind, effective_date, quantity, price_minor, currency, group_id, ref_grant_id, ref_exercise_id`

func scanLedger(sc interface{ Scan(...any) error }) (*LedgerEntry, error) {
	var e LedgerEntry
	var q int64
	var price sql.NullInt64
	var g, x sql.NullString
	if err := sc.Scan(&e.Seq, &e.ID, &e.EntityID, &e.ClassID, &e.StakeholderID, &e.Kind, &e.EffectiveDate, &q, &price, &e.Currency, &e.GroupID, &g, &x); err != nil {
		return nil, err
	}
	e.Quantity = Qty(q)
	if price.Valid {
		v := price.Int64
		e.PriceMinor = &v
	}
	e.GrantID, e.ExerciseID = g.String, x.String
	return &e, nil
}

// LedgerPage is a page of ledger entries.
type LedgerPage struct {
	Items []LedgerEntry `json:"items"`
	Next  string        `json:"next_cursor,omitempty"`
}

// ListLedger lists an entity's ledger in order, optionally up to a date. Each entry returned is
// audited (atomically) before the page is.
func (s *Service) ListLedger(ctx context.Context, a Actor, entityID, upTo string, after int64, limit int) (*LedgerPage, error) {
	if err := s.authorize(ctx, a, ActLedgerRead, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	if upTo == "" {
		upTo = "9999-12-31"
	} else if _, err := parseDate(upTo); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE entity_id = ? AND seq > ? AND effective_date <= ? ORDER BY seq LIMIT ?`, entityID, after, upTo, limit+1)
	if err != nil {
		return nil, err
	}
	var es []LedgerEntry
	for rows.Next() {
		e, err := scanLedger(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		es = append(es, *e)
	}
	_ = rows.Close()
	page := &LedgerPage{Items: []LedgerEntry{}}
	if len(es) > limit {
		es = es[:limit]
		page.Next = itoa(es[len(es)-1].Seq)
	}
	res := make([]string, len(es))
	for i, e := range es {
		res[i] = "ledger:" + e.ID
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldHoldings}); err != nil {
		return nil, err
	}
	page.Items = append(page.Items, es...)
	return page, nil
}
