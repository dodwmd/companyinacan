package finance

// Bank statement parsers (E15c). Statement files come from outside the trust boundary, so every parser is
// strict and bounded: a size cap, a row cap, a field cap, a nesting cap, no document type declarations and
// therefore no entity expansion, no external entities, and one linear pass over the bytes. Money is parsed
// from its decimal text straight to integer minor units: no float is ever created.

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxStatementBytes and maxStatementRows bound one statement file.
	maxStatementBytes = 2 << 20
	maxStatementRows  = 5000
	maxFieldBytes     = 1000
	maxXMLDepth       = 32
	maxSGMLDepth      = 16
	maxAccountIDBytes = 64
	maxReferenceBytes = 200
)

// statementIDs holds one file to one account and one currency. A file with several statement blocks is
// accepted only when every block names the same account and currency (or, for the account, none does): entries
// are never attributed to an account their block did not name.
type statementIDs struct {
	blocks, named int
	account, cur  string
}

// block records the identifiers one statement block gave (empty when it gave none).
func (s *statementIDs) block(account, cur string) error {
	s.blocks++
	if len(account) > maxAccountIDBytes {
		return parseErr("the account identifier is longer than %d bytes", maxAccountIDBytes)
	}
	if account != "" {
		if s.account != "" && normalizeNumber(account) != normalizeNumber(s.account) {
			return parseErr("the file holds statements of more than one account; import one account at a time")
		}
		s.account = account
		s.named++
	}
	if cur != "" {
		if s.cur != "" && cur != s.cur {
			return parseErr("the file holds statements in more than one currency")
		}
		s.cur = cur
	}
	if s.named != 0 && s.named != s.blocks {
		return parseErr("some statements in the file name their account and some do not")
	}
	return nil
}

// set adds a value seen inside a block, refusing a second different one.
func setOnce(dst *string, v, what string) error {
	if *dst != "" && *dst != v {
		return parseErr("a statement names more than one %s", what)
	}
	*dst = v
	return nil
}

// ParsedTxn is one transaction read from a statement.
type ParsedTxn struct {
	Key          string // the bank's transaction id; empty means "derive it from the row"
	ServicerRef  string // CAMT AcctSvcrRef: part of the key, not an id on its own (servicerKeys)
	Date         string // YYYY-MM-DD
	AmountMinor  int64  // signed, money in positive
	Currency     string
	Reference    string
	Counterparty string
	Description  string
}

// ParsedStatement is a parsed file.
type ParsedStatement struct {
	AccountID string // the account identifier the file names, "" if it names none
	Currency  string // the statement currency if the file names one
	Txns      []ParsedTxn
}

// errParse is a rejected statement file; its message is safe to show (it never echoes file content).
type errParse struct{ msg string }

func (e *errParse) Error() string { return e.msg }

func parseErr(format string, a ...any) error { return &errParse{msg: fmt.Sprintf(format, a...)} }

// readStatement reads at most maxStatementBytes and refuses a bigger file.
func readStatement(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxStatementBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxStatementBytes {
		return nil, parseErr("the statement is larger than %d bytes", maxStatementBytes)
	}
	if !utf8.Valid(b) {
		return nil, parseErr("the statement is not valid UTF-8")
	}
	return b, nil
}

