package equity

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"strings"
	"time"
)

// csvSafe defuses spreadsheet formula injection (OWASP "CSV injection"): a text cell that begins
// with = + - @ or a tab, carriage return or newline (after optional spaces) would be run as a
// formula by Excel, LibreOffice or Sheets, so it is prefixed with an apostrophe.
func csvSafe(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	t := strings.TrimLeft(s, " ")
	if t != "" && strings.ContainsRune("=+-@\t\r\n", rune(t[0])) {
		return "'" + s
	}
	return s
}

// num is a cell this package formatted as a number (never user text), written as is.
type num string

type csvOut struct{ cw *csv.Writer }

func newCSV(w io.Writer, header []string) (*csvOut, error) {
	c := &csvOut{cw: csv.NewWriter(w)}
	return c, c.cw.Write(header)
}

func (c *csvOut) row(cells ...any) error {
	rec := make([]string, len(cells))
	for i, cell := range cells {
		switch v := cell.(type) {
		case num:
			rec[i] = string(v)
		case string:
			rec[i] = csvSafe(v)
		}
	}
	return c.cw.Write(rec)
}

func (c *csvOut) close() error { c.cw.Flush(); return c.cw.Error() }

// ExportCSV writes one of "captable" (fully diluted), "grants" or "ledger" for an entity as CSV
// with names and amounts, for lawyers, auditors and investors. Exports are the highest-risk read:
// the whole data set is audited per record BEFORE a byte is written, and the CSV is built in
// memory first so a failure never leaves a half-written file. finance, board or admin.
func (s *Service) ExportCSV(ctx context.Context, a Actor, entityID, name, asof string, w io.Writer) error {
	if err := s.authorize(ctx, a, ActExport, Fact{}, "entity:"+entityID); err != nil {
		return err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return err
	}
	if asof == "" {
		asof = s.today()
	}
	if _, err := parseDate(asof); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	names := map[string]string{}
	nameOf := func(id string) (string, error) {
		if n, ok := names[id]; ok {
			return n, nil
		}
		idn, err := s.openIdentity(ctx, s.db, id)
		if err != nil {
			return "", err
		}
		names[id] = idn.Name
		return idn.Name, nil
	}
	var buf bytes.Buffer
	var resources []string
	fields := []string{FieldHoldings, FieldIdentity}
	switch name {
	case "captable":
		ct, _, err := s.capTable(ctx, s.db, entityID, asof, true)
		if err != nil {
			return err
		}
		out, err := newCSV(&buf, []string{"stakeholder_id", "holder", "kind", "basic_shares", "options_outstanding", "options_vested", "fully_diluted_shares", "fully_diluted_ownership_ppm"})
		if err != nil {
			return err
		}
		for _, h := range ct.Holders {
			n, err := nameOf(h.StakeholderID)
			if err != nil {
				return err
			}
			resources = append(resources, "stakeholder:"+h.StakeholderID)
			if err := out.row(h.StakeholderID, n, h.Kind, num(h.BasicShares.String()), num(h.OptionsOutstanding.String()), num(h.OptionsVested.String()), num(h.FullyDiluted.String()), num(itoa(*h.FullyDilutedPPM))); err != nil {
				return err
			}
		}
		for _, p := range ct.Pool {
			if err := out.row("", "[unallocated pool "+p.Name+"]", "pool", num("0"), num("0"), num("0"), num(p.Available.String()), num(itoa(p.FullyDilutedPPM))); err != nil {
				return err
			}
		}
		if err := out.close(); err != nil {
			return err
		}
	case "grants":
		fields = []string{FieldGrantTerms, FieldIdentity}
		rows, err := s.db.QueryContext(ctx, `SELECT `+grantCols+` FROM grants g WHERE g.entity_id = ? ORDER BY g.rowid LIMIT 50000`, entityID)
		if err != nil {
			return err
		}
		var gs []*grantRow
		for rows.Next() {
			g, err := scanGrant(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			gs = append(gs, g)
		}
		_ = rows.Close()
		out, err := newCSV(&buf, []string{"grant_id", "holder", "type", "status", "version", "quantity", "exercised", "strike", "currency", "grant_date", "vesting_start", "expiry_date", "approved_on"})
		if err != nil {
			return err
		}
		for _, g := range gs {
			n, err := nameOf(g.StakeholderID)
			if err != nil {
				return err
			}
			resources = append(resources, "grant:"+g.ID)
			if err := out.row(g.ID, n, g.Type, g.Status, num(itoa(int64(g.Version))), num(g.Quantity.String()), num(g.Exercised.String()), num(FormatMinor(g.StrikeMinor, g.Currency)), g.Currency, g.GrantDate, g.VestStart, g.ExpiryDate, g.ApprovedOn); err != nil {
				return err
			}
		}
		if err := out.close(); err != nil {
			return err
		}
	case "ledger":
		rows, err := s.db.QueryContext(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE entity_id = ? AND effective_date <= ? ORDER BY seq LIMIT 50000`, entityID, asof)
		if err != nil {
			return err
		}
		var es []LedgerEntry
		for rows.Next() {
			e, err := scanLedger(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			es = append(es, *e)
		}
		_ = rows.Close()
		out, err := newCSV(&buf, []string{"ledger_id", "effective_date", "holder", "class_id", "kind", "quantity", "price", "currency", "group_id"})
		if err != nil {
			return err
		}
		for _, e := range es {
			n, err := nameOf(e.StakeholderID)
			if err != nil {
				return err
			}
			resources = append(resources, "ledger:"+e.ID)
			price := ""
			if e.PriceMinor != nil {
				price = FormatMinor(*e.PriceMinor, e.Currency)
			}
			if err := out.row(e.ID, e.EffectiveDate, n, e.ClassID, e.Kind, num(e.Quantity.String()), num(price), e.Currency, e.GroupID); err != nil {
				return err
			}
		}
		if err := out.close(); err != nil {
			return err
		}
	default:
		return ErrNotFound
	}
	resources = append([]string{"export:" + name + ":" + entityID + "@" + asof}, resources...)
	if err := s.auditReadMany(ctx, a, "export", resources, fields); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}
