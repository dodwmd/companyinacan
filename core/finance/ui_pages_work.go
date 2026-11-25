package finance

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func stringsUpper(s string) string { return strings.ToUpper(s) }

// ---- recurring schedules ---------------------------------------------------------------------

type recurringData struct {
	Items []Recurring
	Names map[int64]string
	Write bool
}

func (u *UI) recurring(c *uiCtx) error {
	items, err := u.Store.ListRecurring(c.ctx(), c.p)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(items))
	for _, r := range items {
		ids = append(ids, r.PartyID)
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, ids)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "recurring.html", "nav.recurring", recurringData{Items: items, Names: names, Write: capsOf(c.p).Write})
	return nil
}

type recurringForm struct {
	Parties []Party
	Rows    []lineRow
	Tax     []TaxRate
	Base    string
	Today   string
}

func (u *UI) recurringNew(c *uiCtx) error {
	parties, _, err := u.Store.ListParties(c.ctx(), c.p, PartyFilter{Kind: "customer"}, 0, 100)
	if err != nil {
		return err
	}
	rates, err := u.Store.ListTaxRates(c.ctx())
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "recurring_form.html", "recurring.new", recurringForm{Parties: parties, Rows: blankRows(4), Tax: activeRates(rates), Base: u.Cfg.BaseCurrency, Today: c.today()})
	return nil
}

func (u *UI) recurringCreate(c *uiCtx) error {
	cur := stringsUpper(c.form("currency"))
	if _, ok := CurrencyExponent(cur); !ok {
		return invalid("currency", "unknown currency code")
	}
	lines, err := lineRows(c.r, cur)
	if err != nil {
		return err
	}
	pid, _ := strconv.ParseInt(c.form("party_id"), 10, 64)
	in := NewRecurring{PartyID: pid, Name: c.form("name"), Currency: cur, FXRate: c.form("fx_rate"), Project: c.form("project"), Series: c.form("series"),
		Lines: lines, Interval: c.form("interval"), Start: c.form("start_date"), Notes: c.r.PostForm.Get("notes")}
	if e := c.form("end_date"); e != "" {
		in.End = &e
	}
	if dd := c.form("due_days"); dd != "" {
		n, err := strconv.Atoi(dd)
		if err != nil {
			return invalid("due_days", "a whole number of days")
		}
		in.DueDays = &n
	}
	if _, err := u.Store.CreateRecurring(c.ctx(), c.p, in); err != nil {
		return err
	}
	return c.redirect("/recurring", "flash.saved")
}

func (u *UI) recurringStatus(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if _, err := u.Store.SetRecurringStatus(c.ctx(), c.p, id, c.form("status")); err != nil {
		return err
	}
	return c.redirect("/recurring", "flash.saved")
}

// ---- contracts -------------------------------------------------------------------------------

type contractsData struct {
	Items  []Contract
	Names  map[int64]string
	Status string
	Pager  pager
	Write  bool
}

var contractStatuses = []string{CtDraft, CtSent, CtSigned, CtActive, CtExpired, CtTerminated}

func (u *UI) contracts(c *uiCtx) error {
	before, err := c.cursor()
	if err != nil {
		return err
	}
	st := c.q("status")
	if st != "" && !slices.Contains(contractStatuses, st) {
		return invalid("status", "not a status")
	}
	items, next, err := u.Store.ListContracts(c.ctx(), c.p, st, 0, before, uiPageSize)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(items))
	for _, ct := range items {
		ids = append(ids, ct.PartyID)
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, ids)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "contracts.html", "nav.contracts", contractsData{Items: items, Names: names, Status: st, Pager: pager{nextLink("/contracts", c.r.URL.Query(), next)}, Write: capsOf(c.p).Write})
	return nil
}

type contractForm struct {
	Templates []ContractTemplate
	Tmpl      *ContractTemplate
	Parties   []Party
	PartyID   int64
}

