package finance

import (
	"net/http"
	"strconv"
	"strings"
)

// Handlers of the accounting API (E15c). Every route is registered in api.go with a finance.ledger.*
// permission, which only an unscoped, not own-only token can use (allowedBy). The handlers only decode,
// call the Store and encode: the rules live in the Store.

func (a *api) listAccounts(w http.ResponseWriter, r *http.Request, p *Principal) {
	items, err := a.store.ListAccounts(r.Context(), p, r.URL.Query().Get("include_inactive") == "true")
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) createAccount(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in AccountInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateAccount(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	w.Header().Set("Location", "/v1/ledger/accounts/"+itoaID(v.ID))
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) patchAccount(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in AccountPatch
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdateAccount(r.Context(), p, id, im, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) applyTemplate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Mappings bool `json:"apply_mappings"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	created, err := a.store.ApplyChartTemplate(r.Context(), p, in.Mappings)
	if err != nil {
		a.fail(w, err)
		return
	}
	if created == nil {
		created = []Account{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "template": DefaultChartTemplate().Name})
}

func (a *api) getMappings(w http.ResponseWriter, r *http.Request, p *Principal) {
	m, err := a.store.GetMappings(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *api) putMappings(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Mappings map[string]string `json:"mappings"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.store.SetMappings(r.Context(), p, in.Mappings); err != nil {
		a.fail(w, err)
		return
	}
	a.getMappings(w, r, p)
}

func (a *api) listJournals(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	items, next, err := a.store.ListJournals(r.Context(), p, JournalFilter{Status: q.Get("status"), Source: q.Get("source"), From: q.Get("from"), To: q.Get("to")}, before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createJournal(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in JournalInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.CreateJournal(r.Context(), p, in, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/v1/ledger/journals/"+itoaID(v.ID), 1, replayed, v)
}

func (a *api) getJournal(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetJournal(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) deleteJournal(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := a.store.DeleteJournalDraft(r.Context(), p, id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) postJournal(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.PostJournal(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) reverseJournal(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Date   string `json:"date"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.ReverseJournal(r.Context(), p, id, in.Date, in.Reason)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) trialBalance(w http.ResponseWriter, r *http.Request, p *Principal) {
	v, err := a.store.TrialBalanceAsOf(r.Context(), p, r.URL.Query().Get("as_of"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) generalLedger(w http.ResponseWriter, r *http.Request, p *Principal) {
	q := r.URL.Query()
	v, err := a.store.GeneralLedgerOf(r.Context(), p, q.Get("account"), q.Get("from"), q.Get("to"), q.Get("cursor"), queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) profitLoss(w http.ResponseWriter, r *http.Request, p *Principal) {
	q := r.URL.Query()
	v, err := a.store.ProfitLossBetween(r.Context(), p, q.Get("from"), q.Get("to"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) balanceSheet(w http.ResponseWriter, r *http.Request, p *Principal) {
	v, err := a.store.BalanceSheetAsOf(r.Context(), p, r.URL.Query().Get("as_of"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) listPeriods(w http.ResponseWriter, r *http.Request, p *Principal) {
	cur, hist, err := a.store.LedgerPeriods(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	var v any
	if cur != "" {
		v = cur
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed_through": v, "history": hist})
}

func (a *api) closePeriod(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Through string `json:"closed_through"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.store.ClosePeriod(r.Context(), p, in.Through); err != nil {
		a.fail(w, err)
		return
	}
	a.listPeriods(w, r, p)
}

func (a *api) reopenPeriod(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Through string `json:"closed_through"`
		Reason  string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.store.ReopenPeriod(r.Context(), p, in.Through, in.Reason); err != nil {
		a.fail(w, err)
		return
	}
	a.listPeriods(w, r, p)
}

func (a *api) listExports(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	items, next, err := a.store.ListExports(r.Context(), p, before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createExport(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Adapter string `json:"adapter"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.ExportJournal(r.Context(), p, in.Adapter, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	loc := "/v1/ledger/exports"
	if v.ID > 0 {
		loc += "/" + itoaID(v.ID) + "/download"
	}
	a.created(w, loc, 1, replayed, v)
}

func (a *api) downloadExport(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	b, content, err := a.store.ExportContent(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="journal-`+itoaID(b.ID)+`.csv"`)
	w.Header().Set("X-Content-SHA256", b.SHA256)
	_, _ = w.Write(content)
}

func (a *api) importPayroll(w http.ResponseWriter, r *http.Request, p *Principal) {
	res, err := a.store.ImportPayrollJournal(r.Context(), p, r.URL.Query().Get("adapter"), r.Body)
	if err != nil {
		a.fail(w, err)
		return
	}
	status := http.StatusCreated
	if res.Already {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

// ---- bank -----------------------------------------------------------------------------

func (a *api) listBankAccounts(w http.ResponseWriter, r *http.Request, p *Principal) {
	items, err := a.store.ListBankAccounts(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) createBankAccount(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in BankAccountInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateBankAccount(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	w.Header().Set("Location", "/v1/bank/accounts/"+itoaID(v.ID))
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) getBankAccount(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetBankAccount(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

var csvDelims = map[string]rune{"comma": ',', "semicolon": ';', "tab": '\t', "pipe": '|'}

// csvConfigFromQuery builds the explicit CSV configuration; nothing is guessed from the file.
func csvConfigFromQuery(r *http.Request) (CSVConfig, error) {
	q := r.URL.Query()
	var p problems
	num := func(name string) int {
		v := q.Get(name)
		if v == "" {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 200 {
			p.add(name, "a column number from 1 to 200")
			return 0
		}
		return n
	}
	cfg := CSVConfig{
		HasHeader: q.Get("header") != "false",
		DateCol:   num("date_col"), AmountCol: num("amount_col"), DebitCol: num("debit_col"), CreditCol: num("credit_col"),
		ReferenceCol: num("reference_col"), DescriptionCol: num("description_col"), CounterpartyCol: num("counterparty_col"), IDCol: num("id_col"),
		DateLayout: q.Get("date_layout"),
	}
	d, ok := csvDelims[q.Get("delimiter")]
	if !ok {
		p.add("delimiter", "required: comma, semicolon, tab or pipe")
	}
	cfg.Delimiter = d
	switch q.Get("decimal") {
	case "", "dot":
		cfg.DecimalSep = '.'
	case "comma":
		cfg.DecimalSep = ','
	default:
		p.add("decimal", "dot or comma")
	}
	switch q.Get("thousands") {
	case "", "none":
	case "comma":
		cfg.ThousandsSep = ','
	case "dot":
		cfg.ThousandsSep = '.'
	case "space":
		cfg.ThousandsSep = ' '
	default:
		p.add("thousands", "none, comma, dot or space")
	}
	if err := p.err(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (a *api) importStatement(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	var cfg CSVConfig
	if format == "csv" {
		var err error
		if cfg, err = csvConfigFromQuery(r); err != nil {
			a.fail(w, err)
			return
		}
	}
	res, replayed, err := a.store.ImportStatement(r.Context(), p, id, format, r.Body, cfg, r.URL.Query().Get("force_duplicates") == "true", idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	loc := "/v1/bank/transactions?account_id=" + itoaID(id)
	if replayed || res.AlreadyImported {
		w.Header().Set("Location", loc)
		if replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	a.created(w, loc, 1, false, res)
}

func (a *api) listBankTxns(w http.ResponseWriter, r *http.Request, p *Principal) {
	after, ok := cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	acct, _ := strconv.ParseInt(q.Get("account_id"), 10, 64)
	items, next, err := a.store.ListBankTransactions(r.Context(), p, TxnFilter{AccountID: acct, Status: q.Get("status"), From: q.Get("from"), To: q.Get("to")}, after, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) suggestMatches(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	items, err := a.store.SuggestMatches(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "advice_only": true})
}

func (a *api) confirmMatch(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		InvoiceID int64 `json:"invoice_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.ConfirmMatch(r.Context(), p, id, in.InvoiceID, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) ignoreTxn(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.IgnoreTransaction(r.Context(), p, id, in.Reason)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) reconciliation(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	items, err := a.store.Reconciliation(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) signOffReconciliation(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Period string `json:"period"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.store.SignOffReconciliation(r.Context(), p, id, in.Period); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"bank_account_id": id, "period": in.Period, "status": "reconciled"})
}
