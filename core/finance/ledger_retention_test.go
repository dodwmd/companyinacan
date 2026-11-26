package finance

import (
	"context"
	"testing"
	"time"
)

// Retention (E15c): accounting records outlive erasure, and decided bank transactions go with their
// retention period; open work (unmatched) is never purged.
func TestRetentionAndErasureForAccountingRecords(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	acc := e.token("dana", acct, nil, nil, false)
	party := e.party(maker, "Erase Me Ltd", "customer")
	inv := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("work", 1000, 15000))
	b := e.bankAccount(admin)
	e.must(201, acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, "date,amount,reference,name,id\n2026-06-10,150.00,"+*inv.Number+",Erase Me Ltd,A1\n2026-06-11,5.00,open item,Z,A2\n", nil)
	var txns struct{ Items []BankTxn }
	e.must(200, acc, "GET", "/v1/bank/transactions?account_id="+id(b.ID), nil, &txns)
	e.must(200, acc, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv.ID}, nil)
	var before int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&before)
	// Erasing the party does not touch the accounting records.
	e.must(200, adminTok, "POST", "/v1/parties/"+id(party)+"/erase", obj{}, nil)
	var after int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&after)
	if before == 0 || after != before {
		t.Fatalf("journal entries %d before erasure, %d after", before, after)
	}
	e.advance(8 * 365 * 24 * time.Hour)
	rep, err := e.app.Purge(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.BankTransactions != 1 || rep.Invoices != 1 {
		t.Errorf("purge %+v", rep)
	}
	var journals, open int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&journals)
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM bank_transactions WHERE status = 'unmatched'`).Scan(&open)
	if journals != before || open != 1 {
		t.Errorf("after the purge: %d journal entries (want %d), %d unmatched (want 1)", journals, before, open)
	}
}