// parseDecimalMinor turns a decimal string into minor units for a currency with exp decimals. Only ASCII
// digits, one optional sign, an optional thousands separator (when thousands != 0) and at most one decimal
// separator are accepted; more fraction digits than exp are accepted only when they are zeros.
func parseDecimalMinor(s string, exp int, decimal, thousands rune) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 40 {
		return 0, errors.New("not an amount")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	var whole, frac []byte
	seenDec := false
	// Grouping is checked, not just stripped: the first group has 1 to 3 digits and every later one exactly 3, so
	// a decimal comma read as a thousands separator ("12,50") is an error instead of 100 times the amount.
	group, grouped := 0, false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if seenDec {
				frac = append(frac, byte(r))
			} else {
				whole = append(whole, byte(r))
				group++
			}
		case r == decimal && !seenDec:
			if grouped && group != 3 {
				return 0, errors.New("not an amount: digit groups must have three digits")
			}
			seenDec = true
		case thousands != 0 && r == thousands && !seenDec:
			if group == 0 || group > 3 || (grouped && group != 3) {
				return 0, errors.New("not an amount: digit groups must have three digits")
			}
			group, grouped = 0, true
		default:
			return 0, errors.New("not an amount")
		}
	}
	if !seenDec && grouped && group != 3 {
		return 0, errors.New("not an amount: digit groups must have three digits")
	}
	if len(whole) == 0 && len(frac) == 0 {
		return 0, errors.New("not an amount")
	}
	for len(frac) > exp {
		if frac[len(frac)-1] != '0' {
			return 0, errors.New("more decimals than the currency has")
		}
		frac = frac[:len(frac)-1]
	}
	for len(frac) < exp {
		frac = append(frac, '0')
	}
	var v int64
	for _, d := range append(whole, frac...) {
		v = v*10 + int64(d-'0')
		if v > maxMinor {
			return 0, errors.New("amount too large")
		}
	}
	if neg {
		v = -v
	}
	return v, nil
}

func cleanText(s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return "", parseErr("a field is longer than %d bytes", max)
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s), nil
}

// finishStatement checks every transaction against the account's currency and normalises its text.
func finishStatement(ps *ParsedStatement, accountCurrency string) error {
	if _, ok := CurrencyExponent(accountCurrency); !ok {
		return parseErr("unknown account currency")
	}
	if len(ps.Txns) == 0 {
		return parseErr("the statement has no transactions")
	}
	if len(ps.Txns) > maxStatementRows {
		return parseErr("the statement has more than %d transactions", maxStatementRows)
	}
	seen := map[string]string{}
	for i := range ps.Txns {
		t := &ps.Txns[i]
		n := i + 1
		if !validDate(t.Date) {
			return parseErr("transaction %d: bad date", n)
		}
		if t.AmountMinor == 0 || t.AmountMinor > maxMinor || t.AmountMinor < -maxMinor {
			return parseErr("transaction %d: the amount is zero or out of range", n)
		}
		if t.Currency == "" {
			t.Currency = accountCurrency
		}
		if t.Currency != accountCurrency {
			return parseErr("transaction %d: the currency differs from the bank account's", n)
		}
		var err error
		if t.Reference, err = cleanText(t.Reference, maxReferenceBytes); err != nil {
			return err
		}
		if t.Counterparty, err = cleanText(t.Counterparty, 200); err != nil {
			return err
		}
		if t.Description, err = cleanText(t.Description, 500); err != nil {
			return err
		}
		if t.Key, err = cleanText(t.Key, 200); err != nil {
			return err
		}
		if t.ServicerRef, err = cleanText(t.ServicerRef, 200); err != nil {
			return err
		}
		if strings.HasPrefix(t.Key, derivedKeyPrefix) || strings.HasPrefix(t.Key, servicerKeyPrefix) {
			t.Key = "id:" + t.Key
		}
		if t.Key != "" {
			content := strings.Join([]string{t.Date, fmt.Sprint(t.AmountMinor), t.Reference, t.Counterparty, t.Description}, "\x1f")
			if prev, dup := seen[t.Key]; dup && prev != content {
				return parseErr("transaction %d: the id is used twice for different transactions", n)
			}
			seen[t.Key] = content
		}
	}
	var err error
	if ps.AccountID, err = cleanText(ps.AccountID, maxAccountIDBytes); err != nil {
		return err // never blank an identifier: that would skip the check that the statement is this account's
	}
	if ps.Currency != "" && ps.Currency != accountCurrency {
		return parseErr("the statement currency differs from the bank account's")
	}
	return nil
}

