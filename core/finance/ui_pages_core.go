package finance

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// ---- shared helpers --------------------------------------------------------------------------

const uiPageSize = 50

// pager is the "older" link of a cursor-paged list: the filters stay, the cursor moves.
type pager struct {
	Next string // URL of the next page, "" when there is none
}

func nextLink(path string, q url.Values, next int64) string {
	if next <= 0 {
		return ""
	}
	v := url.Values{}
	for k, vs := range q {
		if k != "cursor" && k != "ok" {
			v[k] = vs
		}
	}
	v.Set("cursor", encodeCursor(next))
	return path + "?" + v.Encode()
}

func (c *uiCtx) cursor() (int64, error) {
	n, ok := decodeCursor(c.r.URL.Query().Get("cursor"))
	if !ok {
		return 0, invalid("cursor", "the page link is not valid")
	}
	return n, nil
}

func (c *uiCtx) today() string { return dateOf(c.u.Now()) }

func (c *uiCtx) q(name string) string { return strings.TrimSpace(c.r.URL.Query().Get(name)) }

func (c *uiCtx) form(name string) string { return strings.TrimSpace(c.r.PostForm.Get(name)) }

// queryDate reads an optional date from the query string.
func (c *uiCtx) queryDate(name string) (string, error) {
	v := c.q(name)
	if v != "" && !validDate(v) {
		return "", invalid(name, "use YYYY-MM-DD")
	}
	return v, nil
}

// selfMade reports whether the signed-in subject made or last edited a record: the approval
// buttons are then shown disabled with the reason. This is only a hint for the page; the Store's
// authorizeApproval makes the decision again when the form is posted.
func (c *uiCtx) selfMade(who ...string) bool {
	for _, w := range who {
		if w != "" && sameSubject(w, c.p.Subject) {
			return true
		}
	}
	return false
}

// pickParties lists up to 100 parties in scope for a select box, filtered by the search box (query
// pq). More says the list was cut, so the page can ask for a narrower search instead of silently
// hiding parties.
func (c *uiCtx) pickParties(kind string) (parties []Party, more bool, q string, err error) {
	q = c.q("pq")
	if len([]rune(q)) > 100 {
		return nil, false, "", invalid("pq", "at most 100 characters")
	}
	parties, next, err := c.u.Store.ListParties(c.ctx(), c.p, PartyFilter{Kind: kind, Search: q}, 0, 100)
	return parties, next > 0, q, err
}

// ---- approvals queue -------------------------------------------------------------------------

type approvalQueue struct {
	Invoices    []Invoice
	Timesheets  []Timesheet
	Expenses    []Expense
	Orders      []PurchaseOrder
	More        bool
	Total       int
	OwnerHidden bool
	Engine      bool // timesheets, expenses and purchase orders are decided in can-core's inbox
}

// approvalQueue lists what waits for a decision and that the caller could decide: records the
// caller made, edited or owns are left out (the Store would refuse them), so the list is the
// caller's actual work. Each list is bounded.
func (c *uiCtx) approvalQueue() (*approvalQueue, error) {
	st, ctx, p := c.u.Store, c.ctx(), c.p
	q := &approvalQueue{Engine: st.pump != nil}
	invs, next, err := st.ListInvoices(ctx, p, InvoiceFilter{Status: StDraft}, 0, uiPageSize)
	if err != nil {
		return nil, err
	}
	q.More = q.More || next > 0
	for _, v := range invs {
		if !c.selfMade(v.CreatedBy, v.updatedBy) {
			q.Invoices = append(q.Invoices, v)
		}
	}
	tss, next, err := st.ListTimesheets(ctx, p, TimesheetFilter{Status: TsSubmitted}, 0, uiPageSize)
	if err != nil {
		return nil, err
	}
	q.More = q.More || next > 0
	for _, t := range tss {
		if !c.selfMade(t.Owner, t.CreatedBy, t.updatedBy, t.submittedBy) {
			q.Timesheets = append(q.Timesheets, t)
		}
	}
	exs, next, err := st.ListExpenses(ctx, p, ExSubmitted, "", 0, uiPageSize)
	if err != nil {
		return nil, err
	}
	q.More = q.More || next > 0
	for _, e := range exs {
		if !c.selfMade(e.Owner) {
			q.Expenses = append(q.Expenses, e)
		}
	}
	pos, next, err := st.ListPurchaseOrders(ctx, p, PoDraft, 0, uiPageSize)
	if err != nil {
		return nil, err
	}
	q.More = q.More || next > 0
	for _, o := range pos {
		if !c.selfMade(o.CreatedBy, o.updatedBy) {
			q.Orders = append(q.Orders, o)
		}
	}
	q.Total = len(q.Invoices) + len(q.Timesheets) + len(q.Expenses) + len(q.Orders)
	return q, nil
}