func (u *UI) contractNew(c *uiCtx) error {
	tmpls, err := u.Store.ListTemplates(c.ctx())
	if err != nil {
		return err
	}
	parties, _, err := u.Store.ListParties(c.ctx(), c.p, PartyFilter{}, 0, 100)
	if err != nil {
		return err
	}
	d := contractForm{Templates: tmpls, Parties: parties}
	d.PartyID, _ = strconv.ParseInt(c.q("party"), 10, 64)
	if tid, _ := strconv.ParseInt(c.q("template"), 10, 64); tid > 0 {
		for i := range tmpls {
			if tmpls[i].ID == tid {
				d.Tmpl = &tmpls[i]
			}
		}
		if d.Tmpl == nil {
			return ErrNotFound
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "contract_form.html", "contract.new", d)
	return nil
}

func (u *UI) contractCreate(c *uiCtx) error {
	pid, _ := strconv.ParseInt(c.form("party_id"), 10, 64)
	tid, _ := strconv.ParseInt(c.form("template_id"), 10, 64)
	if tid < 1 {
		return invalid("template_id", "choose a template")
	}
	vars := map[string]string{}
	for k, v := range c.r.PostForm {
		if name, ok := strings.CutPrefix(k, "var_"); ok && len(v) > 0 {
			vars[name] = strings.TrimSpace(v[0])
		}
	}
	in := NewContract{PartyID: pid, Title: c.form("title"), TemplateID: &tid, Variables: vars}
	for k, dst := range map[string]**string{"start_date": &in.StartDate, "end_date": &in.EndDate, "renewal_date": &in.RenewalDate} {
		if v := c.form(k); v != "" {
			s := v
			*dst = &s
		}
	}
	if v := c.form("reminder_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return invalid("reminder_days", "a whole number of days")
		}
		in.ReminderDays = &n
	}
	ct, err := u.Store.CreateContract(c.ctx(), c.p, in)
	if err != nil {
		return err
	}
	return c.redirect("/contracts/"+itoa(ct.ID), "flash.saved")
}

type contractData struct {
	C        Contract
	Party    string
	Versions []int64
	Body     *ContractVersion
	Write    bool
	CanSend  bool
	CanEnd   bool
	CanDates bool
	CanSign  bool // an e-signature provider is configured
	Signed   bool
}

func (u *UI) contract(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	ct, err := u.Store.GetContract(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	body, err := u.Store.GetVersion(c.ctx(), c.p, id, ct.CurrentVersion)
	if err != nil {
		return err
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, []int64{ct.PartyID})
	if err != nil {
		return err
	}
	d := contractData{C: *ct, Party: names[ct.PartyID], Body: body, Write: capsOf(c.p).Write, CanSign: u.ESign != nil}
	for n := int64(1); n <= ct.CurrentVersion && n <= 200; n++ {
		d.Versions = append(d.Versions, n)
	}
	d.CanSend = d.Write && ct.Status == CtDraft
	d.CanEnd = d.Write && (ct.Status == CtSigned || ct.Status == CtActive)
	d.CanDates = d.Write && ct.Status != CtExpired && ct.Status != CtTerminated
	u.render(c.w, c.r, c, http.StatusOK, "contract.html", "contract.title", d)
	return nil
}

type versionData struct {
	C Contract
	V ContractVersion
}

func (u *UI) contractVersion(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	n, err := c.id("n")
	if err != nil {
		return err
	}
	ct, err := u.Store.GetContract(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	v, err := u.Store.GetVersion(c.ctx(), c.p, id, n)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "contract_version.html", "contract.version", versionData{C: *ct, V: *v})
	return nil
}

var emailSplitRE = regexp.MustCompile(`[\s,;]+`)

func (u *UI) contractSend(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	in := SendRequest{Manual: c.form("mode") != "esign"}
	if !in.Manual {
		for _, e := range emailSplitRE.Split(c.r.PostForm.Get("signers"), -1) {
			if e != "" {
				in.Signers = append(in.Signers, e)
			}
		}
	}
	var es ESigner
	if u.ESign != nil {
		es = u.ESign
	}
	if _, err := u.Store.SendContract(c.ctx(), c.p, es, id, im, in); err != nil {
		return err
	}
	return c.redirect("/contracts/"+itoa(id), "flash.sent")
}

func (u *UI) contractTerminate(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.TerminateContract(c.ctx(), c.p, id, im, c.form("reason")); err != nil {
		return err
	}
	return c.redirect("/contracts/"+itoa(id), "flash.saved")
}

func (u *UI) contractDates(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	opt := func(name string) Opt[string] { return Opt[string]{Set: true, V: c.form(name)} }
	in := ContractDates{StartDate: opt("start_date"), EndDate: opt("end_date"), RenewalDate: opt("renewal_date")}
	if v := c.form("reminder_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return invalid("reminder_days", "a whole number of days")
		}
		in.ReminderDays = &n
	}
	if _, err := u.Store.UpdateContractDates(c.ctx(), c.p, id, im, in); err != nil {
		return err
	}
	return c.redirect("/contracts/"+itoa(id), "flash.saved")
}

// ---- timesheets ------------------------------------------------------------------------------

type timesheetsData struct {
	Items    []Timesheet
	Pager    pager
	Status   string
	Monday   string
	OwnOnly  bool
	CanWrite bool
}

var timesheetStatuses = []string{TsDraft, TsSubmitted, TsApproved, TsRejected}

