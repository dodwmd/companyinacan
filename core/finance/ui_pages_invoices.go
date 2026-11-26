package finance

import (
	"fmt"
	"github.com/dodwmd/companyinacan/core/uikit/session"
	"net/http"
	"slices"
	"strconv"
	"time"
)

var invoiceStatuses = []string{StDraft, StApproved, StSent, StPartiallyPaid, StPaid, StVoid}

// ---- list ------------------------------------------------------------------------------------

type invoicesData struct {
	Items    []Invoice
	Filter   InvoiceFilter
	Overdue  bool
	Pager    pager
	Statuses []string
	Names    map[int64]string
	Today    string
}

func (c *uiCtx) invoiceFilter() (InvoiceFilter, bool, error) {
	f := InvoiceFilter{Kind: c.q("kind"), Status: c.q("status"), Project: c.q("project")}
	if f.Kind != "" && f.Kind != KindInvoice && f.Kind != KindCreditNote {
		return f, false, invalid("kind", "invoice or credit_note")
	}
	if f.Status != "" && !slices.Contains(invoiceStatuses, f.Status) {
		return f, false, invalid("status", "not a status")
	}
	if f.Project != "" && !projectRE.MatchString(f.Project) {
		return f, false, invalid("project", "not a project key")
	}
	f.PartyID, _ = strconv.ParseInt(c.q("party"), 10, 64)
	var err error
	if f.From, err = c.queryDate("from"); err != nil {
		return f, false, err
	}
	if f.To, err = c.queryDate("to"); err != nil {
		return f, false, err
	}
	overdue := c.q("overdue") == "1"
	if overdue {
		f.Overdue = c.today()
	}
	return f, overdue, nil
}

func (u *UI) invoices(c *uiCtx) error {
	f, overdue, err := c.invoiceFilter()
	if err != nil {
		return err
	}
	before, err := c.cursor()
	if err != nil {
		return err
	}
	items, next, err := u.Store.ListInvoices(c.ctx(), c.p, f, before, uiPageSize)
	if err != nil {
		return err
	}
	f.Overdue = ""
	ids := make([]int64, 0, len(items))
	for _, v := range items {
		ids = append(ids, v.PartyID)
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, ids)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "invoices.html", "nav.invoices", invoicesData{Items: items, Names: names, Filter: f, Overdue: overdue,
		Pager: pager{nextLink("/invoices", c.r.URL.Query(), next)}, Statuses: invoiceStatuses, Today: c.today()})
	return nil
}

// ---- detail ----------------------------------------------------------------------------------

type invoiceData struct {
	V        Invoice
	Party    string
	Payments []Payment
	Version  int64

	CanEdit, CanRequest         bool
	CanApprove, ApproveDisabled bool
	CanSend, CanPay, CanCredit  bool
	CanVoid, CanLink            bool
	PayIdem                     string
}

func (u *UI) invoice(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	v, err := u.Store.GetInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	d := invoiceData{V: *v, Version: v.Version, PayIdem: session.NewToken()[:32]}
	if d.Payments, err = u.Store.ListPayments(c.ctx(), c.p, id); err != nil {
		return err
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, []int64{v.PartyID})
	if err != nil {
		return err
	}
	d.Party = names[v.PartyID]
	cp := capsOf(c.p)
	draft := v.Status == StDraft
	issued := !draft && v.Status != StVoid
	d.CanEdit = cp.Write && draft
	d.CanRequest = cp.Write && draft && u.Store.core != nil
	d.CanApprove = cp.Approve && draft
	d.ApproveDisabled = d.CanApprove && c.selfMade(v.CreatedBy, v.updatedBy)
	d.CanSend = cp.Write && v.Status == StApproved
	d.CanPay = cp.Write && v.Kind == KindInvoice && issued && v.BalanceMinor > 0
	d.CanCredit = cp.Write && v.Kind == KindInvoice && issued
	d.CanVoid = cp.Approve && v.Kind == KindInvoice && issued && v.PaidMinor == 0 && v.CreditedMinor == 0
	d.CanLink = cp.Write && !draft
	u.render(c.w, c.r, c, http.StatusOK, "invoice.html", "invoice.title", d)
	return nil
}

func (u *UI) invoiceHTML(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	v, err := u.Store.GetInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	return serveInvoiceHTML(c.w, v, u.Seller)
}

// ---- create / edit ---------------------------------------------------------------------------

type lineRow struct {
	Desc, Qty, Price, Disc, Tax, SKU string
}

type invoiceForm struct {
	Edit      bool
	ID        int64
	Version   int64
	Idem      string
	PartyID   int64
	Parties   []Party
	More      bool
	PQ        string
	Currency  string
	FXRate    string
	Series    string
	Project   string
	IssueDate string
	DueDate   string
	DueDays   string
	Notes     string
	Rows      []lineRow
	Tax       []TaxRate
	Base      string
	Today     string
}

const blankLineRows = 3

func blankRows(n int) []lineRow { return make([]lineRow, n) }

