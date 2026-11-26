package finance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- parties (customers and vendors) --------------------------------------------------

// Party is a customer, a vendor or both. Contact data is personal data: reads are audited
// and erasure follows the rules in docs/finance.md (legal retention beats erasure for
// issued invoices, which keep a frozen snapshot of name, address and tax id).
type Party struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	ContactName string    `json:"contact_name"`
	Email       string    `json:"email"`
	Phone       string    `json:"phone"`
	Address     string    `json:"address"`
	TaxID       string    `json:"tax_id"`
	Currency    string    `json:"currency"`
	Notes       string    `json:"notes"`
	Erased      bool      `json:"erased"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NewParty is the body of a party create.
type NewParty struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	ContactName string `json:"contact_name"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	Address     string `json:"address"`
	TaxID       string `json:"tax_id"`
	Currency    string `json:"currency"`
	Notes       string `json:"notes"`
}

// PartyPatch changes a party; absent fields are left alone, "" clears a text field.
type PartyPatch struct {
	Kind        *string `json:"kind"`
	Name        *string `json:"name"`
	ContactName *string `json:"contact_name"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
	Address     *string `json:"address"`
	TaxID       *string `json:"tax_id"`
	Currency    *string `json:"currency"`
	Notes       *string `json:"notes"`
}

func (in *NewParty) validate() error {
	var p problems
	checkPartyFields(&p, in.Kind, &in.Name, &in.ContactName, &in.Email, &in.Phone, &in.Address, &in.TaxID, &in.Currency, &in.Notes)
	if strings.TrimSpace(in.Name) == "" {
		p.add("name", "required")
	}
	return p.err()
}

func checkPartyFields(p *problems, kind string, name, contact, email, phone, address, taxID, currency, notes *string) {
	if kind != "" && kind != "customer" && kind != "vendor" && kind != "both" {
		p.add("kind", "must be customer, vendor or both")
	}
	for _, f := range []struct {
		n string
		v *string
		m int
	}{{"name", name, maxName}, {"contact_name", contact, maxName}, {"phone", phone, 50}, {"tax_id", taxID, 50}} {
		if f.v != nil && !oneLineText(*f.v, f.m) {
			p.add(f.n, "one line, at most %d characters", f.m)
		}
	}
	if address != nil && !validText(*address, 500) {
		p.add("address", "at most 500 characters")
	}
	if notes != nil && !validText(*notes, maxText) {
		p.add("notes", "at most %d characters", maxText)
	}
	if email != nil && !validEmail(*email) {
		p.add("email", "not a valid email address")
	}
	if currency != nil && *currency != "" {
		if _, ok := CurrencyExponent(*currency); !ok {
			p.add("currency", "unknown currency code")
		}
	}
}

func (p *PartyPatch) validate() error {
	var pr problems
	kind := ""
	if p.Kind != nil {
		kind = *p.Kind
		if kind == "" {
			pr.add("kind", "must be customer, vendor or both")
		}
	}
	checkPartyFields(&pr, kind, p.Name, p.ContactName, p.Email, p.Phone, p.Address, p.TaxID, p.Currency, p.Notes)
	if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
		pr.add("name", "required")
	}
	return pr.err()
}

const partySelect = `SELECT id, kind, name, contact_name, email, phone, address, tax_id, currency, notes, erased_at, version, created_at, updated_at FROM parties`

func scanParty(sc scanner) (*Party, error) {
	var p Party
	var erased sql.NullInt64
	var c, u int64
	if err := sc.Scan(&p.ID, &p.Kind, &p.Name, &p.ContactName, &p.Email, &p.Phone, &p.Address, &p.TaxID, &p.Currency, &p.Notes, &erased, &p.Version, &c, &u); err != nil {
		return nil, err
	}
	p.Erased, p.CreatedAt, p.UpdatedAt = erased.Valid, fromUnix(c), fromUnix(u)
	return &p, nil
}

// CreateParty adds a party. A party-scoped token cannot create parties: it could not see them.
func (s *Store) CreateParty(ctx context.Context, a *Principal, in NewParty) (*Party, error) {
	if len(a.Parties) > 0 {
		return nil, ErrForbidden
	}
	if in.Kind == "" {
		in.Kind = "customer"
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var out *Party
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := ts(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO parties (kind, name, contact_name, email, phone, address, tax_id, currency, notes, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Kind, in.Name, in.ContactName, in.Email, in.Phone, in.Address, in.TaxID, in.Currency, in.Notes, a.Subject, now, now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		out, err = scanParty(tx.QueryRowContext(ctx, partySelect+` WHERE id = ?`, id))
		return err
	})
	return out, err
}

// GetParty returns a party in the caller's scope. Reading contact data is audited.
func (s *Store) GetParty(ctx context.Context, a *Principal, id int64) (*Party, error) {
	if !a.CanParty(id) {
		return nil, ErrNotFound
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	p, err := scanParty(s.rdb.QueryRowContext(rctx, partySelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "party.read", strconv.FormatInt(id, 10), map[string]any{}); err != nil {
		return nil, err
	}
	return p, nil
}

// PartyFilter selects parties.
type PartyFilter struct {
	Kind   string
	Search string
}

// ListParties lists parties in scope, ordered by id. The listing exposes contact data, so it
// is audited like a single read.
func (s *Store) ListParties(ctx context.Context, a *Principal, f PartyFilter, after int64, limit int) ([]Party, int64, error) {
	limit = clampLimit(limit)
	var w where
	c, args := partyClause(a, "id")
	w.scope(c, args)
	w.and("(id > ?)", after)
	if f.Kind != "" {
		w.and("(kind = ? OR kind = 'both')", f.Kind)
	}
	if f.Search != "" {
		w.and(`(name LIKE ? ESCAPE '\')`, "%"+likeEscape(f.Search)+"%")
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, partySelect+` WHERE `+w.sql()+` ORDER BY id LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []Party{}
	for rows.Next() {
		p, err := scanParty(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	if err := s.auditRead(ctx, a, "party.list", "", map[string]any{"count": len(out)}); err != nil {
		return nil, 0, err
	}
	return out, next, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

var errMustStayVendor = fmt.Errorf("%w: the party has purchase orders and must stay a vendor", ErrConflict)

// mustStayVendor refuses to narrow a party with purchase orders to a customer. Every path that
// changes a party's kind calls it.
func mustStayVendor(ctx context.Context, tx dbq, id int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM purchase_orders WHERE vendor_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errMustStayVendor
	}
	return nil
}

// UpdateParty changes a party. ifMatch, when set, must equal the stored version.
func (s *Store) UpdateParty(ctx context.Context, a *Principal, id int64, ifMatch *int64, in PartyPatch) (*Party, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	if !a.CanParty(id) {
		return nil, ErrNotFound
	}
	var out *Party
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanParty(tx.QueryRowContext(ctx, partySelect+` WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Erased {
			return fmt.Errorf("%w: the party has been erased", ErrConflict)
		}
		if ifMatch != nil && *ifMatch != cur.Version {
			return ErrVersionMismatch
		}
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = *v
			}
		}
		set(&cur.Kind, in.Kind)
		set(&cur.Name, in.Name)
		set(&cur.ContactName, in.ContactName)
		set(&cur.Email, in.Email)
		set(&cur.Phone, in.Phone)
		set(&cur.Address, in.Address)
		set(&cur.TaxID, in.TaxID)
		set(&cur.Currency, in.Currency)
		set(&cur.Notes, in.Notes)
		// Narrowing a party's kind must not strand documents of the dropped kind.
		if cur.Kind == "customer" {
			if err := mustStayVendor(ctx, tx, id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE parties SET kind=?, name=?, contact_name=?, email=?, phone=?, address=?, tax_id=?, currency=?, notes=?, version=version+1, updated_at=? WHERE id=? AND version=?`,
			cur.Kind, cur.Name, cur.ContactName, cur.Email, cur.Phone, cur.Address, cur.TaxID, cur.Currency, cur.Notes, ts(s.now()), id, cur.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrVersionMismatch
		}
		out, err = scanParty(tx.QueryRowContext(ctx, partySelect+` WHERE id = ?`, id))
		return err
	})
	return out, err
}

// Retained describes a record class kept despite an erasure request, and why.
type Retained struct {
	Class  string `json:"class"`
	Count  int    `json:"count"`
	Until  string `json:"until"`
	Reason string `json:"reason"`
}

// ErasureReport is what an erasure did and did not do.
type ErasureReport struct {
	PartyID       int64      `json:"party_id"`
	ErasedFields  []string   `json:"erased_fields"`
	DeletedDrafts int        `json:"deleted_drafts"`
	Retained      []Retained `json:"retained"`
	AlreadyErased bool       `json:"already_erased"`
}

// ErasePartyData applies a data-subject erasure request to a party. Contact data (email,
// phone, contact name, notes, address, tax id, name) is removed from the party record, which
// is pseudonymised. Drafts and recurring schedules are deleted. Issued invoices, credit
// notes, payments and signed contracts are legal records: they are kept for the retention
// period with the frozen snapshot of name, address and tax id they were issued with, and
// the report says which and until when. After the retention period the purge deletes them.
func (s *Store) ErasePartyData(ctx context.Context, a *Principal, id int64, retentionYears int) (*ErasureReport, error) {
	if !a.CanParty(id) {
		return nil, ErrNotFound
	}
	rep := &ErasureReport{PartyID: id, ErasedFields: []string{}, Retained: []Retained{}}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanParty(tx.QueryRowContext(ctx, partySelect+` WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rep.AlreadyErased = cur.Erased
		if err := s.releaseDraftEntries(ctx, tx, `SELECT id FROM invoices WHERE party_id = ? AND status = 'draft'`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM invoices WHERE party_id = ? AND status = 'draft'`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		rep.DeletedDrafts += int(n)
		// Draft contracts were never sent: no legal record yet, and their text and variables
		// carry the party's data. Their versions are immutable, so the purge gate is opened for
		// exactly this delete (the audit cut-off stays 0: no audit entry can go).
		if _, err := tx.ExecContext(ctx, `UPDATE retention_gate SET open = 1, audit_before = 0`); err != nil {
			return err
		}
		// Only contracts that were never sent: a contract that was sent and then given a new version is
		// a draft again, but the versions it was sent with are a legal record and stay (sent_once).
		if _, err := tx.ExecContext(ctx, `DELETE FROM contract_versions WHERE contract_id IN (SELECT id FROM contracts WHERE party_id = ? AND status = 'draft' AND sent_once = 0)`, id); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM contracts WHERE party_id = ? AND status = 'draft' AND sent_once = 0`, id)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		rep.DeletedDrafts += int(n)
		if _, err := tx.ExecContext(ctx, `UPDATE retention_gate SET open = 0, audit_before = 0`); err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM purchase_orders WHERE vendor_id = ? AND status = 'draft'`, `DELETE FROM recurring WHERE party_id = ?`} {
			res, err := tx.ExecContext(ctx, q, id)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			rep.DeletedDrafts += int(n)
		}
		// Cached idempotent responses hold copies of drafts (notes, party ids); drop the ones that
		// name this party so a replay cannot resurrect what was just erased.
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE response LIKE ? OR response LIKE ?`,
			fmt.Sprintf(`%%"party_id":%d,%%`, id), fmt.Sprintf(`%%"vendor_id":%d,%%`, id)); err != nil {
			return err
		}
		if !cur.Erased {
			if _, err := tx.ExecContext(ctx, `UPDATE parties SET name = ?, contact_name = '', email = '', phone = '', address = '', tax_id = '', notes = '', erased_at = ?, version = version + 1, updated_at = ? WHERE id = ?`,
				fmt.Sprintf("Erased party #%d", id), ts(s.now()), ts(s.now()), id); err != nil {
				return err
			}
		}
		rep.ErasedFields = []string{"name", "contact_name", "email", "phone", "address", "tax_id", "notes"}
		var cnt int
		var last sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoices WHERE party_id = ? AND kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid')`, id).Scan(&cnt); err != nil {
			return err
		}
		if cnt > 0 {
			rep.Retained = append(rep.Retained, Retained{Class: "finance-invoice-unpaid", Count: cnt, Until: "",
				Reason: "unpaid invoices are never purged: the retention period starts when they are paid or voided"})
		}
		cnt, last = 0, sql.NullString{}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), MAX(issue_date) FROM invoices WHERE party_id = ? AND status <> 'draft' AND NOT (kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid'))`, id).Scan(&cnt, &last); err != nil {
			return err
		}
		if cnt > 0 {
			rep.Retained = append(rep.Retained, Retained{Class: "finance-invoice", Count: cnt, Until: retainUntil(last.String, retentionYears),
				Reason: "issued invoices and credit notes must be kept for the legal retention period; they keep the name, address and tax id they were issued with"})
		}
		cnt, last = 0, sql.NullString{}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), MAX(COALESCE(end_date, start_date, date(created_at, 'unixepoch'))) FROM contracts WHERE party_id = ? AND (status <> 'draft' OR sent_once = 1)`, id).Scan(&cnt, &last); err != nil {
			return err
		}
		if cnt > 0 {
			rep.Retained = append(rep.Retained, Retained{Class: "finance-contract", Count: cnt, Until: retainUntil(last.String, retentionYears),
				Reason: "contracts that were sent or signed are retained for the legal retention period"})
		}
		cnt, last = 0, sql.NullString{}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), MAX(date(created_at, 'unixepoch')) FROM purchase_orders WHERE vendor_id = ? AND status <> 'draft'`, id).Scan(&cnt, &last); err != nil {
			return err
		}
		if cnt > 0 {
			rep.Retained = append(rep.Retained, Retained{Class: "finance-purchase-order", Count: cnt, Until: retainUntil(last.String, retentionYears),
				Reason: "approved purchase orders are retained for the legal retention period"})
		}
		// Bank data (E15c): the rows matched to this party's invoices keep the payer name, reference and
		// description the bank reported, as accounting evidence. Unmatched or ignored rows may also name the
		// party, but nothing ties them to it, so they cannot be counted here; they are retained likewise.
		cnt, last = 0, sql.NullString{}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), MAX(b.booked_on) FROM bank_transactions b JOIN payments p ON p.id = b.matched_payment_id JOIN invoices i ON i.id = p.invoice_id WHERE i.party_id = ?`, id).Scan(&cnt, &last); err != nil {
			return err
		}
		if cnt > 0 {
			rep.Retained = append(rep.Retained, Retained{Class: "finance-bank", Count: cnt, Until: retainUntil(last.String, retentionYears),
				Reason: "bank transactions matched to this party's invoices keep the counterparty, reference and description the bank reported, as accounting evidence; other bank rows naming the party cannot be attributed and are kept under the same rule"})
		}
		return s.audit(ctx, tx, a.Subject, "party.erase", strconv.FormatInt(id, 10), map[string]any{"deleted_drafts": rep.DeletedDrafts, "retained": rep.Retained})
	})
	if err != nil {
		return nil, err
	}
	if err := s.Scrub(ctx); err != nil {
		return rep, fmt.Errorf("erased, but the write-ahead log could not be truncated: %w", err)
	}
	return rep, nil
}

func retainUntil(lastDate string, years int) string {
	t, err := time.Parse(time.DateOnly, lastDate)
	if err != nil {
		return ""
	}
	return t.AddDate(years, 0, 0).Format(time.DateOnly)
}

// ---- tax rates ------------------------------------------------------------------------

// TaxRate is a named rate. Rates are data: nothing in the code knows any jurisdiction's rules.
type TaxRate struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	RatePPM int64  `json:"rate_ppm"`
	Active  bool   `json:"active"`
}

// TaxRatePatch changes a rate; it applies to documents created from then on.
type TaxRatePatch struct {
	Name    *string `json:"name"`
	RatePPM *int64  `json:"rate_ppm"`
	Active  *bool   `json:"active"`
}

func (t *TaxRate) validate() error {
	var p problems
	if !codeRE.MatchString(t.Code) {
		p.add("code", "1 to 30 letters, digits, dot, dash or underscore")
	}
	if !oneLineText(t.Name, maxName) || strings.TrimSpace(t.Name) == "" {
		p.add("name", "required, one line")
	}
	if t.RatePPM < 0 || t.RatePPM > 1_000_000 {
		p.add("rate_ppm", "between 0 and 1000000 (parts per million: 200000 is 20%%)")
	}
	return p.err()
}

// CreateTaxRate adds a rate (finance.admin).
func (s *Store) CreateTaxRate(ctx context.Context, a *Principal, in TaxRate) (*TaxRate, error) {
	in.Active = true
	if err := in.validate(); err != nil {
		return nil, err
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tax_rates (code, name, rate_ppm, created_at) VALUES (?, ?, ?, ?)`, in.Code, in.Name, in.RatePPM, ts(s.now())); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%w: tax code %s exists", ErrConflict, in.Code)
			}
			return err
		}
		return s.audit(ctx, tx, a.Subject, "tax_rate.create", in.Code, map[string]any{"rate_ppm": in.RatePPM})
	})
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// UpdateTaxRate changes a rate (finance.admin).
func (s *Store) UpdateTaxRate(ctx context.Context, a *Principal, code string, in TaxRatePatch) (*TaxRate, error) {
	var out *TaxRate
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var t TaxRate
		var act bool
		if err := tx.QueryRowContext(ctx, `SELECT code, name, rate_ppm, active FROM tax_rates WHERE code = ?`, code).Scan(&t.Code, &t.Name, &t.RatePPM, &act); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		t.Active = act
		if in.Name != nil {
			t.Name = *in.Name
		}
		if in.RatePPM != nil {
			t.RatePPM = *in.RatePPM
		}
		if in.Active != nil {
			t.Active = *in.Active
		}
		if err := t.validate(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tax_rates SET name = ?, rate_ppm = ?, active = ? WHERE code = ?`, t.Name, t.RatePPM, t.Active, code); err != nil {
			return err
		}
		out = &t
		return s.audit(ctx, tx, a.Subject, "tax_rate.update", code, map[string]any{"rate_ppm": t.RatePPM, "active": t.Active})
	})
	return out, err
}

// ListTaxRates lists every rate.
func (s *Store) ListTaxRates(ctx context.Context) ([]TaxRate, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT code, name, rate_ppm, active FROM tax_rates ORDER BY code LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []TaxRate{}
	for rows.Next() {
		var t TaxRate
		if err := rows.Scan(&t.Code, &t.Name, &t.RatePPM, &t.Active); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// taxRate loads an active rate in a transaction; "" means no tax.
func taxRate(ctx context.Context, q dbq, code string) (int64, error) {
	if code == "" {
		return 0, nil
	}
	var ppm int64
	var active bool
	err := q.QueryRowContext(ctx, `SELECT rate_ppm, active FROM tax_rates WHERE code = ?`, code).Scan(&ppm, &active)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !active) {
		return 0, fmt.Errorf("tax code %q is unknown or inactive", code)
	}
	return ppm, err
}

// ---- products -------------------------------------------------------------------------

// Product is a catalogue entry used to fill invoice lines.
type Product struct {
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	Currency       string `json:"currency"`
	TaxCode        string `json:"tax_code"`
	Active         bool   `json:"active"`
	Version        int64  `json:"version"`
}

// ProductPatch changes a product.
type ProductPatch struct {
	Name           *string `json:"name"`
	Description    *string `json:"description"`
	UnitPriceMinor *int64  `json:"unit_price_minor"`
	TaxCode        *string `json:"tax_code"`
	Active         *bool   `json:"active"`
}

func (p *Product) validate() error {
	var pr problems
	if !codeRE.MatchString(p.SKU) {
		pr.add("sku", "1 to 30 letters, digits, dot, dash or underscore")
	}
	if !oneLineText(p.Name, maxName) || strings.TrimSpace(p.Name) == "" {
		pr.add("name", "required, one line")
	}
	if !validText(p.Description, 2000) {
		pr.add("description", "at most 2000 characters")
	}
	if p.UnitPriceMinor < 0 || p.UnitPriceMinor > maxMinor {
		pr.add("unit_price_minor", "between 0 and %d", maxMinor)
	}
	if _, ok := CurrencyExponent(p.Currency); !ok {
		pr.add("currency", "unknown currency code")
	}
	return pr.err()
}

const productSelect = `SELECT sku, name, description, unit_price_minor, currency, tax_code, active, version FROM products`

func scanProduct(sc scanner) (*Product, error) {
	var p Product
	err := sc.Scan(&p.SKU, &p.Name, &p.Description, &p.UnitPriceMinor, &p.Currency, &p.TaxCode, &p.Active, &p.Version)
	return &p, err
}

// CreateProduct adds a catalogue entry.
func (s *Store) CreateProduct(ctx context.Context, a *Principal, in Product) (*Product, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	var out *Product
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := taxRate(ctx, tx, in.TaxCode); err != nil {
			return invalid("tax_code", "%v", err)
		}
		now := ts(s.now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO products (sku, name, description, unit_price_minor, currency, tax_code, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			in.SKU, in.Name, in.Description, in.UnitPriceMinor, in.Currency, in.TaxCode, now, now); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%w: sku %s exists", ErrConflict, in.SKU)
			}
			return err
		}
		var err error
		out, err = scanProduct(tx.QueryRowContext(ctx, productSelect+` WHERE sku = ?`, in.SKU))
		return err
	})
	return out, err
}

// GetProduct returns a product.
func (s *Store) GetProduct(ctx context.Context, sku string) (*Product, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	p, err := scanProduct(s.rdb.QueryRowContext(rctx, productSelect+` WHERE sku = ?`, sku))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListProducts lists products by sku.
func (s *Store) ListProducts(ctx context.Context, activeOnly bool) ([]Product, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	q := productSelect
	if activeOnly {
		q += ` WHERE active = 1`
	}
	rows, err := s.rdb.QueryContext(rctx, q+` ORDER BY sku LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Product{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// UpdateProduct changes a product; a price change never alters an existing document.
func (s *Store) UpdateProduct(ctx context.Context, a *Principal, sku string, ifMatch *int64, in ProductPatch) (*Product, error) {
	var out *Product
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanProduct(tx.QueryRowContext(ctx, productSelect+` WHERE sku = ?`, sku))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != cur.Version {
			return ErrVersionMismatch
		}
		if in.Name != nil {
			cur.Name = *in.Name
		}
		if in.Description != nil {
			cur.Description = *in.Description
		}
		if in.UnitPriceMinor != nil {
			cur.UnitPriceMinor = *in.UnitPriceMinor
		}
		if in.TaxCode != nil {
			cur.TaxCode = *in.TaxCode
		}
		if in.Active != nil {
			cur.Active = *in.Active
		}
		if err := cur.validate(); err != nil {
			return err
		}
		if _, err := taxRate(ctx, tx, cur.TaxCode); err != nil {
			return invalid("tax_code", "%v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE products SET name=?, description=?, unit_price_minor=?, tax_code=?, active=?, version=version+1, updated_at=? WHERE sku=?`,
			cur.Name, cur.Description, cur.UnitPriceMinor, cur.TaxCode, cur.Active, ts(s.now()), sku); err != nil {
			return err
		}
		out, err = scanProduct(tx.QueryRowContext(ctx, productSelect+` WHERE sku = ?`, sku))
		return err
	})
	return out, err
}
