package finance

import "testing"

func TestCalcLineRounding(t *testing.T) {
	cases := []struct {
		name                                 string
		qtyMilli, unit, discBP, ratePPM      int64
		gross, discount, taxable, tax, total int64
	}{
		{"plain", 2000, 1000, 0, 0, 2000, 0, 2000, 0, 2000},
		{"half rounds away from zero", 1500, 333, 0, 0, 500, 0, 500, 0, 500}, // 499.5 -> 500
		{"just under half", 1000, 1, 0, 0, 1, 0, 1, 0, 1},
		{"tax 20pc rounds down", 1000, 1001, 0, 200000, 1001, 0, 1001, 200, 1201}, // 200.2
		{"tax half up", 1000, 5, 0, 100000, 5, 0, 5, 1, 6},                        // 0.5 -> 1
		{"discount then tax", 1000, 10000, 1000, 200000, 10000, 1000, 9000, 1800, 10800},
		{"fractional quantity", 250, 999, 0, 0, 250, 0, 250, 0, 250},  // 249.75 -> 250
		{"discount rounding", 1000, 105, 3333, 0, 105, 35, 70, 0, 70}, // 34.9965 -> 35
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := CalcLine(c.qtyMilli, c.unit, c.discBP, c.ratePPM)
			if err != nil {
				t.Fatal(err)
			}
			if r.Gross != c.gross || r.Discount != c.discount || r.Taxable != c.taxable || r.Tax != c.tax || r.Total != c.total {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestCalcLineRejects(t *testing.T) {
	bad := [][4]int64{{0, 100, 0, 0}, {-1, 100, 0, 0}, {1000, -1, 0, 0}, {1000, 100, 10001, 0}, {1000, 100, -1, 0}, {1000, 100, 0, 1000001}, {1 << 40, 1 << 40, 0, 0}}
	for _, b := range bad {
		if _, err := CalcLine(b[0], b[1], b[2], b[3]); err == nil {
			t.Errorf("CalcLine%v should be rejected", b)
		}
	}
}

func TestParseFXAndConvert(t *testing.T) {
	good := map[string]int64{"1": 1e8, "1.0850": 108500000, "150.5": 15050000000, "0.00000001": 1}
	for in, want := range good {
		got, err := ParseFX(in)
		if err != nil || got != want {
			t.Errorf("ParseFX(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0.0", "-1", "1.123456789", "abc", "1e3", "1,5", ".5", "5.", "1000001", " 1"} {
		if _, err := ParseFX(in); err == nil {
			t.Errorf("ParseFX(%q) should fail", in)
		}
	}
	if FormatFX(108500000) != "1.085" || FormatFX(1e8) != "1" {
		t.Errorf("FormatFX: %s %s", FormatFX(108500000), FormatFX(1e8))
	}
	// USD 100.00 at 150.5 JPY per USD = 15050 JPY (exponent 0).
	if got := ConvertMinor(10000, 2, 0, 15050000000); got != 15050 {
		t.Errorf("USD->JPY = %d", got)
	}
	// JPY 1000 at 0.0066 USD = 6.60 USD = 660 minor.
	if got := ConvertMinor(1000, 0, 2, 660000); got != 660 {
		t.Errorf("JPY->USD = %d", got)
	}
	// KWD (exp 3) 1.000 at 3.25 USD = 3.25 USD = 325.
	if got := ConvertMinor(1000, 3, 2, 325000000); got != 325 {
		t.Errorf("KWD->USD = %d", got)
	}
	// half rounds away: 1 minor at 0.5 -> 0.5 -> 1
	if got := ConvertMinor(1, 2, 2, 50000000); got != 1 {
		t.Errorf("half = %d", got)
	}
}

func TestCurrencyTable(t *testing.T) {
	if e, ok := CurrencyExponent("JPY"); !ok || e != 0 {
		t.Error("JPY exponent")
	}
	if e, ok := CurrencyExponent("KWD"); !ok || e != 3 {
		t.Error("KWD exponent")
	}
	for _, c := range []string{"usd", "XXX", "", "US", "USDD"} {
		if _, ok := CurrencyExponent(c); ok {
			t.Errorf("%q must be rejected", c)
		}
	}
	if FormatMinor(123456, "USD") != "1234.56" || FormatMinor(-5, "USD") != "-0.05" || FormatMinor(1500, "JPY") != "1500" || FormatMinor(1234, "KWD") != "1.234" {
		t.Error("FormatMinor")
	}
}

func TestAddMinorOverflow(t *testing.T) {
	if _, err := AddMinor(1<<62, 1<<62); err == nil {
		t.Error("overflow must be an error")
	}
	if v, err := AddMinor(1, 2); err != nil || v != 3 {
		t.Error("add")
	}
}