func (u *UI) approvals(c *uiCtx) error {
	q, err := c.approvalQueue()
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "approvals.html", "nav.approvals", q)
	return nil
}

// ---- home ------------------------------------------------------------------------------------

type homeData struct {
	RemindersRestored string
	Stats             *DashboardStats
	Queue             *approvalQueue
	Business          bool // false for an own-only (employee) token: links to its own sheets and claims only
	Today             string
	CanReport         bool
}

func (u *UI) home(c *uiCtx) error {
	d := homeData{Today: c.today()}
	cp := capsOf(c.p)
	if cp.Admin {
		p, err := u.Store.Dunning(c.ctx())
		if err != nil {
			return err
		}
		d.RemindersRestored = restoredOn(p)
	}
	d.Business = cp.Read && !cp.OwnOnly
	if d.Business {
		var err error
		if d.Stats, err = u.Store.Dashboard(c.ctx(), c.p, d.Today); err != nil {
			return err
		}
		if cp.Approve {
			if d.Queue, err = c.approvalQueue(); err != nil {
				return err
			}
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "home.html", "nav.home", d)
	return nil
}

// ---- parties ---------------------------------------------------------------------------------

type partiesData struct {
	Items []Party
	Q     string
	Kind  string
	Pager pager
}

func (u *UI) parties(c *uiCtx) error {
	after, err := c.cursor()
	if err != nil {
		return err
	}
	f := PartyFilter{Kind: c.q("kind"), Search: c.q("q")}
	if f.Kind != "" && f.Kind != "customer" && f.Kind != "vendor" {
		f.Kind = ""
	}
	if len([]rune(f.Search)) > 100 {
		return invalid("q", "at most 100 characters")
	}
	items, next, err := u.Store.ListParties(c.ctx(), c.p, f, after, uiPageSize)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "parties.html", "nav.parties", partiesData{Items: items, Q: f.Search, Kind: f.Kind, Pager: pager{nextLink("/parties", c.r.URL.Query(), next)}})
	return nil
}

type partyForm struct {
	P        Party
	Version  int64
	New      bool
	Base     string
	Currency string
}

func (u *UI) partyNew(c *uiCtx) error {
	u.render(c.w, c.r, c, http.StatusOK, "party_form.html", "party.new", partyForm{New: true, Base: u.Cfg.BaseCurrency, P: Party{Kind: "customer", Currency: u.Cfg.BaseCurrency}})
	return nil
}

func partyFromForm(c *uiCtx) NewParty {
	return NewParty{Kind: c.form("kind"), Name: c.form("name"), ContactName: c.form("contact_name"), Email: c.form("email"), Phone: c.form("phone"),
		Address: strings.TrimSpace(c.r.PostForm.Get("address")), TaxID: c.form("tax_id"), Currency: strings.ToUpper(c.form("currency")), Notes: strings.TrimSpace(c.r.PostForm.Get("notes"))}
}

func (u *UI) partyCreate(c *uiCtx) error {
	v, err := u.Store.CreateParty(c.ctx(), c.p, partyFromForm(c))
	if err != nil {
		return err
	}
	return c.redirect("/parties/"+itoa(v.ID), "flash.saved")
}

