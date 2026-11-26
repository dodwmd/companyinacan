package finance

// The accounting pages of the web UI (E15c, specs/035-accounting): the chart of accounts, the journal
// (read-only), the trial balance, and the bank reconciliation queue with confirm and ignore forms. Like
// every page they are clients of the Store: scoping, separation of duties, audit and the payment rules
// are the Store's. The route table lists finance.ledger.read or finance.ledger.write, and allowedBy lets
// only an unscoped token in.

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

const reconQueueSize = 25

type ledgerAccountsData struct {
	Items []Account
	All   bool
}

func (u *UI) ledgerAccounts(c *uiCtx) error {
	all := c.q("all") == "1"
	items, err := u.Store.ListAccounts(c.ctx(), c.p, all)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "ledger_accounts.html", "ledger.accounts", ledgerAccountsData{Items: items, All: all})
	return nil
}

type ledgerJournalsData struct {
	Items  []JournalEntry
	Status string
	Source string
	Pager  pager
}

var journalSources = []string{"opening", "manual", "payroll", "invoice", "credit_note", "invoice_void", "payment", "expense", "expense_reimbursed", "reversal"}

func (u *UI) ledgerJournals(c *uiCtx) error {
	before, err := c.cursor()
	if err != nil {
		return err
	}
	st, src := c.q("status"), c.q("source")
	if st != "" && st != "draft" && st != "posted" {
		return invalid("status", "not a status")
	}
	if src != "" && !slices.Contains(journalSources, src) {
		return invalid("source", "not a source")
	}
	items, next, err := u.Store.ListJournals(c.ctx(), c.p, JournalFilter{Status: st, Source: src}, before, uiPageSize)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "ledger_journals.html", "ledger.journals", ledgerJournalsData{Items: items, Status: st, Source: src, Pager: pager{nextLink("/ledger/journals", c.r.URL.Query(), next)}})
	return nil
}

func (u *UI) ledgerJournal(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	e, err := u.Store.GetJournal(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "ledger_journal.html", "ledger.journal", e)
	return nil
}

type trialData struct {
	AsOf string
	TB   *TrialBalance
}

func (u *UI) ledgerTrialBalance(c *uiCtx) error {
	asOf, err := c.queryDate("as_of")
	if err != nil {
		return err
	}
	if asOf == "" {
		asOf = c.today()
	}
	tb, err := u.Store.TrialBalanceAsOf(c.ctx(), c.p, asOf)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "ledger_trial_balance.html", "ledger.trial", trialData{AsOf: asOf, TB: tb})
	return nil
}

type reconRow struct {
	Txn         BankTxn
	Suggestions []Suggestion
	Idem        string
}

type reconData struct {
	Accounts []BankAccount
	Account  *BankAccount
	Periods  []ReconPeriod
	Rows     []reconRow
	Write    bool
	Pager    pager
}

// bankReconciliation shows one bank account: its months and the oldest unmatched transactions with the
// suggestions. Reading it is audited by the Store before anything is returned.
func (u *UI) bankReconciliation(c *uiCtx) error {
	accounts, err := u.Store.ListBankAccounts(c.ctx(), c.p)
	if err != nil {
		return err
	}
	d := reconData{Accounts: accounts, Write: capsOf(c.p).LedgerWrite}
	if len(accounts) > 0 {
		pick := accounts[0].ID
		if q := c.q("account"); q != "" {
			if pick, err = parseID(q); err != nil {
				return err
			}
		}
		for i := range accounts {
			if accounts[i].ID == pick {
				d.Account = &accounts[i]
			}
		}
		if d.Account == nil {
			return ErrNotFound
		}
		after, err := c.cursor()
		if err != nil {
			return err
		}
		if d.Periods, err = u.Store.Reconciliation(c.ctx(), c.p, pick); err != nil {
			return err
		}
		txns, next, err := u.Store.ListBankTransactions(c.ctx(), c.p, TxnFilter{AccountID: pick, Status: "unmatched"}, after, reconQueueSize)
		if err != nil {
			return err
		}
		var sg map[int64][]Suggestion
		if len(txns) > 0 {
			ids := make([]int64, len(txns))
			for i, t := range txns {
				ids[i] = t.ID
			}
			if sg, err = u.Store.SuggestMatchesFor(c.ctx(), c.p, ids); err != nil {
				return err
			}
		}
		for _, t := range txns {
			d.Rows = append(d.Rows, reconRow{Txn: t, Suggestions: sg[t.ID], Idem: session.NewToken()[:32]})
		}
		d.Pager = pager{nextLink("/bank/reconciliation", c.r.URL.Query(), next)}
	}
	u.render(c.w, c.r, c, http.StatusOK, "bank_reconciliation.html", "bank.title", d)
	return nil
}

// reconBack is where a form returns to: the same bank account, with the flash message.
func reconBack(c *uiCtx, flash string) string {
	v := url.Values{"ok": {flash}}
	if a := c.form("account"); a != "" {
		if _, err := parseID(a); err == nil {
			v.Set("account", a)
		}
	}
	return "/bank/reconciliation?" + v.Encode()
}

// bankConfirm records the payment a person confirmed. Nothing is posted by suggesting.
func (u *UI) bankConfirm(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	idem, err := formIdem(c.r)
	if err != nil {
		return err
	}
	inv, err := strconv.ParseInt(strings.TrimSpace(c.form("invoice_id")), 10, 64)
	if err != nil || inv < 1 {
		return invalid("invoice_id", "give the invoice id")
	}
	in := ConfirmInput{InvoiceID: inv, RecordNew: c.form("record_new") == "1"}
	if v := c.form("payment_id"); v != "" {
		if in.PaymentID, err = strconv.ParseInt(v, 10, 64); err != nil || in.PaymentID < 1 {
			return invalid("payment_id", "give the payment id, or leave it empty")
		}
	}
	if _, _, err := u.Store.ConfirmMatch(c.ctx(), c.p, id, in, idem); err != nil {
		return err
	}
	return c.redirect(reconBack(c, "flash.paid"), "")
}

func (u *UI) bankIgnore(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if _, err := u.Store.IgnoreTransaction(c.ctx(), c.p, id, c.form("reason")); err != nil {
		return err
	}
	return c.redirect(reconBack(c, "flash.saved"), "")
}