// appendCapped joins v to s with a space and never grows past max bytes (cut on a rune boundary), so a
// document with a great many remittance lines costs linear time, not a copy of everything per line.
func appendCapped(s, v string, max int) string {
	if v == "" || len(s) >= max {
		return s
	}
	if s != "" {
		s += " "
	}
	room := max - len(s)
	if len(v) > room {
		for room > 0 && !utf8.RuneStart(v[room]) {
			room--
		}
		v = v[:room]
	}
	return s + v
}

// servicerKeyPrefix marks a CAMT key built from the account servicer's reference and the entry's content.
const servicerKeyPrefix = "a:"

// derivedKeyPrefix marks a key made from the row's content, as opposed to the bank's own id.
const derivedKeyPrefix = "h:"

// deriveKeys gives every transaction without a bank id a key from its content plus its occurrence number in
// the file, so identical rows in one file both import and re-importing a file yields the same keys.
func deriveKeys(txns []ParsedTxn) {
	count := map[string]int{}
	for i := range txns {
		t := &txns[i]
		if t.Key != "" {
			continue
		}
		base := contentOf(*t)
		count[base]++
		t.Key = contentKey(base, count[base])
	}
}

// contentOf is what makes two rows without a bank id "equal".
func contentOf(t ParsedTxn) string {
	return strings.Join([]string{t.Date, fmt.Sprint(t.AmountMinor), t.Currency, t.Reference, t.Counterparty, t.Description}, "\x1f")
}