func mondayOf(t time.Time) string {
	t = t.UTC()
	off := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -off).Format(time.DateOnly)
}

func (u *UI) timesheets(c *uiCtx) error {
	before, err := c.cursor()
	if err != nil {
		return err
	}
	f := TimesheetFilter{Status: c.q("status")}
	if f.Status != "" && !slices.Contains(timesheetStatuses, f.Status) {
		return invalid("status", "not a status")
	}
	items, next, err := u.Store.ListTimesheets(c.ctx(), c.p, f, before, uiPageSize)
	if err != nil {
		return err
	}
	cp := capsOf(c.p)
	u.render(c.w, c.r, c, http.StatusOK, "timesheets.html", "nav.timesheets", timesheetsData{Items: items, Pager: pager{nextLink("/timesheets", c.r.URL.Query(), next)},
		Status: f.Status, Monday: mondayOf(u.Now()), OwnOnly: cp.OwnOnly, CanWrite: cp.Write})
	return nil
}

func (u *UI) timesheetOpen(c *uiCtx) error {
	t, _, err := u.Store.CreateTimesheet(c.ctx(), c.p, "", c.form("week_start"))
	if err != nil {
		return err
	}
	return c.redirect("/timesheets/"+itoa(t.ID), "")
}

type gridRow struct {
	Project, Task, Desc string
	Billable            bool
	Cells               [7]string
}

type timesheetData struct {
	T          Timesheet
	Days       [7]string
	Rows       []gridRow
	Locked     bool
	LockedThru string
	CanEdit    bool
	CanSubmit  bool
	CanDecide  bool
	Disabled   bool // decision buttons shown but disabled: the viewer is involved with the sheet
	Version    int64
}

