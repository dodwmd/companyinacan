package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Purchase order statuses.
const (
	PoDraft     = "draft"
	PoApproved  = "approved"
	PoReceived  = "received"
	PoCancelled = "cancelled"
)

// PurchaseOrder is a minimal order to a vendor: lines, an approval and a number. Bills
// (payables), three-way matching and vendor portals are later work.
type PurchaseOrder struct {
	ID            int64      `json:"id"`
	VendorID      int64      `json:"vendor_id"`
	Number        *string    `json:"number"`
	Status        string     `json:"status"`
	Currency      string     `json:"currency"`
	Project       string     `json:"project"`
	Lines         []Line     `json:"lines"`
	SubtotalMinor int64      `json:"subtotal_minor"`
	TaxMinor      int64      `json:"tax_minor"`
	TotalMinor    int64      `json:"total_minor"`
	Notes         string     `json:"notes"`
	CreatedBy     string     `json:"created_by"`
	ApprovedBy    *string    `json:"approved_by"`
	ApprovedAt    *time.Time `json:"approved_at"`
	Version       int64      `json:"version"`
	CreatedAt     time.Time  `json:"created_at"`
	updatedBy     string
}

// NewPurchaseOrder is the body of a PO create or edit.
type NewPurchaseOrder struct {
	VendorID int64       `json:"vendor_id"`
	Currency string      `json:"currency"`
	Project  string      `json:"project"`
	Notes    string      `json:"notes"`
	Lines    []LineInput `json:"lines"`
}

const poCols = `id, vendor_id, number_text, status, currency, project, lines, subtotal_minor, tax_minor, total_minor, notes, created_by, updated_by, approved_by, approved_at, version, created_at`

func scanPO(sc scanner) (*PurchaseOrder, error) {
	var po PurchaseOrder
	var num, appBy sql.NullString
	var appAt sql.NullInt64
	var lines string
	var c int64
	if err := sc.Scan(&po.ID, &po.VendorID, &num, &po.Status, &po.Currency, &po.Project, &lines, &po.SubtotalMinor, &po.TaxMinor, &po.TotalMinor, &po.Notes, &po.CreatedBy, &po.updatedBy, &appBy, &appAt, &po.Version, &c); err != nil {
		return nil, err
	}
	po.Number, po.ApprovedBy, po.ApprovedAt, po.CreatedAt = nullStr(num), nullStr(appBy), nullTime(appAt), fromUnix(c)
	return &po, json.Unmarshal([]byte(lines), &po.Lines)
}

func (s *Store) loadPO(ctx context.Context, q dbq, a *Principal, id int64) (*PurchaseOrder, error) {
	var w where
	w.and("(id = ?)", id)
	c, args := partyClause(a, "vendor_id")
	w.scope(c, args)
	c, args = projectClause(a, "project")
	w.scope(c, args)
	po, err := scanPO(q.QueryRowContext(ctx, `SELECT `+poCols+` FROM purchase_orders WHERE `+w.sql(), w.args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return po, err
}

// GetPurchaseOrder returns a PO in scope.
func (s *Store) GetPurchaseOrder(ctx context.Context, a *Principal, id int64) (*PurchaseOrder, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	return s.loadPO(rctx, s.rdb, a, id)
}

// ListPurchaseOrders lists POs in scope, newest first.
func (s *Store) ListPurchaseOrders(ctx context.Context, a *Principal, status string, before int64, limit int) ([]PurchaseOrder, int64, error) {
	limit = clampLimit(limit)
	var w where
	c, args := partyClause(a, "vendor_id")
	w.scope(c, args)
	c, args = projectClause(a, "project")
	w.scope(c, args)
	if before > 0 {
		w.and("(id < ?)", before)
	}
	if status != "" {
		w.and("(status = ?)", status)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+poCols+` FROM purchase_orders WHERE `+w.sql()+` ORDER BY id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []PurchaseOrder{}
	for rows.Next() {
		po, err := scanPO(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *po)
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, rows.Err()
}

func (s *Store) poPrice(ctx context.Context, tx dbq, a *Principal, in NewPurchaseOrder) (cur string, lines []Line, t totals, err error) {
	if in.Project != "" && !projectRE.MatchString(in.Project) {
		return "", nil, t, invalid("project", "not a project key")
	}
	if len(a.Projects) > 0 && (in.Project == "" || !a.CanProject(in.Project)) {
		return "", nil, t, invalid("project", "a project-scoped token must use one of its projects")
	}
	if !validText(in.Notes, maxText) {
		return "", nil, t, invalid("notes", "at most %d characters", maxText)
	}
	v, err := s.loadPartyRow(ctx, tx, a, in.VendorID)
	if err != nil {
		return "", nil, t, invalid("vendor_id", "no such vendor")
	}
	if v.Kind == "customer" || v.Erased {
		return "", nil, t, invalid("vendor_id", "the party is not an active vendor")
	}
	cur = in.Currency
	if cur == "" {
		cur = v.Currency
	}
	if cur == "" {
		cur = s.base
	}
	if _, ok := CurrencyExponent(cur); !ok {
		return "", nil, t, invalid("currency", "unknown currency code")
	}
	lines, t, err = s.priceLines(ctx, tx, cur, "lines", in.Lines)
	return cur, lines, t, err
}

// CreatePurchaseOrder drafts a PO.
func (s *Store) CreatePurchaseOrder(ctx context.Context, a *Principal, in NewPurchaseOrder, idemKey string) (*PurchaseOrder, bool, error) {
	var out *PurchaseOrder
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "po.create", in, func() (*PurchaseOrder, error) {
			cur, lines, t, err := s.poPrice(ctx, tx, a, in)
			if err != nil {
				return nil, err
			}
			lj, _ := json.Marshal(lines)
			now := ts(s.now())
			res, err := tx.ExecContext(ctx, `INSERT INTO purchase_orders (vendor_id, currency, project, lines, subtotal_minor, tax_minor, total_minor, notes, created_by, updated_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				in.VendorID, cur, in.Project, string(lj), t.subtotal-t.discount, t.tax, t.total, in.Notes, a.Subject, a.Subject, now, now)
			if err != nil {
				return nil, err
			}
			id, _ := res.LastInsertId()
			return s.loadPO(ctx, tx, a, id)
		})
		return err
	})
	return out, replayed, err
}

