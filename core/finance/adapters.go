package finance

// Accounting and payroll adapter seams (E15c). The module defines two interfaces and ships exactly the
// generic file adapters: a CSV journal export and a CSV payroll journal import. The named accounting
// products (xero, quickbooks, wave) are documented stubs: they are registered so that the API can say
// "not implemented" precisely, and they make no network call, hold no credentials and never will inside
// this module. Payroll is only ever an inbound journal: no payroll is calculated here.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// exportBatchSize bounds one export batch.
const exportBatchSize = 500

// PayrollDisclaimer is returned with every payroll import and printed in docs/accounting.md.
const PayrollDisclaimer = "This module does not calculate payroll, tax, super or any statutory amount. A payroll journal is read from a file produced by your payroll provider, checked only for balance, and held as a draft until someone other than the importer approves it. Verify it with your payroll provider and accountant."

// AccountingExporter turns a batch of posted journal entries into a file for an accounting system.
type AccountingExporter interface {
	// Name is the adapter's id in the API.
	Name() string
	// Implemented is false for a documented stub: Export then returns ErrNotImplemented and writes nothing.
	Implemented() bool
	// Export writes the entries, oldest first, to w.
	Export(ctx context.Context, entries []JournalEntry, w io.Writer) error
}

// PayrollAdapter reads a payroll provider's journal file into manual journal drafts. It is inbound only.
type PayrollAdapter interface {
	Name() string
	// ParseJournal reads at most maxStatementBytes and returns balanced journals grouped by date and currency.
	ParseJournal(r io.Reader) ([]JournalInput, error)
}

var (
	exporters = map[string]AccountingExporter{
		"csv":        csvExporter{},
		"xero":       stubExporter{"xero"},
		"quickbooks": stubExporter{"quickbooks"},
		"wave":       stubExporter{"wave"},
	}
	payrollAdapters = map[string]PayrollAdapter{"csv": csvPayroll{}}
)

// ExporterNames lists the registered adapters.
func ExporterNames() []string { return sortedKeys(exporters) }

type stubExporter struct{ name string }

func (s stubExporter) Name() string      { return s.name }
func (s stubExporter) Implemented() bool { return false }
func (s stubExporter) Export(context.Context, []JournalEntry, io.Writer) error {
	return fmt.Errorf("%w: the %s adapter is a documented stub; it makes no network call. Use the csv adapter and import the file", ErrNotImplemented, s.name)
}

type csvExporter struct{}

func (csvExporter) Name() string      { return "csv" }
func (csvExporter) Implemented() bool { return true }

// Export writes one row per journal line. Text cells are defused against spreadsheet formula injection.
func (csvExporter) Export(_ context.Context, entries []JournalEntry, w io.Writer) error {
	out, err := newCSV(w, []string{"entry_number", "entry_date", "source", "source_ref", "memo", "currency", "line", "account_code", "account_name", "debit", "credit", "line_memo"})
	if err != nil {
		return err
	}
	for _, e := range entries {
		for _, l := range e.Lines {
			var debit, credit Num
			if l.DebitMinor > 0 {
				debit = Num(FormatMinor(l.DebitMinor, e.Currency))
			}
			if l.CreditMinor > 0 {
				credit = Num(FormatMinor(l.CreditMinor, e.Currency))
			}
			if err := out.row(deref(e.Number), e.EntryDate, e.Source, e.SourceRef, e.Memo, e.Currency, Num(itoa(int64(l.Position))), l.AccountCode, l.AccountName, debit, credit, l.Memo); err != nil {
				return err
			}
		}
	}
	return out.close()
}

