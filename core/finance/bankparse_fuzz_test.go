package finance

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// checkParsed holds whatever a parser accepted to the rules the importer relies on.
func checkParsed(t *testing.T, ps *ParsedStatement, err error) {
	t.Helper()
	if err != nil {
		return
	}
	if len(ps.Txns) == 0 || len(ps.Txns) > maxStatementRows {
		t.Fatalf("%d transactions accepted", len(ps.Txns))
	}
	for _, x := range ps.Txns {
		if x.AmountMinor == 0 || x.AmountMinor > maxMinor || x.AmountMinor < -maxMinor {
			t.Fatalf("amount %d accepted", x.AmountMinor)
		}
		if !validDate(x.Date) || x.Currency != "USD" || x.Key == "" || len(x.Key) > 200 {
			t.Fatalf("bad transaction %+v", x)
		}
		for _, s := range []string{x.Reference, x.Counterparty, x.Description, x.Key} {
			if !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n\t") {
				t.Fatalf("unclean text %q", s)
			}
		}
	}
}

func FuzzParseCSV(f *testing.F) {
	f.Add("d,a,r\n2026-01-01,1.00,x\n2026-01-02,-2.50,\"y,z\"\n")
	f.Add("2026-01-01;1.234,50;a\n")
	f.Add("\"unterminated\n")
	cfg := CSVConfig{Delimiter: ',', HasHeader: true, DateCol: 1, AmountCol: 2, ReferenceCol: 3}
	f.Fuzz(func(t *testing.T, in string) {
		ps, err := ParseCSV(strings.NewReader(in), cfg, "USD")
		checkParsed(t, ps, err)
	})
}

func FuzzParseOFX(f *testing.F) {
	f.Add(ofxSample)
	f.Add("<OFX><STMTTRN><TRNAMT>1.00<DTPOSTED>20260101<FITID>a</STMTTRN></OFX>")
	f.Add("<!DOCTYPE x><OFX>")
	f.Fuzz(func(t *testing.T, in string) {
		ps, err := ParseOFX(strings.NewReader(in), "USD")
		checkParsed(t, ps, err)
	})
}

func FuzzParseCAMT053(f *testing.F) {
	f.Add(camtSample)
	f.Add(`<?xml version="1.0"?><!DOCTYPE a [<!ENTITY b "c">]><a>&b;</a>`)
	f.Add(`<Document><Stmt><Ntry><Amt Ccy="USD">1.00</Amt><CdtDtInd>CRDT</CdtDtInd><BookgDt><Dt>2026-01-01</Dt></BookgDt></Ntry></Stmt></Document>`)
	f.Fuzz(func(t *testing.T, in string) {
		ps, err := ParseCAMT053(strings.NewReader(in), "USD")
		checkParsed(t, ps, err)
	})
}

func FuzzPayrollCSV(f *testing.F) {
	f.Add("date,account_code,debit,credit,currency,memo\n2026-06-30,5000,1.00,,USD,a\n2026-06-30,1000,,1.00,USD,b\n")
	f.Fuzz(func(t *testing.T, in string) {
		js, err := csvPayroll{}.ParseJournal(strings.NewReader(in))
		if err != nil {
			return
		}
		for _, j := range js {
			if !validDate(j.EntryDate) || len(j.Lines) == 0 || len(j.Lines) > maxJournalLines {
				t.Fatalf("bad journal %+v", j)
			}
		}
	})
}