func (s *Store) withPO(ctx context.Context, a *Principal, id int64, ifMatch *int64, fn func(tx *sql.Tx, po *PurchaseOrder) error) (*PurchaseOrder, error) {
	var out *PurchaseOrder
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		po, err := s.loadPO(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != po.Version {
			return ErrVersionMismatch
		}
		if err := fn(tx, po); err != nil {
			return err
		}
		out, err = s.loadPO(ctx, tx, a, id)
		return err
	})
	return out, err
}

// UpdatePurchaseOrder replaces a draft PO's content.
func (s *Store) UpdatePurchaseOrder(ctx context.Context, a *Principal, id int64, ifMatch *int64, in NewPurchaseOrder) (*PurchaseOrder, error) {
	return s.withPO(ctx, a, id, ifMatch, func(tx *sql.Tx, po *PurchaseOrder) error {
		if po.Status != PoDraft {
			return fmt.Errorf("%w: only a draft purchase order can be edited (it is %s)", ErrConflict, po.Status)
		}
		cur, lines, t, err := s.poPrice(ctx, tx, a, in)
		if err != nil {
			return err
		}
		lj, _ := json.Marshal(lines)
		if err := s.withdrawDecision(ctx, tx, apPurchaseOrder, id); err != nil { // asked about the old content
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE purchase_orders SET vendor_id=?, currency=?, project=?, lines=?, subtotal_minor=?, tax_minor=?, total_minor=?, notes=?, updated_by=?, version=version+1, updated_at=? WHERE id=?`,
			in.VendorID, cur, in.Project, string(lj), t.subtotal-t.discount, t.tax, t.total, in.Notes, a.Subject, ts(s.now()), id)
		return err
	})
}

// ApprovePurchaseOrder approves a draft and numbers it from the PO series, gap-free. The
// creator and the last editor cannot approve it.
func (s *Store) ApprovePurchaseOrder(ctx context.Context, a *Principal, id int64, ifMatch *int64) (*PurchaseOrder, error) {
	return s.withPO(ctx, a, id, ifMatch, func(tx *sql.Tx, po *PurchaseOrder) error {
		if err := s.authorizeApproval(ctx, a, ApprovalRequest{Kind: "purchase_order"}, po.CreatedBy, po.updatedBy); err != nil {
			return err
		}
		if po.Status != PoDraft {
			return fmt.Errorf("%w: the purchase order is %s", ErrConflict, po.Status)
		}
		if err := s.engineAllows(ctx, tx, apPurchaseOrder, id, po.Version, a, true); err != nil {
			return err
		}
		if err := s.applyPO(ctx, tx, po, a.Subject); err != nil {
			return err
		}
		return s.withdrawDecision(ctx, tx, apPurchaseOrder, id)
	})
}

// applyPO approves and numbers a draft. The caller has authorised it.
func (s *Store) applyPO(ctx context.Context, tx *sql.Tx, po *PurchaseOrder, by string) error {
	var n int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO series (code, next_number) VALUES ('PO', 2) ON CONFLICT (code) DO UPDATE SET next_number = next_number + 1 RETURNING next_number - 1`).Scan(&n); err != nil {
		return err
	}
	text := fmt.Sprintf("PO-%05d", n)
	now := ts(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE purchase_orders SET status='approved', number=?, number_text=?, approved_by=?, approved_at=?, version=version+1, updated_at=? WHERE id=?`, n, text, by, now, now, po.ID); err != nil {
		return err
	}
	return s.audit(ctx, tx, by, "purchase_order.approve", text, map[string]any{"id": po.ID, "total_minor": po.TotalMinor, "currency": po.Currency})
}

// SetPurchaseOrderStatus marks an approved PO received, or cancels a draft or approved one.
func (s *Store) SetPurchaseOrderStatus(ctx context.Context, a *Principal, id int64, ifMatch *int64, status string) (*PurchaseOrder, error) {
	return s.withPO(ctx, a, id, ifMatch, func(tx *sql.Tx, po *PurchaseOrder) error {
		ok := (status == PoReceived && po.Status == PoApproved) || (status == PoCancelled && (po.Status == PoDraft || po.Status == PoApproved))
		if !ok {
			return fmt.Errorf("%w: cannot move a %s purchase order to %s", ErrConflict, po.Status, status)
		}
		if err := s.withdrawDecision(ctx, tx, apPurchaseOrder, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE purchase_orders SET status=?, version=version+1, updated_at=? WHERE id=?`, status, ts(s.now()), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "purchase_order."+status, fmt.Sprint(id), map[string]any{})
	})
}