// contentKey is the key of the n-th (from 1) row with that content.
func contentKey(content string, n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x1e%d", content, n)))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// ---- CSV ----------------------------------------------------------------------------------

// CSVConfig says how to read a CSV statement. Nothing is guessed: the delimiter, the columns and the
// formats are given by the caller. Columns are numbered from 1; 0 means "none".
type CSVConfig struct {
	Delimiter       rune
	HasHeader       bool
	DateCol         int
	AmountCol       int // a signed amount; or
	DebitCol        int // money out (positive numbers) with
	CreditCol       int // money in
	ReferenceCol    int
	DescriptionCol  int
	CounterpartyCol int
	IDCol           int
	DateLayout      string // Go layout, default 2006-01-02
	DecimalSep      rune   // default '.'
	ThousandsSep    rune   // default none
}

func (c *CSVConfig) check() error {
	var p problems
	switch c.Delimiter {
	case ',', ';', '\t', '|':
	default:
		p.add("delimiter", "give the delimiter explicitly: comma, semicolon, tab or pipe")
	}
	cols := []int{c.DateCol, c.AmountCol, c.DebitCol, c.CreditCol, c.ReferenceCol, c.DescriptionCol, c.CounterpartyCol, c.IDCol}
	for _, v := range cols {
		if v < 0 || v > 200 {
			p.add("columns", "column numbers are 1 to 200 (0 for none)")
			break
		}
	}
	if c.DateCol == 0 {
		p.add("date_col", "required")
	}
	if c.AmountCol == 0 && (c.DebitCol == 0 || c.CreditCol == 0) {
		p.add("amount_col", "give amount_col, or both debit_col and credit_col")
	}
	if c.AmountCol != 0 && (c.DebitCol != 0 || c.CreditCol != 0) {
		p.add("amount_col", "give amount_col or debit_col and credit_col, not both")
	}
	if c.DateLayout == "" {
		c.DateLayout = time.DateOnly
	}
	if _, err := time.Parse(c.DateLayout, time.Now().Format(c.DateLayout)); err != nil {
		p.add("date_layout", "not a usable date layout")
	}
	if c.DecimalSep == 0 {
		c.DecimalSep = '.'
	}
	if c.DecimalSep != '.' && c.DecimalSep != ',' {
		p.add("decimal", "'.' or ','")
	}
	if c.ThousandsSep == c.DecimalSep {
		p.add("thousands", "must differ from the decimal separator")
	}
	return p.err()
}

// ParseCSV reads an RFC 4180 statement with the explicit configuration.
func ParseCSV(r io.Reader, cfg CSVConfig, currency string) (*ParsedStatement, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	exp, ok := CurrencyExponent(currency)
	if !ok {
		return nil, parseErr("unknown account currency")
	}
	b, err := readStatement(r)
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(bytes.NewReader(b))
	cr.Comma = cfg.Delimiter
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	need := max(cfg.DateCol, cfg.AmountCol, cfg.DebitCol, cfg.CreditCol, cfg.ReferenceCol, cfg.DescriptionCol, cfg.CounterpartyCol, cfg.IDCol)
	ps := &ParsedStatement{}
	row := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, parseErr("the file is not valid CSV near row %d", row+1)
		}
		row++
		if row == 1 && cfg.HasHeader {
			continue
		}
		if len(ps.Txns) >= maxStatementRows {
			return nil, parseErr("the statement has more than %d transactions", maxStatementRows)
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue // a blank line
		}
		if len(rec) < need {
			return nil, parseErr("row %d has %d columns, the configuration needs %d", row, len(rec), need)
		}
		for _, f := range rec {
			if len(f) > maxFieldBytes {
				return nil, parseErr("row %d: a field is longer than %d bytes", row, maxFieldBytes)
			}
		}
		col := func(n int) string {
			if n == 0 {
				return ""
			}
			return rec[n-1]
		}
		d, err := time.Parse(cfg.DateLayout, strings.TrimSpace(col(cfg.DateCol)))
		if err != nil {
			return nil, parseErr("row %d: bad date", row)
		}
		t := ParsedTxn{Date: d.Format(time.DateOnly), Currency: currency, Reference: col(cfg.ReferenceCol), Description: col(cfg.DescriptionCol), Counterparty: col(cfg.CounterpartyCol), Key: col(cfg.IDCol)}
		if cfg.AmountCol != 0 {
			t.AmountMinor, err = parseDecimalMinor(col(cfg.AmountCol), exp, cfg.DecimalSep, cfg.ThousandsSep)
			if err != nil {
				return nil, parseErr("row %d: bad amount", row)
			}
		} else {
			var in, out int64
			if s := strings.TrimSpace(col(cfg.CreditCol)); s != "" {
				if in, err = parseDecimalMinor(s, exp, cfg.DecimalSep, cfg.ThousandsSep); err != nil {
					return nil, parseErr("row %d: bad credit amount", row)
				}
			}
			if s := strings.TrimSpace(col(cfg.DebitCol)); s != "" {
				if out, err = parseDecimalMinor(s, exp, cfg.DecimalSep, cfg.ThousandsSep); err != nil {
					return nil, parseErr("row %d: bad debit amount", row)
				}
			}
			if in < 0 || out < 0 {
				return nil, parseErr("row %d: debit and credit columns hold positive amounts", row)
			}
			t.AmountMinor = in - out
		}
		ps.Txns = append(ps.Txns, t)
	}
	if err := finishStatement(ps, currency); err != nil {
		return nil, err
	}
	deriveKeys(ps.Txns)
	return ps, nil
}

// ---- OFX / QFX 1.x (SGML) -----------------------------------------------------------------

var ofxEntities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'")