func rowsFromLines(ls []Line, cur string) []lineRow {
	out := make([]lineRow, 0, len(ls)+blankLineRows)
	for _, l := range ls {
		out = append(out, lineRow{Desc: l.Description, Qty: FormatMilli(l.QuantityMilli), Price: FormatMinor(l.UnitPriceMinor, cur),
			Disc: func() string {
				if l.DiscountBP == 0 {
					return ""
				}
				return FormatBP(l.DiscountBP)
			}(), Tax: l.TaxCode, SKU: l.SKU})
	}
	return append(out, blankRows(blankLineRows)...)
}

func (u *UI) invoiceNew(c *uiCtx) error {
	parties, more, pq, err := c.pickParties("")
	if err != nil {
		return err
	}
	rates, err := u.Store.ListTaxRates(c.ctx())
	if err != nil {
		return err
	}
	pid, _ := strconv.ParseInt(c.q("party"), 10, 64)
	cur := u.Cfg.BaseCurrency
	for _, p := range parties {
		if p.ID == pid && p.Currency != "" {
			cur = p.Currency
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "invoice_form.html", "invoice.new", invoiceForm{Idem: session.NewToken()[:32], PartyID: pid, Parties: parties, More: more, PQ: pq, Currency: cur, Series: "INV",
		Rows: blankRows(5), Tax: activeRates(rates), Base: u.Cfg.BaseCurrency, Today: c.today()})
	return nil
}

func activeRates(rs []TaxRate) []TaxRate {
	out := rs[:0:0]
	for _, r := range rs {
		if r.Active {
			out = append(out, r)
		}
	}
	return out
}

func (u *UI) invoiceCreate(c *uiCtx) error {
	idem, err := formIdem(c.r)
	if err != nil {
		return err
	}
	cur := stringsUpper(c.form("currency"))
	if _, ok := CurrencyExponent(cur); !ok {
		return invalid("currency", "unknown currency code")
	}
	lines, err := lineRows(c.r, cur)
	if err != nil {
		return err
	}
	pid, _ := strconv.ParseInt(c.form("party_id"), 10, 64)
	in := NewInvoice{PartyID: pid, Series: c.form("series"), Project: c.form("project"), Currency: cur, FXRate: c.form("fx_rate"),
		IssueDate: c.form("issue_date"), DueDate: c.form("due_date"), Notes: c.r.PostForm.Get("notes"), Lines: lines}
	if dd := c.form("due_days"); dd != "" && in.DueDate == "" {
		n, err := strconv.Atoi(dd)
		if err != nil {
			return invalid("due_days", "a whole number of days")
		}
		in.DueDays = &n
	}
	v, _, err := u.Store.CreateInvoice(c.ctx(), c.p, in, idem)
	if err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(v.ID), "flash.saved")
}

func (u *UI) invoiceEdit(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	v, err := u.Store.GetInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	if v.Status != StDraft {
		return notDraft(v) // issued documents have no edit form at all: void and credit instead
	}
	rates, err := u.Store.ListTaxRates(c.ctx())
	if err != nil {
		return err
	}
	fx := ""
	if v.Currency != v.BaseCurrency {
		fx = v.FXRate
	}
	u.render(c.w, c.r, c, http.StatusOK, "invoice_form.html", "invoice.edit", invoiceForm{Edit: true, ID: v.ID, Version: v.Version, PartyID: v.PartyID, Currency: v.Currency, FXRate: fx,
		Series: v.Series, Project: v.Project, IssueDate: v.IssueDate, DueDate: v.DueDate, Notes: v.Notes, Rows: rowsFromLines(v.Lines, v.Currency), Tax: activeRates(rates),
		Base: u.Cfg.BaseCurrency, Today: c.today()})
	return nil
}

func (u *UI) invoiceUpdate(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	cur := stringsUpper(c.form("currency"))
	if _, ok := CurrencyExponent(cur); !ok {
		return invalid("currency", "unknown currency code")
	}
	lines, err := lineRows(c.r, cur)
	if err != nil {
		return err
	}
	notes, project := c.r.PostForm.Get("notes"), c.form("project")
	patch := InvoicePatch{Project: &project, Currency: &cur, Notes: &notes, Lines: &lines}
	if v := c.form("fx_rate"); v != "" {
		patch.FXRate = &v
	}
	if v := c.form("issue_date"); v != "" {
		patch.IssueDate = &v
	}
	if v := c.form("due_date"); v != "" {
		patch.DueDate = &v
	}
	if _, err := u.Store.UpdateInvoice(c.ctx(), c.p, id, im, patch); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.saved")
}

