package finance

// UI of mailed documents and payment reminders (specs/062-invoice-email). Like every page here it
// only calls the Store.

import (
	"net/http"
	"strconv"
	"strings"
)

func (u *UI) invoicePDF(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	v, err := u.Store.GetInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	serveInvoicePDF(c.w, c.r, v, u.Seller)
	return nil
}

// mailFlash says what became of a message that was just handed over.
func mailFlash(m *DocumentMail) string {
	switch m.Status {
	case DMQueued, DMSent:
		return "flash.mail_queued"
	case DMNotAllowed:
		return "flash.mail_refused"
	case DMUnknown:
		return "flash.mail_unknown"
	}
	return "flash.mail_failed"
}

func (u *UI) invoiceEmail(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	m, err := u.Store.EmailInvoice(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), mailFlash(m))
}

func (u *UI) invoiceRemindersPost(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if err := u.Store.SetInvoiceReminders(c.ctx(), c.p, id, c.form("state")); err != nil {
		return err
	}
	return c.redirect("/invoices/"+itoa(id), "flash.saved")
}

func (u *UI) partyStatement(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	m, err := u.Store.EmailStatement(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	return c.redirect("/parties/"+itoa(id), mailFlash(m))
}

func (u *UI) partyRemindersPost(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if err := u.Store.SetPartyReminders(c.ctx(), c.p, id, c.form("state")); err != nil {
		return err
	}
	return c.redirect("/parties/"+itoa(id), "flash.saved")
}

// restoredOn is the date a restore switched reminders off, "" when there is nothing to say.
func restoredOn(p DunningPolicy) string {
	if p.RestoredAt == nil {
		return ""
	}
	return dateOf(*p.RestoredAt)
}

type dunningData struct {
	Restored string
	P        DunningPolicy
	Days     string
	CanEdit  bool
	Ready    bool
}

func (u *UI) dunning(c *uiCtx) error {
	p, err := u.Store.Dunning(c.ctx())
	if err != nil {
		return err
	}
	days := make([]string, len(p.DaysAfterDue))
	for i, d := range p.DaysAfterDue {
		days[i] = strconv.Itoa(d)
	}
	u.render(c.w, c.r, c, http.StatusOK, "reminders.html", "remind.title", dunningData{P: p, Restored: restoredOn(p), Days: strings.Join(days, ", "),
		CanEdit: capsOf(c.p).Admin && c.p.Unscoped(), Ready: u.Store.MailReady()})
	return nil
}

func (u *UI) dunningPost(c *uiCtx) error {
	in := DunningPolicy{Enabled: c.form("enabled") == "1"}
	for _, f := range strings.FieldsFunc(c.form("days"), func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(f)
		if err != nil {
			return invalid("days_after_due", "whole numbers separated by commas")
		}
		in.DaysAfterDue = append(in.DaysAfterDue, n)
	}
	if v := c.form("max_overdue_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return invalid("max_overdue_days", "a whole number of days")
		}
		in.MaxOverdueDays = n
	}
	if _, err := u.Store.SetDunning(c.ctx(), c.p, in); err != nil {
		return err
	}
	return c.redirect("/settings/reminders", "flash.saved")
}