// ParseOFX reads the OFX 1.x SGML subset (bank statements, as QFX uses). It accepts tags with or without
// closing tags, ignores the header lines before <OFX>, and refuses anything that declares or references
// entities: only &amp; &lt; &gt; &quot; and &apos; are understood, and nothing is ever expanded further.
func ParseOFX(r io.Reader, currency string) (*ParsedStatement, error) {
	b, err := readStatement(r)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(b, []byte("<!")) {
		return nil, parseErr("document type and entity declarations are not accepted")
	}
	start := bytes.Index(bytes.ToUpper(b), []byte("<OFX>"))
	if start < 0 {
		return nil, parseErr("not an OFX statement: no <OFX> element")
	}
	b = b[start:]
	ps := &ParsedStatement{}
	var cur *ParsedTxn
	var stack []string
	var ids statementIDs
	var stAccount, stCur string
	inBlock := false
	i := 0
	for i < len(b) {
		lt := bytes.IndexByte(b[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		gt := bytes.IndexByte(b[i:], '>')
		if gt < 0 || gt > 80 {
			return nil, parseErr("a tag is not closed")
		}
		tag := strings.ToUpper(strings.TrimSpace(string(b[i+1 : i+gt])))
		i += gt + 1
		if strings.HasPrefix(tag, "?") {
			continue // processing instruction (<?OFX ...?>)
		}
		if strings.HasPrefix(tag, "/") {
			name := tag[1:]
			// An SGML leaf with no value and no closing tag looks like an opening aggregate. Closing an aggregate
			// therefore unwinds whatever was left open inside it, so empty leaves do not pile up to the depth cap.
			for j := len(stack) - 1; j >= 0; j-- {
				if stack[j] == name {
					stack = stack[:j]
					break
				}
			}
			if (name == "STMTRS" || name == "CCSTMTRS") && inBlock {
				if err := ids.block(stAccount, stCur); err != nil {
					return nil, err
				}
				inBlock, stAccount, stCur = false, "", ""
			}
			if name == "STMTTRN" && cur != nil {
				ps.Txns = append(ps.Txns, *cur)
				cur = nil
			}
			continue
		}
		if strings.ContainsAny(tag, " \t\r\n/=\"'") || tag == "" {
			return nil, parseErr("a tag has attributes or odd characters")
		}
		// Text up to the next tag is the value of a leaf; an aggregate has only whitespace.
		nx := bytes.IndexByte(b[i:], '<')
		if nx < 0 {
			nx = len(b) - i
		}
		raw := strings.TrimSpace(string(b[i : i+nx]))
		if raw == "" {
			if len(stack) >= maxSGMLDepth {
				return nil, parseErr("the statement is nested too deeply")
			}
			stack = append(stack, tag)
			if tag == "STMTRS" || tag == "CCSTMTRS" {
				if inBlock {
					return nil, parseErr("a statement block is not closed")
				}
				inBlock = true
			}
			if tag == "STMTTRN" {
				if len(ps.Txns) >= maxStatementRows {
					return nil, parseErr("the statement has more than %d transactions", maxStatementRows)
				}
				cur = &ParsedTxn{}
			}
			continue
		}
		if len(raw) > maxFieldBytes {
			return nil, parseErr("a field is longer than %d bytes", maxFieldBytes)
		}
		if strings.Contains(raw, "&") {
			raw = ofxEntities.Replace(raw)
			if strings.ContainsAny(raw, "&") && entityLike(raw) {
				return nil, parseErr("entity references other than the five predefined ones are not accepted")
			}
		}
		switch {
		case cur != nil:
			switch tag {
			case "DTPOSTED":
				if len(raw) < 8 {
					return nil, parseErr("bad transaction date")
				}
				d, err := time.Parse("20060102", raw[:8])
				if err != nil {
					return nil, parseErr("bad transaction date")
				}
				cur.Date = d.Format(time.DateOnly)
			case "TRNAMT":
				exp, ok := CurrencyExponent(currency)
				if !ok {
					return nil, parseErr("unknown account currency")
				}
				if cur.AmountMinor, err = parseDecimalMinor(raw, exp, '.', 0); err != nil {
					return nil, parseErr("bad transaction amount")
				}
			case "FITID":
				cur.Key = raw
			case "NAME":
				cur.Counterparty = raw
			case "MEMO":
				cur.Description = raw
			case "CHECKNUM", "REFNUM":
				if cur.Reference == "" {
					cur.Reference = raw
				}
			case "CURRENCY":
				cur.Currency = strings.ToUpper(raw)
			case "CURSYM":
				// <CURRENCY> is an aggregate: the amount is in its CURSYM. (<ORIGCURRENCY> describes an amount already
				// converted to the statement currency, so it is ignored.)
				if slices.Contains(stack, "CURRENCY") {
					cur.Currency = strings.ToUpper(raw)
				}
			}
		case tag == "ACCTID":
			if err := setOnce(&stAccount, raw, "account"); err != nil {
				return nil, err
			}
		case tag == "CURDEF":
			if err := setOnce(&stCur, strings.ToUpper(raw), "currency"); err != nil {
				return nil, err
			}
		}
		i += nx
	}
	if cur != nil {
		return nil, parseErr("a transaction is not closed")
	}
	// A block left open (or identifiers outside any block) is still checked.
	if inBlock || stAccount != "" || stCur != "" {
		if err := ids.block(stAccount, stCur); err != nil {
			return nil, err
		}
	}
	ps.AccountID, ps.Currency = ids.account, ids.cur
	if err := finishStatement(ps, currency); err != nil {
		return nil, err
	}
	deriveKeys(ps.Txns)
	return ps, nil
}

func entityLike(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '&' {
			if j := strings.IndexByte(s[i:], ';'); j > 1 && j < 12 {
				return true
			}
		}
	}
	return false
}

// ---- CAMT.053 (ISO 20022 bank-to-customer statement) ---------------------------------------

// ParseCAMT053 reads a camt.053 document with encoding/xml in strict mode. A document type declaration is
// refused before parsing (so there is no entity to expand, internal or external), the file and every text
// value are size-capped, and nesting is capped. Only UTF-8 is accepted.
func ParseCAMT053(r io.Reader, currency string) (*ParsedStatement, error) {
	b, err := readStatement(r)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(bytes.ToUpper(b), []byte("<!DOCTYPE")) || bytes.Contains(bytes.ToUpper(b), []byte("<!ENTITY")) {
		return nil, parseErr("document type and entity declarations are not accepted")
	}
	exp, ok := CurrencyExponent(currency)
	if !ok {
		return nil, parseErr("unknown account currency")
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	dec.Entity = nil
	ps := &ParsedStatement{}
	var path []string
	var text strings.Builder
	var cur *ParsedTxn
	var credit *bool
	var amtCcy, acctSvcrRef, stAccount, stCur string
	var ids statementIDs
	inPath := func(suffix ...string) bool {
		if len(path) < len(suffix) {
			return false
		}
		for i, s := range suffix {
			if path[len(path)-len(suffix)+i] != s {
				return false
			}
		}
		return true
	}
	inEntry := func() bool { return cur != nil }
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, parseErr("the file is not well-formed XML")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(path) >= maxXMLDepth {
				return nil, parseErr("the statement is nested too deeply")
			}
			path = append(path, t.Name.Local)
			text.Reset()
			// Only a camt.053 statement is read. An account report (052) or notification (054) puts its entries
			// elsewhere, where neither the account they belong to nor their completeness would be checked.
			switch {
			case len(path) == 1 && (t.Name.Local != "Document" || (t.Name.Space != "" && !strings.Contains(t.Name.Space, "camt.053"))),
				len(path) == 2 && t.Name.Local != "BkToCstmrStmt":
				return nil, parseErr("not a camt.053 bank-to-customer statement (camt.052 and camt.054 are not accepted)")
			case t.Name.Local == "Ntry" && (len(path) != 4 || path[2] != "Stmt"):
				return nil, parseErr("an entry is outside a statement")
			}
			if t.Name.Local == "Stmt" {
				stAccount, stCur = "", ""
			}
			if t.Name.Local == "Ntry" {
				if len(ps.Txns) >= maxStatementRows {
					return nil, parseErr("the statement has more than %d transactions", maxStatementRows)
				}
				cur, credit, amtCcy, acctSvcrRef = &ParsedTxn{}, nil, "", ""
			}
			if t.Name.Local == "Amt" && inEntry() && len(path) > 0 {
				for _, a := range t.Attr {
					if a.Name.Local == "Ccy" {
						amtCcy = a.Value
					}
				}
			}
		case xml.CharData:
			if text.Len()+len(t) > maxFieldBytes*4 {
				return nil, parseErr("a field is longer than %d bytes", maxFieldBytes*4)
			}
			text.Write(t)
		case xml.EndElement:
			v := strings.TrimSpace(text.String())
			switch {
			case inPath("Stmt", "Acct", "Id", "IBAN"), inPath("Stmt", "Acct", "Id", "Othr", "Id"):
				if v == "" {
					break
				}
				if err := setOnce(&stAccount, v, "account"); err != nil {
					return nil, err
				}
			case inPath("Stmt", "Acct", "Ccy"):
				if err := setOnce(&stCur, v, "currency"); err != nil {
					return nil, err
				}
			case t.Name.Local == "Stmt":
				if err := ids.block(stAccount, stCur); err != nil {
					return nil, err
				}
			case !inEntry():
			case inPath("Ntry", "Amt"):
				if amtCcy != "" && amtCcy != currency {
					return nil, parseErr("an entry is in a currency other than the bank account's")
				}
				if cur.AmountMinor, err = parseDecimalMinor(v, exp, '.', 0); err != nil || cur.AmountMinor < 0 {
					return nil, parseErr("bad entry amount")
				}
			case inPath("Ntry", "CdtDtInd"):
				c := v == "CRDT"
				if v != "CRDT" && v != "DBIT" {
					return nil, parseErr("bad credit/debit indicator")
				}
				credit = &c
			case inPath("Ntry", "BookgDt", "Dt"), inPath("Ntry", "BookgDt", "DtTm"):
				if len(v) < 10 || !validDate(v[:10]) {
					return nil, parseErr("bad booking date")
				}
				cur.Date = v[:10]
			case inPath("Ntry", "AcctSvcrRef"):
				acctSvcrRef = v
			case inPath("Ntry", "AddtlNtryInf"):
				cur.Description = v
			case inPath("TxDtls", "Refs", "EndToEndId"):
				if cur.Reference == "" && v != "NOTPROVIDED" {
					cur.Reference = appendCapped("", v, maxReferenceBytes)
				}
			case inPath("RmtInf", "Ustrd"):
				cur.Reference = appendCapped(cur.Reference, v, maxReferenceBytes)
			case inPath("RltdPties", "Dbtr", "Nm"), inPath("RltdPties", "Cdtr", "Nm"):
				if cur.Counterparty == "" {
					cur.Counterparty = v
				}
			case t.Name.Local == "Ntry":
				if credit == nil || cur.Date == "" || cur.AmountMinor == 0 {
					return nil, parseErr("an entry lacks its amount, indicator or booking date")
				}
				if !*credit {
					cur.AmountMinor = -cur.AmountMinor
				}
				// NtryRef is unique at best inside one message (banks number entries 1, 2, 3 in every
				// statement) and is not used. AcctSvcrRef is kept, but see servicerKeys.
				cur.ServicerRef = acctSvcrRef
				ps.Txns = append(ps.Txns, *cur)
				cur = nil
			}
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
			text.Reset()
		case xml.Directive, xml.ProcInst:
			if d, ok := t.(xml.Directive); ok && len(d) > 0 {
				return nil, parseErr("document type and entity declarations are not accepted")
			}
		}
	}
	if len(path) != 0 || cur != nil {
		return nil, parseErr("the XML is truncated")
	}
	ps.AccountID, ps.Currency = ids.account, ids.cur
	if err := finishStatement(ps, currency); err != nil {
		return nil, err
	}
	servicerKeys(ps.Txns)
	deriveKeys(ps.Txns)
	return ps, nil
}

// servicerKeys keys the entries that carry the account servicer's reference. Banks reuse one reference for the
// entries of one operation (a payment and its fee, in one file or in different ones), so the reference alone is
// not an identity: the key is the reference with the booking date, signed amount and currency, plus the
// occurrence number among entries of the file that share all four. Descriptive text is left out, so the same
// entry worded differently in a later statement is still recognised.
func servicerKeys(txns []ParsedTxn) {
	count := map[string]int{}
	for i := range txns {
		t := &txns[i]
		if t.ServicerRef == "" {
			continue
		}
		base := strings.Join([]string{t.ServicerRef, t.Date, fmt.Sprint(t.AmountMinor), t.Currency}, "\x1f")
		count[base]++
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x1e%d", base, count[base])))
		t.Key = servicerKeyPrefix + hex.EncodeToString(sum[:16])
	}
}
