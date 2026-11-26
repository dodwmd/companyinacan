package finance

// CSV import of parties (specs/047-csv-import). All or nothing: the whole file is applied in
// one transaction with its audit entries, and a preview is the same transaction rolled back.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

// defaultImportTimeout bounds the write transaction of an import (Store.importTimeout).
const defaultImportTimeout = 15 * time.Second

const erasedMessage = "this external_id belongs to an erased party; remove the row"

// externalHash is what is left of an erased party's external id once the party is purged.
func externalHash(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// keepErasedIDs records the ids of the parties the purge is about to delete.
func (s *Store) keepErasedIDs(ctx context.Context, tx dbq) error {
	rows, err := tx.QueryContext(ctx, `SELECT external_id FROM parties WHERE external_id IS NOT NULL AND `+purgeableParties)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO erased_external_ids (hash, purged_at) VALUES (?, ?)`, externalHash(id), ts(s.now())); err != nil {
			return err
		}
	}
	return nil
}

var partyImportFields = []csvimport.Field{
	{Name: csvimport.ExternalID, Required: true}, {Name: "name", Required: true}, {Name: "kind"}, {Name: "contact_name"},
	{Name: "email"}, {Name: "phone"}, {Name: "address"}, {Name: "tax_id"}, {Name: "currency"}, {Name: "notes"},
}

// ImportParties previews or applies a CSV of customers and vendors. It needs finance.write and
// a principal that is not limited to parties, projects or own records: such a token could not
// see all of what the file would match.
func (s *Store) ImportParties(ctx context.Context, a *Principal, r io.Reader, o csvimport.Options) (*csvimport.Report, error) {
	if !a.Can(PermWrite) || !a.Unscoped() || a.OwnOnly {
		return nil, ErrForbidden
	}
	if !s.importing.TryLock() {
		return nil, csvimport.ErrBusy
	}
	defer s.importing.Unlock()
	f, err := csvimport.Read(r, partyImportFields, o.Mapping)
	if err != nil {
		return nil, err
	}
	rep := csvimport.NewReport(f, o)
	ctx, cancel := context.WithTimeout(ctx, s.importTimeout)
	defer cancel()
	summary := map[string]any{"file_sha256": f.SHA256, "rows": len(f.Rows)}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		names, err := partyNames(ctx, tx)
		if err != nil {
			return err
		}
		for _, row := range f.Rows {
			item := rep.Row(row)
			if item.Failed() {
				continue
			}
			if err := s.importParty(ctx, tx, a, row, item, names); err != nil {
				return err
			}
		}
		if !o.Apply || rep.HasErrors() {
			return csvimport.ErrRollback
		}
		return s.audit(ctx, tx, a.Subject, "party.import", "", summary)
	})
	switch {
	case errors.Is(err, csvimport.ErrRollback):
		// The report says which ids already exist and which match, so a run that wrote nothing
		// is still recorded; without the record there is no report.
		if err := s.auditRead(ctx, a, "party.import_preview", "", summary); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	rep.Finish()
	return rep, nil
}

// partyNames are the lower-case names in use, to warn about likely duplicates.
func partyNames(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM parties WHERE erased_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names[strings.ToLower(n)] = true
	}
	return names, rows.Err()
}

func (s *Store) importParty(ctx context.Context, tx *sql.Tx, a *Principal, row csvimport.Row, item *csvimport.Item, names map[string]bool) error {
	external := row.Get(csvimport.ExternalID)
	var id int64
	var erased sql.NullInt64
	var cur NewParty
	err := tx.QueryRowContext(ctx, `SELECT id, erased_at, kind, name, contact_name, email, phone, address, tax_id, currency, notes FROM parties WHERE external_id = ?`, external).
		Scan(&id, &erased, &cur.Kind, &cur.Name, &cur.ContactName, &cur.Email, &cur.Phone, &cur.Address, &cur.TaxID, &cur.Currency, &cur.Notes)
	isNew := errors.Is(err, sql.ErrNoRows)
	switch {
	case isNew:
		var purged int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM erased_external_ids WHERE hash = ?`, externalHash(external)).Scan(&purged); err != nil {
			return err
		}
		if purged > 0 {
			item.Error(csvimport.ExternalID, erasedMessage)
			return nil
		}
		cur = NewParty{Kind: "customer"}
	case err != nil:
		return err
	case erased.Valid:
		item.Error(csvimport.ExternalID, erasedMessage)
		return nil
	}
	var changed []string
	for _, c := range []struct {
		field string
		dst   *string
	}{{"kind", &cur.Kind}, {"name", &cur.Name}, {"contact_name", &cur.ContactName}, {"email", &cur.Email}, {"phone", &cur.Phone},
		{"address", &cur.Address}, {"tax_id", &cur.TaxID}, {"currency", &cur.Currency}, {"notes", &cur.Notes}} {
		if v := row.Get(c.field); v != "" && v != *c.dst { // an empty cell keeps the stored value
			*c.dst = v
			changed = append(changed, c.field)
		}
	}
	if row.Get("name") == "" {
		item.Error("name", "required")
		return nil
	}
	if err := cur.validate(); err != nil {
		var inv *InvalidError
		if !errors.As(err, &inv) {
			return err
		}
		for _, p := range inv.Problems {
			item.Error(p.Field, p.Message)
		}
		return nil
	}
	if !isNew && cur.Kind == "customer" && slices.Contains(changed, "kind") {
		if err := mustStayVendor(ctx, tx, id); errors.Is(err, errMustStayVendor) {
			item.Error("kind", "the party has purchase orders and must stay a vendor")
			return nil
		} else if err != nil {
			return err
		}
	}
	switch {
	case isNew:
		if names[strings.ToLower(cur.Name)] {
			item.Warn("name", "a party with this name already exists")
		}
		now := ts(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO parties (kind, name, contact_name, email, phone, address, tax_id, currency, notes, external_id, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cur.Kind, cur.Name, cur.ContactName, cur.Email, cur.Phone, cur.Address, cur.TaxID, cur.Currency, cur.Notes, external, a.Subject, now, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		item.Create(strconv.FormatInt(id, 10))
	case len(changed) > 0:
		if _, err := tx.ExecContext(ctx, `UPDATE parties SET kind=?, name=?, contact_name=?, email=?, phone=?, address=?, tax_id=?, currency=?, notes=?, version=version+1, updated_at=? WHERE id=?`,
			cur.Kind, cur.Name, cur.ContactName, cur.Email, cur.Phone, cur.Address, cur.TaxID, cur.Currency, cur.Notes, ts(s.now()), id); err != nil {
			return err
		}
		item.Update(strconv.FormatInt(id, 10), changed)
	default:
		item.Update(strconv.FormatInt(id, 10), nil)
		return nil
	}
	names[strings.ToLower(cur.Name)] = true
	// A preview or a failed import rolls this back with the record.
	detail := map[string]any{"result": item.Action}
	if len(changed) > 0 && !isNew {
		detail["fields"] = changed
	}
	return s.audit(ctx, tx, a.Subject, "party.import", strconv.FormatInt(id, 10), detail)
}

// importParties is POST /v1/import/parties: the body is the CSV.
func (a *api) importParties(w http.ResponseWriter, r *http.Request, p *Principal) {
	var rep *csvimport.Report
	o, err := csvimport.ParseQuery(r.URL.Query())
	if err == nil {
		rep, err = a.store.ImportParties(r.Context(), p, r.Body, o)
	}
	if status, code, ok := csvimport.Failure(err); ok {
		writeError(w, status, code, err.Error())
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, rep.Status(), rep)
}
