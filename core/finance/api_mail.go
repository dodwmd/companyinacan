package finance

// API of mailed documents and payment reminders (specs/062-invoice-email).

import (
	"net/http"
	"time"
)

// pdfSlots bounds the PDFs being laid out at once: the public link is not rate limited for valid
// requests, and the module has little memory.
var pdfSlots = make(chan struct{}, 2)

const pdfWait = 5 * time.Second

func serveInvoicePDF(w http.ResponseWriter, r *http.Request, v *Invoice, seller Seller) {
	wait := time.NewTimer(pdfWait)
	defer wait.Stop()
	select {
	case pdfSlots <- struct{}{}:
	case <-r.Context().Done():
		return
	case <-wait.C:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "busy", "too many PDFs are being made; try again shortly")
		return
	}
	// Held only while laying out: a slow reader of the answer keeps nobody waiting.
	b := func() []byte {
		defer func() { <-pdfSlots }()
		return RenderInvoicePDF(v, seller)
	}()
	h := w.Header()
	h.Set("Content-Type", "application/pdf")
	h.Set("Content-Disposition", `attachment; filename="`+pdfFilename(v)+`"`)
	h.Set("X-Robots-Tag", "noindex, nofollow")
	_, _ = w.Write(b)
}

func (a *api) invoicePDF(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := a.store.GetInvoice(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	serveInvoicePDF(w, r, v, a.seller)
}

func (a *api) emailInvoice(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	m, err := a.store.EmailInvoice(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if v, err := a.store.GetInvoice(r.Context(), p, id); err == nil {
		m.PDFIncomplete = pdfIncomplete(v, a.seller)
	}
	writeJSON(w, http.StatusCreated, m)
}

func (a *api) emailStatement(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	m, err := a.store.EmailStatement(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (a *api) invoiceEmails(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	items, st, err := a.store.ListInvoiceMails(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.store.GetInvoice(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	// Here so that a client can warn before it emails the document.
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "reminders": st, "pdf_incomplete": pdfIncomplete(v, a.seller)})
}

func (a *api) partyEmails(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	items, st, err := a.store.ListPartyMails(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "reminders": st})
}

type reminderBody struct {
	State string `json:"state"`
}

func (a *api) invoiceReminders(w http.ResponseWriter, r *http.Request, p *Principal) {
	a.setReminders(w, r, func(id int64, state string) error { return a.store.SetInvoiceReminders(r.Context(), p, id, state) })
}

func (a *api) partyReminders(w http.ResponseWriter, r *http.Request, p *Principal) {
	a.setReminders(w, r, func(id int64, state string) error { return a.store.SetPartyReminders(r.Context(), p, id, state) })
}

func (a *api) setReminders(w http.ResponseWriter, r *http.Request, set func(int64, string) error) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in reminderBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := set(id, in.State); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (a *api) getDunning(w http.ResponseWriter, r *http.Request, _ *Principal) {
	pol, err := a.store.Dunning(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (a *api) putDunning(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in DunningPolicy
	if !decodeJSON(w, r, &in) {
		return
	}
	pol, err := a.store.SetDunning(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

// runDunning sends the due reminders now (the hourly loop does it on its own). It takes no date:
// a reminder is only ever sent for what is overdue today.
func (a *api) runDunning(w http.ResponseWriter, r *http.Request, _ *Principal) {
	n, err := a.store.RunDunning(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": n})
}
