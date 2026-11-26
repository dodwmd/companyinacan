package finance

import "fmt"

// uiMessagesEN is the message catalogue of the web UI: every user-visible string of the templates
// lives here, addressed by a dotted key, so another language is one more map. Values may contain
// fmt verbs; templates call them as {{t "key" args...}}. A test checks that every key a template or
// the Go code uses exists, and that templates hold no literal text.
var uiMessagesEN = map[string]string{
	"app.name":   "Company in a Can: Finance",
	"app.footer": "Company in a Can finance. Apache-2.0. A tool, not accounting, tax or legal advice.",
	"a11y.skip":  "Skip to main content",

	"nav.label":         "Main",
	"nav.home":          "Home",
	"nav.approvals":     "Approvals",
	"nav.parties":       "Customers and vendors",
	"nav.invoices":      "Invoices",
	"nav.recurring":     "Recurring",
	"nav.contracts":     "Contracts",
	"nav.pos":           "Purchase orders",
	"nav.timesheets":    "Timesheets",
	"nav.expenses":      "Expenses",
	"nav.reports":       "Reports",
	"nav.settings":      "Settings",
	"nav.notifications": "Notifications",
	"nav.logout":        "Log out",

	"common.save":     "Save",
	"common.edit":     "Edit",
	"common.cancel":   "Cancel",
	"common.filter":   "Filter",
	"common.show":     "Show",
	"common.all":      "Show all",
	"common.older":    "Older",
	"common.actions":  "Actions",
	"common.approve":  "Approve",
	"common.reject":   "Reject",
	"common.yes":      "Yes",
	"common.no":       "No",
	"common.active":   "Active",
	"common.inactive": "Inactive",

	"login.title":   "Sign in",
	"login.token":   "Personal access token",
	"login.hint":    "Create a personal access token in the admin interface and paste it here. It is checked once and kept only in memory on the server; your browser keeps a session cookie, never the token.",
	"login.submit":  "Sign in",
	"login.invalid": "That token was not accepted.",
	"login.locked":  "Too many failed attempts. Try again in a few seconds.",
	"login.csrf":    "The sign-in form expired. Try again.",

	"login.sso_only":        "Sign in from the Apps page of the admin UI. Pasting an access token is switched off here.",
	"login.token_off":       "Pasting an access token is switched off. Open finance from the Apps page of the admin UI.",
	"login.ticket_invalid":  "That sign-in link was not accepted. A link works once, for about a minute, and only in the browser that opened it from the admin UI. Open finance again from the Apps page.",
	"login.sso_unavailable": "Single sign-on is not available right now. Try again in a moment.",
	"ticket.title":          "Continue with single sign-on",
	"ticket.intro":          "You came here from the admin UI. Check the account, then press Continue. Nothing happens until you do. If it is not your account, close this page.",
	"ticket.submit":         "Continue",
	"ticket.account":        "You are about to sign in as",
	"ticket.other":          "This browser is already signed in as another account:",
	"ticket.replace":        "Sign that account out here and sign in as the one above",

	"flash.saved":     "Saved.",
	"flash.deleted":   "The draft was deleted.",
	"flash.approved":  "Approved.",
	"flash.rejected":  "Rejected.",
	"flash.withdrawn": "The signing request was withdrawn: its links no longer work.", "flash.invited": "Request saved. Check the invitation status below.", "flash.sent": "Marked as sent.",
	"flash.paid":      "Payment recorded.",
	"flash.voided":    "The invoice was voided. Its number stays used.",
	"flash.revoked":   "Every public link of this invoice was revoked.",
	"flash.requested": "Approval requested.",
	"flash.submitted": "Submitted for approval.",

	"err.request_id":        "Request id:",
	"err.home":              "Back to home",
	"err.login":             "Back to sign in",
	"err.go_back":           "Go back, fix the fields above and submit again.",
	"err.not_found_title":   "Page not found",
	"err.not_found":         "There is nothing here, or you may not see it.",
	"err.forbidden_title":   "Not permitted",
	"err.forbidden":         "Your token may not do that.",
	"err.separation_title":  "Someone else must do this",
	"err.separation":        "You cannot approve a record you created, edited or own (separation of duties). Ask another approver.",
	"err.invalid_title":     "That could not be saved",
	"err.conflict_title":    "Not possible in the current state",
	"err.locked_title":      "The period is locked",
	"err.stale_title":       "The record changed",
	"err.stale":             "The record changed since you opened this page. Reload it and try again.",
	"err.idem":              "That form was already used with different values. Reload the page for a fresh form.",
	"err.csrf_title":        "Request refused",
	"err.csrf":              "The form was not accepted (missing or invalid security token). Go back, reload the page and try again.",
	"err.busy_title":        "Slow down",
	"err.busy":              "Too many requests. Try again in a few seconds.",
	"err.unavailable_title": "Temporarily unavailable",
	"err.unavailable":       "The service is busy or took too long. Narrow the request (dates, filters) or try again in a few seconds.",
	"err.too_large_title":   "Too large",
	"err.too_large":         "The request was too large.",
	"err.internal_title":    "Something went wrong",
	"err.internal":          "An internal error occurred. Quote the request id to your administrator.",
	"err.bad_form":          "The form is incomplete or out of date. Reload the page and try again.",
	"err.bad_form_title":    "The form could not be read",

	"f.id": "Document", "f.name": "Name", "f.kind": "Kind", "f.type": "Type", "f.email": "Email", "f.phone": "Phone", "f.contact": "Contact person",
	"f.address": "Address", "f.tax_id": "Tax id", "f.currency": "Currency", "f.notes": "Notes", "f.status": "Status", "f.party": "Customer or vendor",
	"f.number": "Number", "f.issued": "Issue date", "f.due": "Due date", "f.total": "Total", "f.balance": "Balance", "f.amount": "Amount",
	"f.date": "Date", "f.from": "From", "f.to": "To", "f.project": "Project", "f.reason": "Reason", "f.description": "Description",
	"f.person": "Person", "f.created_by": "Created by", "f.created": "Created", "f.search": "Search by name", "f.all": "All", "f.choose": "Choose...",
	"f.overdue_only": "Overdue", "f.overdue_as_of": "Overdue today (%s)",

	"kind.customer": "Customer", "kind.vendor": "Vendor", "kind.both": "Customer and vendor", "kind.invoice": "Invoice", "kind.credit_note": "Credit note",
	"inv.draft": "Draft", "inv.approved": "Approved", "inv.sent": "Sent", "inv.partially_paid": "Partially paid", "inv.paid": "Paid", "inv.void": "Void",
	"ts.draft": "Draft", "ts.submitted": "Submitted", "ts.approved": "Approved", "ts.rejected": "Rejected",
	"ex.draft": "Draft", "ex.submitted": "Submitted", "ex.approved": "Approved", "ex.rejected": "Rejected", "ex.reimbursed": "Reimbursed",
	"ct.draft": "Draft", "ct.sent": "Sent", "ct.signed": "Signed", "ct.active": "Active", "ct.expired": "Expired", "ct.terminated": "Terminated",
	"po.draft": "Draft", "po.approved": "Approved", "po.received": "Received", "po.cancelled": "Cancelled",
	"rec.active": "Active", "rec.paused": "Paused", "rec.ended": "Ended",
	"interval.weekly": "Weekly", "interval.monthly": "Monthly", "interval.quarterly": "Quarterly", "interval.yearly": "Yearly",

	"home.own_only":       "This token covers your own timesheets and expenses.",
	"home.approvals":      "Waiting for your approval",
	"home.approvals_n":    "%d records are waiting for your decision.",
	"home.approvals_none": "Nothing is waiting for your approval.",
	"home.more":           "There may be more.",
	"home.open_queue":     "Open the approval queue",
	"home.overdue":        "Overdue invoices",
	"home.overdue_n":      "%d invoices are past their due date, unpaid balance:",
	"home.overdue_none":   "No invoice is overdue.",
	"home.list_overdue":   "List the overdue invoices",
	"home.month":          "This month",
	"home.issued":         "Issued this month",
	"home.received":       "Received this month",
	"home.nothing":        "Nothing yet",
	"home.month_hint":     "Totals per currency as of %s. Currencies are never added together.",
	"home.shortcuts":      "Go to",

	"approvals.hint":       "Records you could decide. Anything you created, edited or own is left out: someone else must approve it.",
	"approvals.more":       "Each list shows the first 50; decide some and reload for the rest.",
	"approvals.invoices":   "Draft invoices waiting for approval",
	"approvals.timesheets": "Submitted timesheets",
	"approvals.expenses":   "Submitted expenses",
	"approvals.orders":     "Draft purchase orders",

	"sod.hint":    "You made, edited or own this record, so someone else must approve it. The service enforces this whatever the page shows.",
	"sod.invoice": "You created this invoice (or last edited it), so someone else must approve it. The service enforces this whatever the page shows.",

	"parties.caption": "Customers and vendors", "parties.none": "No party matches.",
	"parties.pii":          "Contact details are personal data. Every listing and every page of a party is recorded in the audit log.",
	"party.new":            "New customer or vendor",
	"party.edit":           "Edit customer or vendor",
	"party.title":          "Customer or vendor",
	"party.n":              "Party #%d",
	"party.recent":         "Recent invoices",
	"party.recent_caption": "Recent invoices of this party",
	"party.erased_notice":  "This party was erased. Its contact data is gone; issued documents keep the name, address and tax id they were issued with.",
	"party.currency_hint":  "Optional three-letter code such as USD. Empty uses the company currency (%s).",
	"party.pii_form":       "Only enter what is needed to invoice this party. Email and phone are personal data when they identify a person.",

	"erase.link":         "Erase personal data",
	"erase.title":        "Erase a customer or vendor",
	"erase.party":        "Party: %s",
	"erase.what":         "What happens",
	"erase.removes":      "Name, contact person, email, phone, address, tax id and notes are removed from the party record, which is kept as an anonymous placeholder.",
	"erase.drafts":       "Draft invoices, draft contracts that were never sent, draft purchase orders and recurring schedules of this party are deleted.",
	"erase.keeps":        "Issued invoices and credit notes, payments, sent or signed contracts and approved purchase orders are NOT deleted: they are legal records kept for the legal retention period (%d years after their own date). They keep the name, address and tax id they were issued with.",
	"erase.unpaid":       "Unpaid invoices are never purged: their retention starts when they are paid or voided.",
	"erase.irreversible": "This cannot be undone. The result below says exactly what was erased and what is kept.",
	"erase.confirm":      "Type ERASE to confirm",
	"erase.confirm_hint": "Capital letters, nothing else.",
	"erase.token":        "Your sign-in token again", "erase.token_hint": "Erasing cannot be undone, so the token you signed in with is asked for once more.",
	"erase.sso_hint":     "Erasing cannot be undone. You came from the admin UI, so this works only within five minutes of signing in there. If it is refused, sign out of the admin UI, sign in again and open finance from Apps.",
	"erase.submit":       "Erase personal data",
	"erase.done_title":   "Erasure done",
	"erase.warning":      "The data was erased, but the database log could not be truncated afterwards. Ask your administrator to run the cleanup.",
	"erase.already":      "This party had already been erased; the report is repeated.",
	"erase.fields":       "Erased fields",
	"erase.deleted":      "Deleted drafts",
	"erase.kept":         "Kept as legal records",
	"erase.kept_caption": "Records kept after the erasure",
	"erase.class":        "Class", "erase.count": "Count", "erase.until": "Kept until", "erase.reason": "Why",
	"erase.no_end":       "until paid or voided, then the retention period",
	"erase.kept_hint":    "The retention period is %d years from the date of each record; the purge deletes them afterwards.",
	"erase.nothing_kept": "Nothing had to be kept.",
	"erase.back":         "Back to the party",

	"invoice.new": "New invoice", "invoice.edit": "Edit draft invoice", "invoice.title": "Invoice", "invoice.draft_n": "Draft #%d",
	"invoice.approve": "Approve", "invoice.approve_hint": "Approving gives the invoice its number and freezes it: it can never be edited afterwards, only voided or credited.",
	"invoice.request": "Request approval", "invoice.delete": "Delete draft", "invoice.send": "Mark as sent",
	"invoice.send_hint": "Sending itself (email) is up to you; this records that the invoice left the building.",
	"invoice.print":     "Printable view", "invoice.void": "Void", "invoice.void_hint": "Voiding keeps the number used. Invoices with payments or credit notes cannot be voided: issue a credit note.",
	"invoice.void_reason": "Void reason", "invoice.approved_by": "Approved by", "invoice.original": "Credit note for", "invoice.original_link": "The original invoice",
	"invoice.immutable": "This invoice is issued and cannot be edited. Void it or issue a credit note.",
	"invoice.fx":        "rate %[1]s to %[2]s", "invoice.fx_label": "Exchange rate to the company currency",
	"invoice.currency_hint": "Three-letter code. The company currency is %s; for another currency give the exchange rate.",
	"invoice.series":        "Number series", "invoice.due_days": "Days until due", "invoice.due_days_hint": "Used when no due date is given. Default 30.",
	"invoice.subtotal": "Subtotal", "invoice.tax": "Tax", "invoice.total": "Total %s", "invoice.paid": "Paid", "invoice.credited": "Credited", "invoice.balance": "Balance due",
	"invoices.caption": "Invoices and credit notes", "invoices.none": "No invoice matches.",
	"export.invoices": "Download invoices (CSV)", "export.payments": "Download payments (CSV)", "export.expenses": "Download expenses (CSV)",
	"export.hint": "Downloads follow the filters of dates and status, are limited to your scope, and every download is recorded in the audit log. Text that starts with a formula character is defused.",

	"line.desc": "Description", "line.qty": "Quantity", "line.price": "Unit price", "line.disc": "Discount %", "line.tax": "Tax", "line.sku": "Product code", "line.no_tax": "No tax",
	"lines.caption": "Lines", "lines.hint": "Leave unused rows empty. Prices are tax-exclusive, in the currency above, as plain numbers such as 1234.50. Quantities may have up to three decimals.",

	"payments.title": "Payments", "payments.caption": "Payments received",
	"pay.record": "Record a payment", "pay.amount": "Amount (%s)", "pay.hint": "Open balance: %s. A payment cannot exceed it.", "pay.method": "Method", "pay.reference": "Reference", "pay.received_on": "Received on",

	"credit.title": "Credit note", "credit.for": "Credit note for invoice", "credit.create": "Draft the credit note",
	"credit.hint": "Lines are in %s. A credit note reduces what is still unpaid; it is capped at the open balance and is approved by someone else.",

	"link.title": "Public link", "link.hint": "A signed, expiring link shows the printable invoice without signing in. It is shown once, when created. Revoking invalidates every link of this invoice.",
	"link.ttl": "Valid for (hours)", "link.create": "Create a link", "link.revoke": "Revoke all links",
	"link.created_title": "Public link created", "link.once": "Copy the link now. It is not stored and cannot be shown again.",
	"link.no_base": "No public base URL is configured, so only the path is shown.", "link.expires": "Expires %s.",
	"link.revoke_hint": "You can revoke it from the invoice page.", "link.back": "Back to the invoice",

	"recurring.new": "New schedule", "recurring.hint": "A schedule drafts an invoice each period. Drafts are never approved or sent automatically.",
	"recurring.caption": "Recurring schedules", "recurring.none": "No schedule yet.", "recurring.interval": "Interval", "recurring.next": "Next date",
	"recurring.pause": "Pause", "recurring.resume": "Resume", "recurring.end": "End", "recurring.start": "First date", "recurring.end_date": "Last date (optional)",
	"recurring.draft_hint": "The drafts are made in your name, so you cannot approve them.",

	"contract.new": "New contract", "contract.title": "Title", "contracts.caption": "Contracts", "contracts.none": "No contract yet.",
	"contract.choose_template": "Choose a template", "contract.no_templates": "There is no contract template yet. Templates are created through the API.",
	"contract.vars": "%d variables", "contract.template": "Template:", "contract.variable": "Value for %s",
	"contract.start": "Start date", "contract.end": "End date", "contract.renewal": "Renewal date", "contract.reminder": "Remind this many days before renewal",
	"contract.plain_hint":  "Values are inserted as plain text. Nothing is interpreted.",
	"contract.sign_status": "Signature", "contract.signed_at": "Signed %s", "contract.not_signed": "Not signed yet", "contract.hash": "Text hash (SHA-256)",
	"contract.signed_hash": "Signed text hash", "contract.provider": "E-signature provider", "contract.text": "Current text", "contract.versions": "Versions",
	"contract.versions_hint": "Versions are read-only: the text of a signed contract never changes.", "contract.version_n": "Version %d",
	"contract.version_of": "Version %d of %s", "contract.version_readonly": "This version is read-only.", "contract.back": "Back to the contract",
	"contract.send": "Send for signature", "contract.mode": "How it is signed", "contract.mode_manual": "Signed outside this system (record later)",
	"contract.mode_esign": "E-signature provider", "contract.signers": "Signer email addresses",
	"contract.send_hint": "Sending freezes the text. Signer addresses go to the provider and are not stored here.",
	"contract.dates":     "Dates and reminders", "contract.terminate": "Terminate",

	"signing.send": "Send a signing link by email", "signing.send_btn": "Send the links",
	"signing.counterparty": "Who signs for the other side", "signing.company": "Who countersigns for the company (optional)",
	"signing.lines_hint": "One person per line, written as: Full Name <address@example.com>",
	"signing.ordered":    "Invite one after another, in the order written (the company last)", "signing.days": "Days each link works (1 to 30)",
	"signing.hint":  "Sending freezes the text. Each person gets a link that only they receive and that works once. A signature is identified by access to that mailbox only.",
	"signing.title": "Signature by link", "signing.status": "Request", "signing.caption": "Signers", "signing.signer": "Signer", "signing.role": "Role",
	"signing.state": "Status", "signing.mail": "Invitation email", "signing.expires": "Link expires", "signing.expired": "(expired)",
	"signing.signed_as": "Signed as", "signing.reason": "Reason given", "signing.resend": "Send a new link", "signing.revoke": "Withdraw the request",
	"signing.certificate": "Completion certificate", "signing.hash": "Document sent (SHA-256)",
	"env.pending": "Waiting for signatures", "env.completed": "Completed", "env.declined": "Declined", "env.revoked": "Withdrawn",
	"role.counterparty": "Other side", "role.company": "Company",
	"signer.waiting": "Not invited yet", "signer.invited": "Invited", "signer.signed": "Signed", "signer.declined": "Declined",
	"mailst.pending": "Not confirmed", "mailst.queued": "Handed to the mail service",
	"mailst.not_allowed": "Refused: this address is not allowed (company.yaml email.modules.finance)", "mailst.failed": "Not sent: try again",

	"ts.title": "Week of %s", "ts.week": "Week starting", "ts.week_start": "Week starting (a Monday)", "ts.monday_hint": "Opening a week that already exists returns it.",
	"ts.open": "Open a week", "ts.open_btn": "Open", "ts.own_only": "This token shows your own timesheets only.", "ts.caption": "Timesheets", "ts.none": "No timesheet yet.",
	"ts.total": "Total", "ts.billable": "Billable", "ts.task": "Task", "ts.grid_caption": "Hours per day",
	"ts.grid_hint": "Type hours as 7:30 or 7. Leave days empty. A row needs a project key such as ACME. Saving replaces the whole week.",
	"ts.cell":      "Row %d, %s", "ts.locked": "This week is locked: records dated on or before %s can no longer change.",
	"ts.rejected_because": "Sent back: %s", "ts.submit": "Submit for approval", "ts.save_first": "Save the grid before submitting.", "ts.decide": "Decision",

	"expense.new": "New expense", "expense.title": "Expense", "expenses.caption": "Expenses", "expenses.none": "No expense yet.", "expense.category": "Category",
	"expense.category_hint": "A lower-case word such as travel or meals.", "expense.incurred": "Date incurred", "expense.submit": "Submit for approval",
	"expense.decided_by": "Decided by", "expense.reference": "Reimbursement reference", "expense.reimburse": "Record reimbursement",
	"receipt.legend": "Receipt (optional)", "receipt.hint": "Only the receipt's name, type, size and SHA-256 are recorded here. The file itself is kept elsewhere.",
	"receipt.filename": "File name", "receipt.type": "File type", "receipt.size": "Size in bytes", "receipt.sha256": "SHA-256 of the file",

	"po.new": "New purchase order", "po.title": "Purchase order", "po.vendor": "Vendor", "po.draft_n": "Draft PO #%d", "pos.caption": "Purchase orders", "pos.none": "No purchase order yet.",
	"po.receive": "Mark as received", "po.cancel": "Cancel the order",

	"picker.search": "Find a party by name", "picker.more": "Only the first 100 matching parties are listed. Search by name to narrow the list.",
	"report.aging": "Receivables aging", "report.statement": "Customer statement",
	"aging.as_of": "As of", "aging.hint": "Unpaid balances of issued invoices by days past due, per currency. Credit notes are netted. Currencies are never added together.",
	"aging.caption": "Receivables in %s", "aging.current": "Not yet due", "aging.d30": "1 to 30 days", "aging.d60": "31 to 60 days", "aging.d90": "61 to 90 days",
	"aging.over": "Over 90 days", "aging.total": "Total", "aging.all_parties": "All customers", "aging.hidden": "%d more customers are not shown; narrow the date or use the CSV export.",
	"aging.none":      "Nothing is outstanding.",
	"statement.owing": "Open balance:", "statement.caption": "Documents and payments", "statement.payment": "Payment", "statement.more": "Only the newest 100 documents are shown.",

	"settings.readonly": "This page is read-only. Settings are changed in the module configuration; tax rates through the API.",
	"settings.series":   "Number series", "settings.series_caption": "Numbering series", "settings.series_code": "Series", "settings.series_last": "Last number issued",
	"settings.series_scoped": "Numbering counters are company-wide and are not shown to a token limited to some customers or projects.",
	"settings.series_none":   "No number has been issued yet.", "settings.series_hint": "Numbers are taken only when a document is approved, so they have no gaps.",
	"settings.tax": "Tax rates", "settings.tax_caption": "Tax rates", "settings.tax_code": "Code", "settings.tax_rate": "Rate", "settings.tax_none": "No tax rate is defined.",
	"settings.general": "General", "settings.base": "Company currency", "settings.seller": "Seller on invoices", "settings.payment": "Payment instructions",
	"settings.retention": "Record retention", "settings.years": "%d years", "settings.audit": "Audit log retention", "settings.days": "%d days",
	"settings.lock": "Periods locked through", "settings.no_lock": "No period is locked", "settings.public": "Public link base URL", "settings.not_set": "Not set",
	"settings.where": "The period lock is set through the API by a finance administrator.",
}

func (u *UI) msg(key string, args ...any) string {
	s, ok := uiMessagesEN[key]
	if !ok {
		return key
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// msgOr returns the catalogue text of a dynamic key, or the fallback when there is none (an
// unknown status coming from the data is shown as it is, escaped).
func (u *UI) msgOr(key, fallback string) string {
	if s, ok := uiMessagesEN[key]; ok {
		return s
	}
	return fallback
}

// flashKeys are the only flash messages the "ok" query parameter can select, so a crafted link
// cannot put text of its choosing on the page.
var flashKeys = []string{"flash.saved", "flash.deleted", "flash.approved", "flash.rejected", "flash.sent", "flash.paid", "flash.voided", "flash.revoked", "flash.requested", "flash.submitted", "flash.invited", "flash.withdrawn"}

func flashMsg(u *UI, key string) string {
	for _, k := range flashKeys {
		if k == key {
			return u.msg(k)
		}
	}
	return ""
}
