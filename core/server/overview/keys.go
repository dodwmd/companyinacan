package overview

// Figure describes one key of summary.v1 as can-core shows it. Everything a person reads (labels,
// links) comes from can-core: a module supplies numbers only.
type Figure struct {
	Key string
	// Module is the only module that may report the key.
	Module string
	// Attention puts a non-zero figure in the "needs attention" list.
	Attention bool
	// Money figures carry amounts by currency.
	Money bool
	// Unit is written after the number ("" for a plain count).
	Unit string
	// Path is the page of the module's own UI that shows the records ("" when it has none).
	Path string
}

// Figures are the keys of summary.v1 in display order (modules in the order shown). A test keeps
// them equal to schemas/capabilities/summary.v1.json.
var Figures = []Figure{
	{Key: "leads.new", Module: "forms", Attention: true},
	{Key: "invoices.overdue", Module: "finance", Attention: true, Money: true, Path: "/invoices"},
	{Key: "invoices.outstanding", Module: "finance", Money: true, Path: "/reports/aging"},
	{Key: "contracts.awaiting_signature", Module: "finance", Attention: true, Path: "/contracts"},
	{Key: "contracts.expiring", Module: "finance", Attention: true, Path: "/contracts"},
	{Key: "work.urgent_overdue", Module: "work", Attention: true, Path: "/"},
	{Key: "work.urgent_open", Module: "work", Path: "/"},
	{Key: "people.headcount", Module: "people", Path: "/people"},
	{Key: "people.off_today", Module: "people", Path: "/timeoff/calendar"},
	{Key: "requisitions.open", Module: "jobs", Path: "/jobs"},
	{Key: "applications.new", Module: "jobs", Attention: true, Path: "/jobs"},
	{Key: "interviews.due", Module: "jobs", Attention: true, Path: "/interviews"},
	{Key: "grants.pending_approval", Module: "equity", Attention: true, Path: "/company"},
	{Key: "vesting.upcoming", Module: "equity", Path: "/company"},
	{Key: "storage.company", Module: "files", Unit: "MiB", Path: "/admin/files"},
	{Key: "storage.managed", Module: "files", Unit: "MiB", Path: "/"},
	{Key: "files.retention_ending", Module: "files", Attention: true, Path: "/"},
	{Key: "filings.overdue", Module: "entity", Attention: true},
	{Key: "filings.upcoming", Module: "entity", Attention: true},
}

func figureOf(module, key string) (Figure, int, bool) {
	for i, f := range Figures {
		if f.Key == key && f.Module == module {
			return f, i, true
		}
	}
	return Figure{}, 0, false
}

// exponents are the currencies whose minor unit is not two decimals (ISO 4217); a test compares
// them with the finance module's table.
var exponents = map[string]int{
	"JPY": 0, "KRW": 0, "CLP": 0, "VND": 0, "ISK": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3,
}

// FormatMoney renders minor units as "AUD 1234.56".
func FormatMoney(currency string, minor int64) string {
	exp, ok := exponents[currency]
	if !ok {
		exp = 2
	}
	sign, u := "", uint64(minor) //nolint:gosec // two's complement negation below
	if minor < 0 {
		sign, u = "-", -u
	}
	s := utoa(u)
	if exp > 0 {
		for len(s) <= exp {
			s = "0" + s
		}
		s = s[:len(s)-exp] + "." + s[len(s)-exp:]
	}
	return currency + " " + sign + s
}