func (u *UI) partyEdit(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	v, err := u.Store.GetParty(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	if v.Erased {
		return invalid("party", "an erased party cannot be edited")
	}
	u.render(c.w, c.r, c, http.StatusOK, "party_form.html", "party.edit", partyForm{P: *v, Version: v.Version, Base: u.Cfg.BaseCurrency})
	return nil
}

func (u *UI) partyUpdate(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	in := partyFromForm(c)
	_, err = u.Store.UpdateParty(c.ctx(), c.p, id, im, PartyPatch{Kind: &in.Kind, Name: &in.Name, ContactName: &in.ContactName, Email: &in.Email, Phone: &in.Phone,
		Address: &in.Address, TaxID: &in.TaxID, Currency: &in.Currency, Notes: &in.Notes})
	if err != nil {
		return err
	}
	return c.redirect("/parties/"+itoa(id), "flash.saved")
}

type partyData struct {
	P        Party
	Invoices []Invoice
	More     bool
	CanEdit  bool
	CanErase bool

	CanRemind, CanStatement, MailReady bool
	Mails                              []DocumentMail
	Reminders                          *ReminderState
}

func (u *UI) party(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	p, err := u.Store.GetParty(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	invs, next, err := u.Store.ListInvoices(c.ctx(), c.p, InvoiceFilter{PartyID: id}, 0, 10)
	if err != nil {
		return err
	}
	cp := capsOf(c.p)
	d := partyData{P: *p, Invoices: invs, More: next > 0, CanEdit: cp.Write && !p.Erased, CanErase: cp.Admin, MailReady: u.Store.MailReady()}
	// A token limited to some projects sees only part of a party, so it gets none of this.
	switch d.Mails, d.Reminders, err = u.Store.ListPartyMails(c.ctx(), c.p, id); {
	case errors.Is(err, ErrForbidden):
	case err != nil:
		return err
	default:
		d.CanRemind = cp.Write && !p.Erased
		d.CanStatement = d.CanRemind && p.Email != ""
	}
	u.render(c.w, c.r, c, http.StatusOK, "party.html", "party.title", d)
	return nil
}

type eraseForm struct {
	// SSO is a launch-ticket session: no token field; a recent admin sign-in is the step-up.
	SSO            bool
	P              Party
	RetentionYears int
}

func (u *UI) partyEraseForm(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	p, err := u.Store.GetParty(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "party_erase.html", "erase.title", eraseForm{P: *p, RetentionYears: u.Cfg.RetentionYears, SSO: c.ss.SSO()})
	return nil
}

type erasedData struct {
	Report         *ErasureReport
	Warning        bool
	RetentionYears int
}

func (u *UI) partyErase(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if c.form("confirm") != "ERASE" {
		return invalid("confirm", "type ERASE to confirm")
	}
	// Step-up: erasing is irreversible, so a session left open on an unlocked screen is not enough.
	// A token session is asked for its sign-in token again, which can-core (or the token store) must
	// accept now, not from a cache: it may have been revoked since. A launch-ticket session needs a recent admin sign-in, confirmed by can-core (stepUpSSO).
	if c.ss.SSO() {
		if err := u.stepUpSSO(c); err != nil {
			return err
		}
	} else {
		p, err := u.sessions.StepUp(c.r, c.ss, strings.TrimSpace(c.r.PostForm.Get("token")), func(p Principal) bool { return allowedBy(&p, PermAdmin, false) })
		switch {
		case errors.Is(err, session.ErrStepUpReenter):
			return invalid("token", "re-enter your sign-in token to confirm the erasure")
		case err != nil:
			return invalid("token", "your sign-in token could not be confirmed as still allowed to erase; sign in again")
		}
		c.p = &p
	}
	rep, err := u.Store.ErasePartyData(c.ctx(), c.p, id, u.Cfg.RetentionYears)
	warn := false
	if err != nil {
		if rep == nil {
			return err
		}
		// erased, but the write-ahead log could not be truncated: say so, with the report
		u.Log.Error("erasure follow-up failed", "error", err)
		warn = true
	}
	u.render(c.w, c.r, c, http.StatusOK, "party_erased.html", "erase.done_title", erasedData{Report: rep, Warning: warn, RetentionYears: u.Cfg.RetentionYears})
	return nil
}

// ---- reports ---------------------------------------------------------------------------------

type agingRow struct {
	PartyID int64
	Name    string
	AgingBuckets
}

type agingCurrency struct {
	Currency string
	Totals   AgingBuckets
	Rows     []agingRow
	Hidden   int
}

type agingData struct {
	AsOf       string
	Currencies []agingCurrency
}

const agingMaxRows = 200

func (u *UI) aging(c *uiCtx) error {
	asOf := c.q("as_of")
	if asOf == "" {
		asOf = c.today()
	}
	rep, err := u.Store.Aging(c.ctx(), c.p, asOf)
	if err != nil {
		return err
	}
	d := agingData{AsOf: asOf}
	var ids []int64
	for _, cu := range rep.Currencies {
		for i, pr := range cu.Parties {
			if i < agingMaxRows {
				ids = append(ids, pr.PartyID)
			}
		}
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, ids)
	if err != nil {
		return err
	}
	for _, cu := range rep.Currencies {
		ac := agingCurrency{Currency: cu.Currency, Totals: cu.Totals}
		for i, pr := range cu.Parties {
			if i >= agingMaxRows {
				ac.Hidden = len(cu.Parties) - agingMaxRows
				break
			}
			ac.Rows = append(ac.Rows, agingRow{PartyID: pr.PartyID, Name: names[pr.PartyID], AgingBuckets: pr.AgingBuckets})
		}
		d.Currencies = append(d.Currencies, ac)
	}
	u.render(c.w, c.r, c, http.StatusOK, "aging.html", "report.aging", d)
	return nil
}

type statementRow struct {
	Inv      Invoice
	Payments []Payment
}

type statementData struct {
	P       Party
	Rows    []statementRow
	More    bool
	Owing   []CurrencyAmount
	Parties []Party
	PMore   bool
	PQ      string
}

const statementMax = 100

func (u *UI) statement(c *uiCtx) error {
	pid, _ := strconv.ParseInt(c.q("party"), 10, 64)
	d := statementData{}
	parties, more, pq, err := c.pickParties("")
	if err != nil {
		return err
	}
	d.Parties, d.PMore, d.PQ = parties, more, pq
	if pid > 0 {
		p, err := u.Store.GetParty(c.ctx(), c.p, pid)
		if err != nil {
			return err
		}
		d.P = *p
		invs, next, err := u.Store.ListInvoices(c.ctx(), c.p, InvoiceFilter{PartyID: pid}, 0, statementMax)
		if err != nil {
			return err
		}
		d.More = next > 0
		owing := map[string]int64{}
		for _, v := range invs {
			if v.Status == StDraft {
				continue
			}
			row := statementRow{Inv: v}
			if v.PaidMinor > 0 {
				if row.Payments, err = u.Store.ListPayments(c.ctx(), c.p, v.ID); err != nil {
					return err
				}
			}
			d.Rows = append(d.Rows, row)
			if v.Kind == KindInvoice && v.Status != StVoid {
				owing[v.Currency] += v.BalanceMinor
			}
		}
		d.Owing = sumByCurrency(owing)
	}
	u.render(c.w, c.r, c, http.StatusOK, "statement.html", "report.statement", d)
	return nil
}

// ---- settings --------------------------------------------------------------------------------

type settingsData struct {
	ShowSeries  bool
	Series      []SeriesInfo
	TaxRates    []TaxRate
	Base        string
	Seller      Seller
	PayInstr    string
	RetentionYr int
	AuditDays   int
	LockedUntil string
	Public      string
}

func (u *UI) settings(c *uiCtx) error {
	// The numbering counters are global: how many invoices the whole company has issued. A token limited
	// to some parties or projects must not learn that, so only an unscoped token sees them.
	var series []SeriesInfo
	showSeries := c.p.Unscoped()
	if showSeries {
		var err error
		if series, err = u.Store.ListSeries(c.ctx()); err != nil {
			return err
		}
	}
	rates, err := u.Store.ListTaxRates(c.ctx())
	if err != nil {
		return err
	}
	lock, err := u.Store.PeriodLock(c.ctx())
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "settings.html", "nav.settings", settingsData{Series: series, ShowSeries: showSeries, TaxRates: rates, Base: u.Cfg.BaseCurrency, Seller: u.Seller,
		PayInstr: u.Cfg.PaymentInstructions, RetentionYr: u.Cfg.RetentionYears, AuditDays: u.Cfg.AuditRetentionDays, LockedUntil: lock, Public: u.Cfg.PublicBaseURL})
	return nil
}