// ExportBatch describes an export. Content is never in the JSON: download it by id.
type ExportBatch struct {
	ID      int64  `json:"id"`
	Adapter string `json:"adapter"`
	// FirstNumber and LastNumber are posting numbers (the n of JE-0000n).
	FirstNumber int64     `json:"first_number"`
	LastNumber  int64     `json:"last_number"`
	EntryCount  int       `json:"entry_count"`
	SHA256      string    `json:"sha256"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// ExportJournal exports, in posting order, the posted entries after the adapter's cursor, at most exportBatchSize, as one
// batch. The batch, the cursor and the audit entry are written in one transaction, so an entry is in
// exactly one batch per adapter: running it twice never exports anything twice, and with an
// Idempotency-Key a retry returns the same batch. Nothing to export returns a batch with ID 0.
func (s *Store) ExportJournal(ctx context.Context, a *Principal, adapter, idemKey string) (*ExportBatch, bool, error) {
	ex, ok := exporters[adapter]
	if !ok {
		return nil, false, invalid("adapter", "one of %s", strings.Join(ExporterNames(), ", "))
	}
	if !ex.Implemented() {
		return nil, false, ex.Export(ctx, nil, io.Discard)
	}
	var out *ExportBatch
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "ledger.export."+adapter, map[string]string{"adapter": adapter}, func() (*ExportBatch, error) {
			var cursor int64
			if err := tx.QueryRowContext(ctx, `SELECT last_number FROM export_cursors WHERE adapter = ?`, adapter).Scan(&cursor); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			rows, err := tx.QueryContext(ctx, `SELECT id, number FROM journal_entries WHERE status = 'posted' AND number > ? ORDER BY number LIMIT ?`, cursor, exportBatchSize)
			if err != nil {
				return nil, err
			}
			var ids, numbers []int64
			for rows.Next() {
				var id, number int64
				if err := rows.Scan(&id, &number); err != nil {
					_ = rows.Close()
					return nil, err
				}
				ids, numbers = append(ids, id), append(numbers, number)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				return &ExportBatch{Adapter: adapter, SHA256: "", CreatedAt: s.now().UTC()}, nil
			}
			entries := make([]JournalEntry, 0, len(ids))
			for _, id := range ids {
				e, err := s.loadEntry(ctx, tx, id)
				if err != nil {
					return nil, err
				}
				entries = append(entries, *e)
			}
			var buf bytes.Buffer
			if err := ex.Export(ctx, entries, &buf); err != nil {
				return nil, err
			}
			sum := sha256.Sum256(buf.Bytes())
			b := &ExportBatch{Adapter: adapter, FirstNumber: numbers[0], LastNumber: numbers[len(numbers)-1], EntryCount: len(ids), SHA256: hex.EncodeToString(sum[:]), CreatedBy: a.Subject, CreatedAt: s.now().UTC()}
			res, err := tx.ExecContext(ctx, `INSERT INTO export_batches (adapter, first_number, last_number, entry_count, sha256, content, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				adapter, b.FirstNumber, b.LastNumber, b.EntryCount, b.SHA256, buf.String(), a.Subject, ts(s.now()))
			if err != nil {
				return nil, err
			}
			b.ID, _ = res.LastInsertId()
			if _, err := tx.ExecContext(ctx, `INSERT INTO export_cursors (adapter, last_number) VALUES (?, ?) ON CONFLICT (adapter) DO UPDATE SET last_number = excluded.last_number`, adapter, b.LastNumber); err != nil {
				return nil, err
			}
			if err := s.audit(ctx, tx, a.Subject, "ledger.export", fmt.Sprint(b.ID), map[string]any{"adapter": adapter, "entries": b.EntryCount, "first": b.FirstNumber, "last": b.LastNumber, "sha256": b.SHA256}); err != nil {
				return nil, err
			}
			return b, nil
		})
		return err
	})
	return out, replayed, err
}