func (u *UI) timesheet(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	t, err := u.Store.GetTimesheet(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	lock, err := u.Store.PeriodLock(c.ctx())
	if err != nil {
		return err
	}
	d := timesheetData{T: *t, Version: t.Version, Locked: lock != "" && t.WeekStart <= lock, LockedThru: lock}
	ws := parseDate(t.WeekStart)
	for i := range d.Days {
		d.Days[i] = ws.AddDate(0, 0, i).Format(time.DateOnly)
	}
	type key struct {
		p, t, d string
		b       bool
	}
	idx := map[key]int{}
	for _, e := range t.Entries {
		k := key{e.Project, e.Task, e.Description, e.Billable}
		i, ok := idx[k]
		if !ok {
			i = len(d.Rows)
			idx[k] = i
			d.Rows = append(d.Rows, gridRow{Project: e.Project, Task: e.Task, Desc: e.Description, Billable: e.Billable})
		}
		off := int(parseDate(e.Day).Sub(ws).Hours() / 24)
		if off >= 0 && off < 7 {
			prev, _ := ParseHM(d.Rows[i].Cells[off])
			d.Rows[i].Cells[off] = FormatHM(prev + e.Minutes)
		}
	}
	editable := (t.Status == TsDraft || t.Status == TsRejected) && canActOn(c.p, t.Owner)
	d.CanEdit = editable && !d.Locked
	d.CanSubmit = d.CanEdit && len(t.Entries) > 0
	if d.CanEdit {
		for i := 0; i < 3 && len(d.Rows) < 25; i++ {
			d.Rows = append(d.Rows, gridRow{Billable: true})
		}
	}
	d.CanDecide = capsOf(c.p).Approve && t.Status == TsSubmitted
	d.Disabled = d.CanDecide && c.selfMade(t.Owner, t.CreatedBy, t.updatedBy, t.submittedBy)
	u.render(c.w, c.r, c, http.StatusOK, "timesheet.html", "timesheet.title", d)
	return nil
}

func (u *UI) timesheetEntries(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	t, err := u.Store.GetTimesheet(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	f := c.r.PostForm
	get := func(k string, i int) string {
		if i < len(f[k]) {
			return strings.TrimSpace(f[k][i])
		}
		return ""
	}
	rows := len(f["project"])
	if rows > 40 {
		return invalid("entries", "too many rows")
	}
	ws := parseDate(t.WeekStart)
	var in []EntryInput
	var p problems
	for i := 0; i < rows; i++ {
		project, task, desc := strings.ToUpper(get("project", i)), strings.ToUpper(get("task", i)), get("desc", i)
		bill := get("billable", i) == "1"
		any := false
		for d := 0; d < 7; d++ {
			cell := get("h", i*7+d)
			if cell == "" {
				continue
			}
			m, err := ParseHM(cell)
			if err != nil {
				p.add(fmt.Sprintf("row %d day %d", i+1, d+1), "%s", err.Error())
				continue
			}
			if m == 0 {
				continue
			}
			any = true
			in = append(in, EntryInput{Day: ws.AddDate(0, 0, d).Format(time.DateOnly), Project: project, Task: task, Minutes: m, Billable: bill, Description: desc})
		}
		if !any && project == "" && task == "" && desc == "" {
			continue
		}
		if project == "" {
			p.add(fmt.Sprintf("row %d project", i+1), "required")
		}
	}
	if err := p.err(); err != nil {
		return err
	}
	if _, err := u.Store.SetEntries(c.ctx(), c.p, id, im, in); err != nil {
		return err
	}
	return c.redirect("/timesheets/"+itoa(id), "flash.saved")
}

func (u *UI) timesheetSubmit(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.SubmitTimesheet(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/timesheets/"+itoa(id), "flash.submitted")
}

func (u *UI) timesheetApprove(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.ApproveTimesheet(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/timesheets/"+itoa(id), "flash.approved")
}

func (u *UI) timesheetReject(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.RejectTimesheet(c.ctx(), c.p, id, im, c.form("reason")); err != nil {
		return err
	}
	return c.redirect("/timesheets/"+itoa(id), "flash.rejected")
}

// ---- expenses --------------------------------------------------------------------------------

var expenseStatuses = []string{ExDraft, ExSubmitted, ExApproved, ExRejected, ExReimbursed}

type expensesData struct {
	Items    []Expense
	Pager    pager
	Status   string
	Statuses []string
	CanWrite bool
	CanExp   bool
}

func (u *UI) expenses(c *uiCtx) error {
	before, err := c.cursor()
	if err != nil {
		return err
	}
	st := c.q("status")
	if st != "" && !slices.Contains(expenseStatuses, st) {
		return invalid("status", "not a status")
	}
	items, next, err := u.Store.ListExpenses(c.ctx(), c.p, st, "", before, uiPageSize)
	if err != nil {
		return err
	}
	cp := capsOf(c.p)
	u.render(c.w, c.r, c, http.StatusOK, "expenses.html", "nav.expenses", expensesData{Items: items, Pager: pager{nextLink("/expenses", c.r.URL.Query(), next)}, Status: st,
		Statuses: expenseStatuses, CanWrite: cp.Write, CanExp: cp.Read && !cp.OwnOnly})
	return nil
}

type expenseForm struct {
	Idem  string
	Base  string
	Today string
	Types []string
}

func (u *UI) expenseNew(c *uiCtx) error {
	u.render(c.w, c.r, c, http.StatusOK, "expense_form.html", "expense.new", expenseForm{Idem: newToken()[:32], Base: u.Cfg.BaseCurrency, Today: c.today(), Types: receiptTypes})
	return nil
}

func (u *UI) expenseCreate(c *uiCtx) error {
	idem, err := formIdem(c.r)
	if err != nil {
		return err
	}
	cur := stringsUpper(c.form("currency"))
	amt, err := ParseMinor(c.form("amount"), cur)
	if err != nil {
		return formProblem("amount", err)
	}
	in := ExpenseInput{Description: c.form("description"), AmountMinor: amt, Currency: cur, Category: c.form("category"), IncurredOn: c.form("incurred_on"), Project: strings.ToUpper(c.form("project"))}
	if fn := c.form("receipt_filename"); fn != "" || c.form("receipt_sha256") != "" || c.form("receipt_size") != "" {
		size, err := strconv.ParseInt(c.form("receipt_size"), 10, 64)
		if err != nil {
			return invalid("receipt_size", "a whole number of bytes")
		}
		in.Receipt = &Receipt{Filename: fn, ContentType: c.form("receipt_type"), SizeBytes: size, SHA256: strings.ToLower(c.form("receipt_sha256"))}
	}
	e, _, err := u.Store.CreateExpense(c.ctx(), c.p, in, idem)
	if err != nil {
		return err
	}
	return c.redirect("/expenses/"+itoa(e.ID), "flash.saved")
}

type expenseData struct {
	E                         Expense
	CanSubmit                 bool
	CanDecide, DecideDisabled bool
	CanReimburse              bool
	Version                   int64
}

func (u *UI) expense(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	e, err := u.Store.GetExpense(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	cp := capsOf(c.p)
	d := expenseData{E: *e, Version: e.Version}
	d.CanSubmit = cp.Write && e.Status == ExDraft && sameSubject(e.Owner, c.p.Subject)
	d.CanDecide = cp.Approve && e.Status == ExSubmitted
	d.DecideDisabled = d.CanDecide && c.selfMade(e.Owner)
	d.CanReimburse = cp.Write && !cp.OwnOnly && e.Status == ExApproved && !sameSubject(e.Owner, c.p.Subject)
	u.render(c.w, c.r, c, http.StatusOK, "expense.html", "expense.title", d)
	return nil
}

func (u *UI) expenseSubmit(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.SubmitExpense(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/expenses/"+itoa(id), "flash.submitted")
}

func (u *UI) expenseApprove(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.ApproveExpense(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/expenses/"+itoa(id), "flash.approved")
}

func (u *UI) expenseReject(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.RejectExpense(c.ctx(), c.p, id, im, c.form("reason")); err != nil {
		return err
	}
	return c.redirect("/expenses/"+itoa(id), "flash.rejected")
}

func (u *UI) expenseReimburse(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.ReimburseExpense(c.ctx(), c.p, id, im, c.form("reference")); err != nil {
		return err
	}
	return c.redirect("/expenses/"+itoa(id), "flash.saved")
}

// ---- purchase orders -------------------------------------------------------------------------

var poStatuses = []string{PoDraft, PoApproved, PoReceived, PoCancelled}

type posData struct {
	Items  []PurchaseOrder
	Names  map[int64]string
	Status string
	Pager  pager
	Write  bool
}

func (u *UI) purchaseOrders(c *uiCtx) error {
	before, err := c.cursor()
	if err != nil {
		return err
	}
	st := c.q("status")
	if st != "" && !slices.Contains(poStatuses, st) {
		return invalid("status", "not a status")
	}
	items, next, err := u.Store.ListPurchaseOrders(c.ctx(), c.p, st, before, uiPageSize)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(items))
	for _, o := range items {
		ids = append(ids, o.VendorID)
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, ids)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "pos.html", "nav.pos", posData{Items: items, Names: names, Status: st, Pager: pager{nextLink("/purchase-orders", c.r.URL.Query(), next)}, Write: capsOf(c.p).Write})
	return nil
}

type poForm struct {
	Idem    string
	Vendors []Party
	Rows    []lineRow
	Tax     []TaxRate
	Base    string
}

func (u *UI) poNew(c *uiCtx) error {
	vendors, _, err := u.Store.ListParties(c.ctx(), c.p, PartyFilter{Kind: "vendor"}, 0, 100)
	if err != nil {
		return err
	}
	rates, err := u.Store.ListTaxRates(c.ctx())
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "po_form.html", "po.new", poForm{Idem: newToken()[:32], Vendors: vendors, Rows: blankRows(4), Tax: activeRates(rates), Base: u.Cfg.BaseCurrency})
	return nil
}

func (u *UI) poCreate(c *uiCtx) error {
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
	vid, _ := strconv.ParseInt(c.form("vendor_id"), 10, 64)
	po, _, err := u.Store.CreatePurchaseOrder(c.ctx(), c.p, NewPurchaseOrder{VendorID: vid, Currency: cur, Project: strings.ToUpper(c.form("project")), Notes: c.r.PostForm.Get("notes"), Lines: lines}, idem)
	if err != nil {
		return err
	}
	return c.redirect("/purchase-orders/"+itoa(po.ID), "flash.saved")
}

type poData struct {
	O                        PurchaseOrder
	Vendor                   string
	CanApprove, ApproveBlock bool
	CanReceive, CanCancel    bool
	Version                  int64
}

func (u *UI) purchaseOrder(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	o, err := u.Store.GetPurchaseOrder(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	names, err := u.Store.PartyNames(c.ctx(), c.p, []int64{o.VendorID})
	if err != nil {
		return err
	}
	cp := capsOf(c.p)
	d := poData{O: *o, Vendor: names[o.VendorID], Version: o.Version}
	d.CanApprove = cp.Approve && o.Status == PoDraft
	d.ApproveBlock = d.CanApprove && c.selfMade(o.CreatedBy, o.updatedBy)
	d.CanReceive = cp.Write && o.Status == PoApproved
	d.CanCancel = cp.Write && (o.Status == PoDraft || o.Status == PoApproved)
	u.render(c.w, c.r, c, http.StatusOK, "po.html", "po.title", d)
	return nil
}

func (u *UI) poApprove(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.ApprovePurchaseOrder(c.ctx(), c.p, id, im); err != nil {
		return err
	}
	return c.redirect("/purchase-orders/"+itoa(id), "flash.approved")
}

func (u *UI) poStatus(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	if _, err := u.Store.SetPurchaseOrderStatus(c.ctx(), c.p, id, im, c.form("status")); err != nil {
		return err
	}
	return c.redirect("/purchase-orders/"+itoa(id), "flash.saved")
}