func (u *UI) invoiceDelete(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if err := u.Store.DeleteDraft(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/invoices", "flash.deleted")
}

// ---- state changes ---------------------------------------------------------------------------

func (u *UI) invoiceRequestApproval(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if _, err := u.Store.RequestInvoiceApproval(c.ctx(), c.p, id); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.requested")
}

func (u *UI) invoiceApprove(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.ApproveInvoice(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.approved")
}

func (u *UI) invoiceSend(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.SendInvoice(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.sent")
}

func (u *UI) invoiceVoid(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.VoidInvoice(c.ctx(), c.p, id, im, c.form("reason")); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.voided")
}

type creditNoteData struct {
	V    Invoice
	Idem string
	Rows []lineRow
	Tax  []TaxRate
}

func (u *UI) creditNoteForm(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	v, err := u.Store.GetInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	if v.Kind != KindInvoice || v.Status == StDraft || v.Status == StVoid {
		return fmt.Errorf("%w: a credit note needs an issued, not void, invoice", ErrConflict)
	}
	rates, err := u.Store.ListTaxRates(c.ctx())
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "credit_note_form.html", "credit.title", creditNoteData{V: *v, Idem: session.NewToken()[:32], Rows: blankRows(3), Tax: activeRates(rates)})
	return nil
}

func (u *UI) creditNoteCreate(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	idem, err := formIdem(c.r)
	if err != nil {
		return err
	}
	v, err := u.Store.GetInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	lines, err := lineRows(c.r, v.Currency)
	if err != nil {
		return err
	}
	cn, _, err := u.Store.CreateCreditNote(c.ctx(), c.p, id, c.form("reason"), lines, idem)
	if err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(cn.ID), "flash.saved")
}

func (u *UI) paymentCreate(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	idem, err := formIdem(c.r)
	if err != nil {
		return err
	}
	v, err := u.Store.GetInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	amt, err := ParseMinor(c.form("amount"), v.Currency)
	if err != nil {
		return formProblem("amount", err)
	}
	in := PaymentInput{AmountMinor: amt, Currency: v.Currency, Method: c.form("method"), Reference: c.form("reference"), ReceivedOn: c.form("received_on")}
	if _, _, err := u.Store.RecordPayment(c.ctx(), c.p, id, in, idem); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.paid")
}

// ---- public links ----------------------------------------------------------------------------

type linkData struct {
	ID      int64
	URL     string
	Path    bool // no public_base_url configured: only the path is shown
	Expires time.Time
}

// linkCreate issues a signed link and shows it once, in the response to the POST. The token is not
// stored by the UI, not put in a redirect or a query string, not logged (the access log redacts
// /public/invoices/ paths and the UI log never records bodies) and not shown again.
func (u *UI) linkCreate(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	ttl := 0
	if v := c.form("ttl_hours"); v != "" {
		if ttl, err = strconv.Atoi(v); err != nil || ttl < 0 || ttl > int(maxLinkTTL.Hours()) {
			return invalid("ttl_hours", "a whole number of hours, at most %d", int(maxLinkTTL.Hours()))
		}
	}
	tok, exp, err := u.Store.IssueLink(c.ctx(), c.p, u.LinkSecret, id, time.Duration(ttl)*time.Hour)
	if err != nil {
		return err
	}
	d := linkData{ID: id, Expires: exp, URL: u.Cfg.PublicBaseURL + "/public/invoices/" + tok, Path: u.Cfg.PublicBaseURL == ""}
	u.render(c.w, c.r, c, http.StatusOK, "link_created.html", "link.title", d)
	return nil
}

func (u *UI) linkRevoke(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if _, err := u.Store.RevokeLinks(c.ctx(), c.p, id); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.revoked")
}

// ---- exports ---------------------------------------------------------------------------------

func (u *UI) csv(c *uiCtx, name string, run func(w *csvWriter) error) error {
	cw := &csvWriter{w: c.w, name: name}
	if err := run(cw); err != nil {
		if !cw.wrote {
			return err
		}
		u.Log.Error("export "+name, "error", err)
	}
	return nil
}

func (u *UI) exportInvoices(c *uiCtx) error {
	f := InvoiceFilter{Status: c.q("status")}
	if f.Status != "" && !slices.Contains(invoiceStatuses, f.Status) {
		return invalid("status", "not a status")
	}
	var err error
	if f.From, err = c.queryDate("from"); err != nil {
		return err
	}
	if f.To, err = c.queryDate("to"); err != nil {
		return err
	}
	return u.csv(c, "invoices.csv", func(w *csvWriter) error { return u.Store.ExportInvoicesCSV(c.ctx(), c.p, f, w) })
}

func (u *UI) exportPayments(c *uiCtx) error {
	from, err := c.queryDate("from")
	if err != nil {
		return err
	}
	to, err := c.queryDate("to")
	if err != nil {
		return err
	}
	return u.csv(c, "payments.csv", func(w *csvWriter) error { return u.Store.ExportPaymentsCSV(c.ctx(), c.p, from, to, w) })
}

func (u *UI) exportExpenses(c *uiCtx) error {
	st := c.q("status")
	if st != "" && !slices.Contains(expenseStatuses, st) {
		return invalid("status", "not a status")
	}
	return u.csv(c, "expenses.csv", func(w *csvWriter) error { return u.Store.ExportExpensesCSV(c.ctx(), c.p, st, w) })
}