// ListExports lists batches, newest first, before the cursor id.
func (s *Store) ListExports(ctx context.Context, a *Principal, before int64, limit int) ([]ExportBatch, int64, error) {
	limit = clampLimit(limit)
	cancel, rctx := readCtx(ctx)
	defer cancel()
	q := `SELECT id, adapter, first_number, last_number, entry_count, sha256, created_by, created_at FROM export_batches`
	var args []any
	if before > 0 {
		q += ` WHERE (id < ?)`
		args = append(args, before)
	}
	rows, err := s.rdb.QueryContext(rctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []ExportBatch{}
	for rows.Next() {
		var b ExportBatch
		var c int64
		if err := rows.Scan(&b.ID, &b.Adapter, &b.FirstNumber, &b.LastNumber, &b.EntryCount, &b.SHA256, &b.CreatedBy, &c); err != nil {
			return nil, 0, err
		}
		b.CreatedAt = fromUnix(c)
		out = append(out, b)
	}
	var next int64
	if len(out) == limit {
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

// ExportContent returns a batch's file, byte for byte. Downloads are audited before anything is returned.
func (s *Store) ExportContent(ctx context.Context, a *Principal, id int64) (*ExportBatch, []byte, error) {
	if err := s.auditRead(ctx, a, "ledger.export.download", fmt.Sprint(id), map[string]any{}); err != nil {
		return nil, nil, err
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	var b ExportBatch
	var c int64
	var content string
	err := s.rdb.QueryRowContext(rctx, `SELECT id, adapter, first_number, last_number, entry_count, sha256, created_by, created_at, content FROM export_batches WHERE id = ?`, id).
		Scan(&b.ID, &b.Adapter, &b.FirstNumber, &b.LastNumber, &b.EntryCount, &b.SHA256, &b.CreatedBy, &c, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	b.CreatedAt = fromUnix(c)
	return &b, []byte(content), err
}

// ---- payroll ------------------------------------------------------------------------------

type csvPayroll struct{}

func (csvPayroll) Name() string { return "csv" }

var payrollHeader = []string{"date", "account_code", "debit", "credit", "currency", "memo"}

// ParseJournal reads a CSV with exactly the header date,account_code,debit,credit,currency,memo. Rows with
// the same date and currency form one journal; every journal must balance.
func (csvPayroll) ParseJournal(r io.Reader) ([]JournalInput, error) {
	b, err := readStatement(r)
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(bytes.NewReader(b))
	cr.FieldsPerRecord = len(payrollHeader)
	cr.ReuseRecord = true
	hdr, err := cr.Read()
	if err != nil || strings.Join(hdr, ",") != strings.Join(payrollHeader, ",") {
		return nil, parseErr("the header must be exactly %s", strings.Join(payrollHeader, ","))
	}
	type key struct{ date, cur string }
	groups := map[key]*JournalInput{}
	var order []key
	rows := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, parseErr("the file is not valid CSV near row %d", rows+2)
		}
		rows++
		if rows > maxStatementRows {
			return nil, parseErr("more than %d rows", maxStatementRows)
		}
		date, code, cur := strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1]), strings.TrimSpace(rec[4])
		exp, ok := CurrencyExponent(cur)
		if !validDate(date) || !ok || !codeRE.MatchString(code) {
			return nil, parseErr("row %d: bad date, account code or currency", rows+1)
		}
		var debit, credit int64
		if s := strings.TrimSpace(rec[2]); s != "" {
			if debit, err = parseDecimalMinor(s, exp, '.', 0); err != nil || debit < 0 {
				return nil, parseErr("row %d: bad debit", rows+1)
			}
		}
		if s := strings.TrimSpace(rec[3]); s != "" {
			if credit, err = parseDecimalMinor(s, exp, '.', 0); err != nil || credit < 0 {
				return nil, parseErr("row %d: bad credit", rows+1)
			}
		}
		memo, err := cleanText(rec[5], 200)
		if err != nil {
			return nil, err
		}
		k := key{date, cur}
		g := groups[k]
		if g == nil {
			g = &JournalInput{EntryDate: date, Currency: cur, Memo: "Payroll journal " + date}
			groups[k] = g
			order = append(order, k)
		}
		if len(g.Lines) >= maxJournalLines {
			return nil, parseErr("more than %d lines for %s %s", maxJournalLines, date, cur)
		}
		g.Lines = append(g.Lines, JournalLineInput{AccountCode: code, DebitMinor: debit, CreditMinor: credit, Memo: memo})
	}
	if len(order) == 0 {
		return nil, parseErr("the file has no rows")
	}
	out := make([]JournalInput, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out, nil
}

// PayrollImportResult lists the drafts an import created.
type PayrollImportResult struct {
	DraftIDs   []int64 `json:"draft_ids"`
	Already    bool    `json:"already_imported"`
	Disclaimer string  `json:"disclaimer"`
}

// ImportPayrollJournal reads a payroll journal file into draft manual journals. They post only when
// someone other than the importer approves them (PostJournal). Importing the same file again makes only the journals whose drafts were deleted.
func (s *Store) ImportPayrollJournal(ctx context.Context, a *Principal, adapter string, body io.Reader) (*PayrollImportResult, error) {
	ad, ok := payrollAdapters[adapter]
	if !ok {
		return nil, invalid("adapter", "one of %s", strings.Join(sortedKeys(payrollAdapters), ", "))
	}
	data, err := readStatement(body)
	if err != nil {
		return nil, invalid("file", "%s", err.Error())
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	journals, err := ad.ParseJournal(bytes.NewReader(data))
	if err != nil {
		var pe *errParse
		if errors.As(err, &pe) {
			return nil, invalid("file", "%s", pe.msg)
		}
		return nil, err
	}
	res := &PayrollImportResult{DraftIDs: []int64{}, Disclaimer: PayrollDisclaimer}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		// A journal of this file that still exists (draft or posted) is never made twice; one whose draft was
		// deleted is made again, so a deleted draft can be re-imported.
		res.DraftIDs, res.Already = []int64{}, true
		for _, j := range journals {
			ref := hash[:16] + ":" + j.EntryDate + ":" + j.Currency
			if id, ok, err := s.entryExists(ctx, tx, "payroll", ref); err != nil {
				return err
			} else if ok {
				res.DraftIDs = append(res.DraftIDs, id)
				continue
			}
			lines, err := s.resolveLines(ctx, tx, j.Lines)
			if err != nil {
				return err
			}
			id, err := s.insertDraftEntry(ctx, tx, postSpec{Source: "payroll", Ref: ref, Date: j.EntryDate, Currency: j.Currency, Memo: j.Memo, By: a.Subject, Lines: lines})
			if err != nil {
				return err
			}
			res.DraftIDs, res.Already = append(res.DraftIDs, id), false
		}
		if res.Already {
			return nil
		}
		ids, _ := json.Marshal(res.DraftIDs)
		if _, err := tx.ExecContext(ctx, `INSERT INTO payroll_imports (sha256, entry_ids, imported_by, imported_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (sha256) DO UPDATE SET entry_ids = excluded.entry_ids, imported_by = excluded.imported_by, imported_at = excluded.imported_at`, hash, string(ids), a.Subject, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "ledger.payroll.import", hash[:16], map[string]any{"drafts": len(res.DraftIDs)})
	})
	return res, err
}
