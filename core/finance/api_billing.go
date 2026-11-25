package finance

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ---- parties, catalogue ---------------------------------------------------------------

func (a *api) listParties(w http.ResponseWriter, r *http.Request, p *Principal) {
	after, ok := cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	items, next, err := a.store.ListParties(r.Context(), p, PartyFilter{Kind: q.Get("kind"), Search: q.Get("q")}, after, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createParty(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewParty
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateParty(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	w.Header().Set("Location", "/v1/parties/"+strconv.FormatInt(v.ID, 10))
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) getParty(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetParty(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) patchParty(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in PartyPatch
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdateParty(r.Context(), p, id, im, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) eraseParty(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rep, err := a.store.ErasePartyData(r.Context(), p, id, a.cfg.RetentionYears)
	if err != nil {
		if rep != nil { // erased, but the log truncation failed: say so, with the report
			a.log.Error("erasure follow-up failed", "error", err)
			writeJSON(w, http.StatusOK, rep)
			return
		}
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (a *api) listTaxRates(w http.ResponseWriter, r *http.Request, _ *Principal) {
	items, err := a.store.ListTaxRates(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) createTaxRate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in TaxRate
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateTaxRate(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) patchTaxRate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in TaxRatePatch
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdateTaxRate(r.Context(), p, r.PathValue("code"), in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) listProducts(w http.ResponseWriter, r *http.Request, _ *Principal) {
	items, err := a.store.ListProducts(r.Context(), r.URL.Query().Get("active") == "true")
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) createProduct(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in Product
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Active, in.Version = true, 0
	v, err := a.store.CreateProduct(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) getProduct(w http.ResponseWriter, r *http.Request, _ *Principal) {
	v, err := a.store.GetProduct(r.Context(), r.PathValue("sku"))
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) patchProduct(w http.ResponseWriter, r *http.Request, p *Principal) {
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in ProductPatch
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdateProduct(r.Context(), p, r.PathValue("sku"), im, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

// ---- invoices -------------------------------------------------------------------------

func (a *api) listInvoices(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, ok := cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := InvoiceFilter{Kind: q.Get("kind"), Status: q.Get("status"), Project: q.Get("project"), From: q.Get("from"), To: q.Get("to"), Overdue: q.Get("overdue_as_of")}
	f.PartyID, _ = strconv.ParseInt(q.Get("party_id"), 10, 64)
	for _, d := range []string{f.From, f.To, f.Overdue} {
		if d != "" && !validDate(d) {
			writeError(w, http.StatusBadRequest, "bad_request", "dates use YYYY-MM-DD")
			return
		}
	}
	items, next, err := a.store.ListInvoices(r.Context(), p, f, before, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.page(w, items, next)
}

func (a *api) createInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in NewInvoice
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.CreateInvoice(r.Context(), p, in, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/v1/invoices/"+strconv.FormatInt(v.ID, 10), v.Version, replayed, v)
}

// created writes a 201 (or a 200 replay of an idempotent create) with its Location and ETag.
func (a *api) created(w http.ResponseWriter, loc string, version int64, replayed bool, v any) {
	setETag(w, version)
	w.Header().Set("Location", loc)
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, v)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) respondInvoice(w http.ResponseWriter, v *Invoice, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, v.Version)
	writeJSON(w, http.StatusOK, v)
}

func (a *api) getInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetInvoice(r.Context(), p, id)
	a.respondInvoice(w, v, err)
}

func (a *api) patchInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in InvoicePatch
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.UpdateInvoice(r.Context(), p, id, im, in)
	a.respondInvoice(w, v, err)
}

func (a *api) deleteInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteDraft(r.Context(), p, id, im); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) approveInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	v, err := a.store.ApproveInvoice(r.Context(), p, id, im)
	a.respondInvoice(w, v, err)
}

// requestInvoiceApproval opens the can-core approval request of a draft invoice.
func (a *api) requestInvoiceApproval(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ap, err := a.store.RequestInvoiceApproval(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ap)
}

func (a *api) sendInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	v, err := a.store.SendInvoice(r.Context(), p, id, im)
	a.respondInvoice(w, v, err)
}

type reasonBody struct {
	Reason string `json:"reason"`
}

func (a *api) voidInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
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
	v, err := a.store.VoidInvoice(r.Context(), p, id, im, in.Reason)
	a.respondInvoice(w, v, err)
}

func (a *api) createCreditNote(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string      `json:"reason"`
		Lines  []LineInput `json:"lines"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.CreateCreditNote(r.Context(), p, id, in.Reason, in.Lines, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/v1/invoices/"+strconv.FormatInt(v.ID, 10), v.Version, replayed, v)
}

func (a *api) invoiceHTML(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetInvoice(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeInvoiceHTML(w, v)
}

func (a *api) writeInvoiceHTML(w http.ResponseWriter, v *Invoice) {
	if err := serveInvoiceHTML(w, v, a.seller); err != nil {
		a.fail(w, err)
	}
}

// serveInvoiceHTML writes the printable invoice. The API and the web UI share it, so there is one
// renderer, one escaping policy and one set of headers.
func serveInvoiceHTML(w http.ResponseWriter, v *Invoice, seller Seller) error {
	b, err := RenderInvoiceHTML(v, seller)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Inline styles only: no script, no images, no framing, no outgoing requests.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	_, _ = w.Write(b)
	return nil
}

func (a *api) issueLink(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		TTLHours int `json:"ttl_hours"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	tok, exp, err := a.store.IssueLink(r.Context(), p, a.linkSecret, id, time.Duration(in.TTLHours)*time.Hour)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"url": a.cfg.PublicBaseURL + "/public/invoices/" + tok, "expires_at": exp})
}

func (a *api) revokeLinks(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.RevokeLinks(r.Context(), p, id)
	a.respondInvoice(w, v, err)
}

// publicInvoice serves the signed-link view. Every failure is the same 404, so a link is
// not an oracle for invoice ids. Bad links count against the client address, never valid
// ones, so a valid link is never throttled.
func (a *api) publicInvoice(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.PublicInvoice(r.Context(), a.linkSecret, r.PathValue("token"))
	if err != nil {
		if err == ErrNotFound {
			if ok, wait := a.failures.allow(clientIP(r, a.trustProxy)); !ok {
				select {
				case <-time.After(a.penalty):
				case <-r.Context().Done():
				}
				tooMany(w, wait)
				return
			}
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		a.fail(w, err)
		return
	}
	a.writeInvoiceHTML(w, v)
}

func (a *api) checkout(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s, err := a.store.CreateCheckout(r.Context(), p, a.payments, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s)
}

func (a *api) listPayments(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	items, err := a.store.ListPayments(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) recordPayment(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in PaymentInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, replayed, err := a.store.RecordPayment(r.Context(), p, id, in, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, v)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) importPayments(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Payments []ImportPayment `json:"payments"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	res, replayed, err := a.store.ImportPayments(r.Context(), p, in.Payments, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"items": res})
}

// paymentWebhook receives a provider's "payment succeeded" notification. It authenticates
// by the provider's signature; a bad signature counts against the client address.
func (a *api) paymentWebhook(w http.ResponseWriter, r *http.Request) {
	if a.payments == nil {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		return
	}
	n, err := a.payments.ParseWebhook(r.Header, body)
	if err != nil {
		a.reject(w, r)
		return
	}
	pay, dup, err := a.store.RecordProviderPayment(r.Context(), a.payments, n)
	if err != nil {
		// A verified notification that cannot be applied (unknown or void invoice, overpayment, wrong
		// currency) will never succeed on retry. Answer 200 so the provider stops, and keep a record
		// for a person to reconcile instead of dropping it.
		var ie *InvalidError
		if errors.As(err, &ie) || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			if aerr := a.store.recordUnmatched(r.Context(), a.payments.Name(), n, err); aerr != nil {
				a.fail(w, aerr)
				return
			}
			a.log.Warn("provider payment could not be applied", "event_id", n.EventID, "invoice_id", n.InvoiceID)
			writeJSON(w, http.StatusOK, map[string]any{"status": "unmatched"})
			return
		}
		a.fail(w, err)
		return
	}
	status := "recorded"
	if dup {
		status = "duplicate"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "payment_id": pay.ID})
}

// ---- recurring, reports, exports ------------------------------------------------------

func (a *api) listRecurring(w http.ResponseWriter, r *http.Request, p *Principal) {
	items, err := a.store.ListRecurring(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) createRecurring(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewRecurring
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateRecurring(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) recurringStatus(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.SetRecurringStatus(r.Context(), p, id, in.Status)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) runRecurring(w http.ResponseWriter, r *http.Request, _ *Principal) {
	var in struct {
		AsOf string `json:"as_of"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	if in.AsOf == "" {
		in.AsOf = dateOf(a.now())
	}
	ids, err := a.store.RunRecurring(r.Context(), in.AsOf)
	if err != nil {
		a.fail(w, err)
		return
	}
	if ids == nil {
		ids = []int64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafted": ids})
}

func (a *api) aging(w http.ResponseWriter, r *http.Request, p *Principal) {
	asOf := r.URL.Query().Get("as_of")
	if asOf == "" {
		asOf = dateOf(a.now())
	}
	rep, err := a.store.Aging(r.Context(), p, asOf)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// csvWriter sets the CSV headers on its first write, so an export that fails before it
// produces anything (the audit write, a bad query) can still answer with a proper error.
type csvWriter struct {
	w     http.ResponseWriter
	name  string
	wrote bool
}

func (c *csvWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.wrote = true
		h := c.w.Header()
		h.Set("Content-Type", "text/csv; charset=utf-8")
		h.Set("Content-Disposition", `attachment; filename="`+c.name+`"`)
	}
	return c.w.Write(b)
}

func (a *api) finishExport(w http.ResponseWriter, c *csvWriter, name string, err error) {
	if err == nil {
		return
	}
	a.log.Error("export "+name, "error", err)
	if !c.wrote {
		a.fail(w, err)
	}
}

func (a *api) exportInvoices(w http.ResponseWriter, r *http.Request, p *Principal) {
	q := r.URL.Query()
	f := InvoiceFilter{Status: q.Get("status"), From: q.Get("from"), To: q.Get("to")}
	for _, d := range []string{f.From, f.To} {
		if d != "" && !validDate(d) {
			writeError(w, http.StatusBadRequest, "bad_request", "dates use YYYY-MM-DD")
			return
		}
	}
	c := &csvWriter{w: w, name: "invoices.csv"}
	a.finishExport(w, c, "invoices", a.store.ExportInvoicesCSV(r.Context(), p, f, c))
}

func (a *api) exportPayments(w http.ResponseWriter, r *http.Request, p *Principal) {
	q := r.URL.Query()
	for _, d := range []string{q.Get("from"), q.Get("to")} {
		if d != "" && !validDate(d) {
			writeError(w, http.StatusBadRequest, "bad_request", "dates use YYYY-MM-DD")
			return
		}
	}
	c := &csvWriter{w: w, name: "payments.csv"}
	a.finishExport(w, c, "payments", a.store.ExportPaymentsCSV(r.Context(), p, q.Get("from"), q.Get("to"), c))
}

func (a *api) exportExpenses(w http.ResponseWriter, r *http.Request, p *Principal) {
	c := &csvWriter{w: w, name: "expenses.csv"}
	a.finishExport(w, c, "expenses", a.store.ExportExpensesCSV(r.Context(), p, r.URL.Query().Get("status"), c))
}
