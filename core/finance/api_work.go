package finance

import (
	"io"
	"net/http"
	"strconv"
	"time"
)

func itoaID(id int64) string { return strconv.FormatInt(id, 10) }

// ---- contracts ------------------------------------------------------------------------

func (a *api) respondContract(w http.ResponseWriter, v *Contract, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) listTemplates(w http.ResponseWriter, r *http.Request, _ *Principal) {
	items, err := a.store.ListTemplates(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) createTemplate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewTemplate
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateTemplate(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) listContracts(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	party, _ := strconv.ParseInt(r.URL.Query().Get("party_id"), 10, 64)
	items, next, err := a.store.ListContracts(r.Context(), p, r.URL.Query().Get("status"), party, before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createContract(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewContract
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateContract(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	w.Header().Set("Location", "/v1/contracts/"+itoaID(v.ID))
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) getContract(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetContract(r.Context(), p, id)
	a.respondContract(w, v, err)
}

func (a *api) newVersion(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Body      string            `json:"body"`
		Variables map[string]string `json:"variables"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.NewVersion(r.Context(), p, id, im, in.Body, in.Variables)
	a.respondContract(w, v, err)
}

func (a *api) getVersion(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	n, ok := pathID(w, r, "n")
	if !ok {
		return
	}
	v, err := a.store.GetVersion(r.Context(), p, id, n)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) sendContract(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in SendRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.SendContract(r.Context(), p, a.esign, id, im, in)
	a.respondContract(w, v, err)
}

// recordSignature records a signature that happened outside a provider (wet ink, another
// tool). The caller supplies the SHA-256 of the document that was signed.
func (a *api) recordSignature(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		SHA256 string `json:"sha256"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.RecordSignature(r.Context(), p, id, "", in.SHA256, p.Subject)
	a.respondContract(w, v, err)
}

func (a *api) contractDates(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in ContractDates
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdateContractDates(r.Context(), p, id, im, in)
	a.respondContract(w, v, err)
}

func (a *api) terminateContract(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in reasonBody
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.TerminateContract(r.Context(), p, id, im, in.Reason)
	a.respondContract(w, v, err)
}

func (a *api) verifyContract(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.VerifyContract(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) sweepContracts(w http.ResponseWriter, r *http.Request, _ *Principal) {
	var in struct {
		AsOf string `json:"as_of"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	if in.AsOf == "" {
		in.AsOf = dateOf(a.now())
	}
	n, err := a.store.SweepContracts(r.Context(), in.AsOf)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": n})
}

// esignWebhook receives the e-signature service's "signed" notification, authenticated by
// its signature. The document hash it reports must match the contract's current text.
func (a *api) esignWebhook(w http.ResponseWriter, r *http.Request) {
	if a.esign == nil {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		return
	}
	n, err := a.esign.ParseWebhook(r.Header, body)
	if err != nil {
		a.reject(w, r)
		return
	}
	v, err := a.store.RecordSignature(r.Context(), nil, 0, n.ExternalID, n.SHA256, "esign:"+a.esign.Name())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contract_id": v.ID, "status": v.Status})
}

// ---- timesheets -----------------------------------------------------------------------

func (a *api) respondTimesheet(w http.ResponseWriter, v *Timesheet, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) listTimesheets(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	items, next, err := a.store.ListTimesheets(r.Context(), p, TimesheetFilter{Owner: q.Get("owner"), Status: q.Get("status"), From: q.Get("from"), To: q.Get("to")}, before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createTimesheet(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Owner     string `json:"owner"`
		WeekStart string `json:"week_start"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, created, err := a.store.CreateTimesheet(r.Context(), p, in.Owner, in.WeekStart)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/v1/timesheets/"+itoaID(v.ID))
	}
	writeJSON(w, status, v)
}

func (a *api) getTimesheet(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetTimesheet(r.Context(), p, id)
	a.respondTimesheet(w, v, err)
}

func (a *api) setEntries(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Entries []EntryInput `json:"entries"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.SetEntries(r.Context(), p, id, im, in.Entries)
	a.respondTimesheet(w, v, err)
}

func (a *api) submitTimesheet(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	v, err := a.store.SubmitTimesheet(r.Context(), p, id, im)
	a.respondTimesheet(w, v, err)
}

func (a *api) approveTimesheet(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	v, err := a.store.ApproveTimesheet(r.Context(), p, id, im)
	a.respondTimesheet(w, v, err)
}

func (a *api) rejectTimesheet(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in reasonBody
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.RejectTimesheet(r.Context(), p, id, im, in.Reason)
	a.respondTimesheet(w, v, err)
}

func (a *api) billTime(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in BillingRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.BillTime(r.Context(), p, in, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/v1/invoices/"+itoaID(v.ID), v.Version, replayed, v)
}

func (a *api) getLock(w http.ResponseWriter, r *http.Request, _ *Principal) {
	d, err := a.store.PeriodLock(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	var v any
	if d != "" {
		v = d
	}
	writeJSON(w, http.StatusOK, map[string]any{"locked_through": v})
}

func (a *api) setLock(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Through string `json:"locked_through"`
		Note    string `json:"note"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.store.LockPeriod(r.Context(), p, in.Through, in.Note); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"locked_through": in.Through})
}

// ---- expenses -------------------------------------------------------------------------

func (a *api) respondExpense(w http.ResponseWriter, v *Expense, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) listExpenses(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	items, next, err := a.store.ListExpenses(r.Context(), p, q.Get("status"), q.Get("owner"), before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createExpense(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in ExpenseInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.CreateExpense(r.Context(), p, in, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/v1/expenses/"+itoaID(v.ID), v.Version, replayed, v)
}

func (a *api) getExpense(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetExpense(r.Context(), p, id)
	a.respondExpense(w, v, err)
}

func (a *api) updateExpense(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in ExpenseInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdateExpense(r.Context(), p, id, im, in)
	a.respondExpense(w, v, err)
}

func (a *api) submitExpense(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	v, err := a.store.SubmitExpense(r.Context(), p, id, im)
	a.respondExpense(w, v, err)
}

func (a *api) approveExpense(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	v, err := a.store.ApproveExpense(r.Context(), p, id, im)
	a.respondExpense(w, v, err)
}

func (a *api) rejectExpense(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in reasonBody
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.RejectExpense(r.Context(), p, id, im, in.Reason)
	a.respondExpense(w, v, err)
}

func (a *api) reimburseExpense(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Reference string `json:"reference"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.ReimburseExpense(r.Context(), p, id, im, in.Reference)
	a.respondExpense(w, v, err)
}

// ---- purchase orders ------------------------------------------------------------------

func (a *api) respondPO(w http.ResponseWriter, v *PurchaseOrder, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) listPOs(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	items, next, err := a.store.ListPurchaseOrders(r.Context(), p, r.URL.Query().Get("status"), before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createPO(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in NewPurchaseOrder
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.CreatePurchaseOrder(r.Context(), p, in, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/v1/purchase-orders/"+itoaID(v.ID), v.Version, replayed, v)
}

func (a *api) getPO(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetPurchaseOrder(r.Context(), p, id)
	a.respondPO(w, v, err)
}

func (a *api) updatePO(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in NewPurchaseOrder
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdatePurchaseOrder(r.Context(), p, id, im, in)
	a.respondPO(w, v, err)
}

func (a *api) approvePO(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	v, err := a.store.ApprovePurchaseOrder(r.Context(), p, id, im)
	a.respondPO(w, v, err)
}

func (a *api) poStatus(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.SetPurchaseOrderStatus(r.Context(), p, id, im, in.Status)
	a.respondPO(w, v, err)
}

// ---- tokens, audit, events ------------------------------------------------------------

func (a *api) listTokens(w http.ResponseWriter, r *http.Request, _ *Principal) {
	items, err := a.store.ListTokens(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) createToken(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewToken
	if !decodeJSON(w, r, &in) {
		return
	}
	info, secret, err := a.store.CreateToken(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": secret, "info": info})
}

func (a *api) revokeToken(w http.ResponseWriter, r *http.Request, p *Principal) {
	if err := a.store.RevokeToken(r.Context(), p, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listAudit(w http.ResponseWriter, r *http.Request, _ *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListAudit(r.Context(), before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	out := map[string]any{"items": items}
	if len(items) > 0 && len(items) == clampLimit(queryInt(r, "limit")) {
		out["next_cursor"] = encodeCursor(items[len(items)-1].ID)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) listDead(w http.ResponseWriter, r *http.Request, _ *Principal) {
	after, ok := cursor(w, r)
	if !ok {
		return
	}
	items, next, err := a.store.ListDeadEvents(r.Context(), after, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) replayEvent(w http.ResponseWriter, r *http.Request, p *Principal) {
	if err := a.store.ReplayEvent(r.Context(), p, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var _ = time.Second
