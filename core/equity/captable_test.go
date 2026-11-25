package equity

import (
	"math/rand"
	"testing"
)

func TestAllocatePPMAlwaysSumsToOneMillion(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 5000; i++ {
		n := 1 + r.Intn(40)
		vals := make([]Qty, n)
		var total Qty
		for j := range vals {
			switch r.Intn(4) {
			case 0:
				vals[j] = 0
			case 1:
				vals[j] = Qty(1 + r.Intn(10))
			default:
				vals[j] = Qty(r.Int63n(int64(MaxQty) / 100))
			}
			total += vals[j]
		}
		got := allocatePPM(vals)
		var sum int64
		for j, p := range got {
			sum += p
			if p < 0 || (vals[j] == 0 && p != 0) {
				t.Fatalf("bad part %d for %d in %v", p, vals[j], vals)
			}
			if total > 0 {
				exact := float64(vals[j]) / float64(total) * 1e6
				if d := float64(p) - exact; d > 1.0001 || d < -1.0001 {
					t.Fatalf("part %d too far from exact %.3f", p, exact)
				}
			}
		}
		if total > 0 && sum != 1_000_000 {
			t.Fatalf("parts sum to %d for %v", sum, vals)
		}
		if total == 0 && sum != 0 {
			t.Fatal("parts of nothing")
		}
	}
	// equal holders: the odd part goes to the earliest
	if got := allocatePPM([]Qty{1, 1, 1}); got[0] != 333334 || got[1] != 333333 || got[2] != 333333 {
		t.Fatalf("thirds = %v", got)
	}
}

func FuzzAllocatePPM(f *testing.F) {
	f.Add(int64(1), int64(1), int64(1))
	f.Add(int64(0), int64(10000), int64(99))
	f.Fuzz(func(t *testing.T, a, b, c int64) {
		vals := []Qty{Qty(a), Qty(b), Qty(c)}
		var total int64
		for _, v := range vals {
			if v < 0 || v > MaxQty {
				return
			}
			total += int64(v)
		}
		got := allocatePPM(vals)
		var sum int64
		for _, p := range got {
			sum += p
		}
		if total > 0 && sum != 1_000_000 || total == 0 && sum != 0 {
			t.Fatalf("sum %d for %v", sum, vals)
		}
	})
}

func TestCapTableViewsAsOfDates(t *testing.T) {
	e := newEnv(t).base()
	// 8,000,000 founder shares, 1,000,000 plan, grants of 100,000 (eve) approved 10 Mar
	_, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, Quantity: q(8_000_000), EffectiveDate: "2026-01-01"}, "", nil)
	e.must(err)
	id := e.active(e.eveStk.ID, q(100_000))
	basic := m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "", "basic", false))
	if basic.Totals.BasicShares != q(8_000_000) || len(basic.Holders) != 1 || basic.Holders[0].BasicPPM != 1_000_000 || basic.Holders[0].OptionsOutstanding != nil {
		t.Fatalf("basic = %+v", basic)
	}
	fd := m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "", "fully-diluted", false))
	if fd.Totals.OptionsOutstanding != q(100_000) || fd.Totals.PoolAvailable != q(900_000) || fd.Totals.FullyDiluted != q(9_000_000) {
		t.Fatalf("fd totals = %+v", fd.Totals)
	}
	var sum int64
	for _, h := range fd.Holders {
		sum += *h.FullyDilutedPPM
	}
	for _, p := range fd.Pool {
		sum += p.FullyDilutedPPM
	}
	if sum != 1_000_000 {
		t.Fatalf("fully diluted ownership sums to %d", sum)
	}
	if fd.Holders[0].Name != "" {
		t.Fatal("names appear without being asked for")
	}
	if len(fd.Warnings) != 0 {
		t.Fatalf("warnings = %v", fd.Warnings)
	}
	// before the grant existed: no options, the whole pool available
	past := m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "2026-03-09", "fully-diluted", false))
	if past.Totals.OptionsOutstanding != 0 || past.Totals.PoolAvailable != q(1_000_000) {
		t.Fatalf("9 Mar = %+v", past.Totals)
	}
	// before the plan was adopted: no pool
	if ancient := m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "2025-12-31", "fully-diluted", false)); ancient.Totals.PoolAvailable != 0 || ancient.Totals.FullyDiluted != 0 {
		t.Fatalf("2025 = %+v", ancient.Totals)
	}
	// terminated 2027-07-15 after 18 months: from then only the vested part is outstanding, until the window closes
	e.advance("2027-07-15")
	m(e.s.TerminateGrant(e.ctx, e.hr2, id, "2027-07-15", false))
	at := func(d string) Qty {
		return m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, d, "fully-diluted", false)).Totals.OptionsOutstanding
	}
	if at("2027-07-14") != q(100_000) {
		t.Fatalf("day before termination: %s", at("2027-07-14"))
	}
	if at("2027-07-15") != q(100_000)*18/48 {
		t.Fatalf("on termination: %s", at("2027-07-15"))
	}
	if at("2027-10-13") != q(100_000)*18/48 || at("2027-10-14") != 0 {
		t.Fatalf("around the window end: %s %s", at("2027-10-13"), at("2027-10-14"))
	}
	if _, err := e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "", "nonsense", false); err == nil {
		t.Fatal("bad view accepted")
	}
	// names need the identity permission too
	if _, err := e.s.GetCapTable(e.ctx, e.board, e.ent.ID, "", "basic", true); err != nil {
		t.Fatal(err)
	}
}

func TestCapTableWarnsWhenOverCommitted(t *testing.T) {
	e := newEnv(t).base()
	// class authorised 10M: issue 9.5M, plan 1M -> fully diluted exceeds the class
	_, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, Quantity: q(9_500_000), EffectiveDate: "2026-01-01"}, "", nil)
	e.must(err)
	fd := m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "", "fully-diluted", false))
	if len(fd.Warnings) == 0 {
		t.Fatal("no warning for options and pool beyond the authorised shares")
	}
}

// ---- round modelling ----------------------------------------------------------------------------

func TestModelRoundPlainPriced(t *testing.T) {
	r, err := ModelRound(RoundInput{Currency: "USD", PreMoneyMinor: 8_000_000_00, InvestmentMinor: 2_000_000_00, ExistingShares: q(10_000_000), ConversionDate: "2026-06-01"})
	if err != nil {
		t.Fatal(err)
	}
	// $8M pre / 10M shares = $0.80; $2M buys 2.5M shares; 2.5 / 12.5 = 20%
	if r.PricePerShare != "0.800000" || r.NewInvestorShares != q(2_500_000) || r.PostRoundShares != q(12_500_000) || r.NewInvestorPPM != 200_000 || r.PostMoneyMinor != 10_000_000_00 {
		t.Fatalf("result = %+v", r)
	}
	if !r.Illustrative || r.Notice == "" {
		t.Fatal("the model must say it is illustrative")
	}
	// an option pool top-up before the money dilutes existing holders only
	r, err = ModelRound(RoundInput{Currency: "USD", PreMoneyMinor: 8_000_000_00, InvestmentMinor: 2_000_000_00, PoolTopUp: q(2_000_000), ExistingShares: q(10_000_000), ConversionDate: "2026-06-01"})
	if err != nil {
		t.Fatal(err)
	}
	// price = 8M / 12M = 0.666667; 3M new shares of 15M = 20%; the top-up is 2M of 15M
	if r.PricePerShare != "0.666667" || r.NewInvestorPPM != 200_000 || r.PoolTopUpPPM != 133_333 {
		t.Fatalf("top-up result = %+v", r)
	}
}

func TestModelRoundConvertibles(t *testing.T) {
	in := RoundInput{Currency: "USD", PreMoneyMinor: 10_000_000_00, InvestmentMinor: 2_000_000_00, ExistingShares: q(10_000_000), ConversionDate: "2026-07-01",
		Instruments: []Instrument{
			{ID: "a", Kind: "safe_pre_money", PrincipalMinor: 500_000_00, Currency: "USD", CapMinor: 5_000_000_00},
			{ID: "b", Kind: "safe_pre_money", PrincipalMinor: 500_000_00, Currency: "USD", DiscountBP: 2000},
			{ID: "c", Kind: "convertible_note", PrincipalMinor: 1_000_000_00, Currency: "USD", InterestBP: 800, IssuedOn: "2025-07-01", CapMinor: 20_000_000_00},
		}}
	r, err := ModelRound(in)
	if err != nil {
		t.Fatal(err)
	}
	// P = 10M/10M = $1.00. a: cap price 5M/10M = 0.50 -> 1,000,000 shares. b: discount price 0.80 -> 625,000.
	// c: 1,000,000 + 8% for 365 days = 1,080,000 at min(0.80?no discount -> 1.00, cap 2.00) = 1.00 -> 1,080,000.
	byID := map[string]Conversion{}
	for _, c := range r.Conversions {
		byID[c.ID] = c
	}
	if byID["a"].Shares != q(1_000_000) || byID["a"].Method != "cap" || byID["a"].ConversionPrice != "0.500000" {
		t.Errorf("a = %+v", byID["a"])
	}
	if byID["b"].Shares != q(625_000) || byID["b"].Method != "discount" {
		t.Errorf("b = %+v", byID["b"])
	}
	if byID["c"].Shares != q(1_080_000) || byID["c"].Method != "round_price" || byID["c"].ConvertingMinor != 1_080_000_00 {
		t.Errorf("c = %+v", byID["c"])
	}
	if r.ConvertedShares != q(2_705_000) || r.PostRoundShares != r.PreRoundShares+r.ConvertedShares+r.NewInvestorShares {
		t.Errorf("totals = %+v", r)
	}
}

func TestModelRoundPostMoneySAFE(t *testing.T) {
	// One post-money SAFE: $1M at a $10M post-money cap owns exactly 10% of the capitalisation that includes it.
	in := RoundInput{Currency: "USD", PreMoneyMinor: 20_000_000_00, InvestmentMinor: 0, ExistingShares: q(9_000_000), ConversionDate: "2026-07-01",
		Instruments: []Instrument{{ID: "s", Kind: "safe_post_money", PrincipalMinor: 1_000_000_00, Currency: "USD", CapMinor: 10_000_000_00}}}
	r, err := ModelRound(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Conversions[0].Shares != q(1_000_000) || r.ConvertedPPM != 100_000 {
		t.Fatalf("post-money SAFE = %+v", r.Conversions[0])
	}
	// with a discount that beats the cap the instrument takes the discount shares
	in.Instruments[0].DiscountBP = 5000 // round price = 20M / 9M = 2.2222; half = 1.1111 -> 900,000 shares < cap 1,000,000, so cap still wins
	if r, err = ModelRound(in); err != nil || r.Conversions[0].Method != "cap" {
		t.Fatalf("cap should win: %v %+v", err, r)
	}
	in.PreMoneyMinor = 4_000_000_00 // price = 0.4444; half = 0.2222 -> 4,500,000 shares: the discount wins
	if r, err = ModelRound(in); err != nil || r.Conversions[0].Method != "discount" || r.Conversions[0].Shares != q(4_500_000) {
		t.Fatalf("discount should win: %v %+v", err, r)
	}
	// impossible: SAFEs that own more than 100%
	in.Instruments = []Instrument{{Kind: "safe_post_money", PrincipalMinor: 2_000_000_00, Currency: "USD", CapMinor: 1_000_000_00}}
	if _, err := ModelRound(in); err == nil {
		t.Fatal("principal above the cap accepted")
	}
}

func TestModelRoundRejectsBadInput(t *testing.T) {
	good := RoundInput{Currency: "USD", PreMoneyMinor: 100, InvestmentMinor: 10, ExistingShares: q(10), ConversionDate: "2026-01-01"}
	for name, mut := range map[string]func(*RoundInput){
		"currency":   func(r *RoundInput) { r.Currency = "XXX" },
		"pre money":  func(r *RoundInput) { r.PreMoneyMinor = 0 },
		"investment": func(r *RoundInput) { r.InvestmentMinor = -1 },
		"no shares":  func(r *RoundInput) { r.ExistingShares = 0 },
		"date":       func(r *RoundInput) { r.ConversionDate = "x" },
		"kind":       func(r *RoundInput) { r.Instruments = []Instrument{{Kind: "x", PrincipalMinor: 1, Currency: "USD"}} },
		"mixed cur": func(r *RoundInput) {
			r.Instruments = []Instrument{{Kind: "safe_pre_money", PrincipalMinor: 1, Currency: "EUR"}}
		},
		"discount 100": func(r *RoundInput) {
			r.Instruments = []Instrument{{Kind: "safe_pre_money", PrincipalMinor: 1, Currency: "USD", DiscountBP: 10000}}
		},
	} {
		in := good
		mut(&in)
		if _, err := ModelRound(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestModelRoundProperties(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	kinds := []string{"safe_pre_money", "safe_post_money", "convertible_note"}
	for i := 0; i < 2000; i++ {
		in := RoundInput{Currency: "USD", PreMoneyMinor: 1 + r.Int63n(1_000_000_000_00), InvestmentMinor: r.Int63n(100_000_000_00),
			ExistingShares: Qty(1+r.Int63n(100_000_000)) * QtyScale, PoolTopUp: Qty(r.Int63n(1_000_000)) * QtyScale, ConversionDate: "2026-06-01"}
		for j := 0; j < r.Intn(5); j++ {
			ins := Instrument{Kind: kinds[r.Intn(3)], PrincipalMinor: 1 + r.Int63n(5_000_000_00), Currency: "USD", DiscountBP: r.Intn(9000), InterestBP: r.Intn(1500), IssuedOn: "2025-01-01"}
			if r.Intn(2) == 0 {
				ins.CapMinor = 1 + r.Int63n(100_000_000_00)
			}
			in.Instruments = append(in.Instruments, ins)
		}
		res, err := ModelRound(in)
		if err != nil {
			continue // an impossible combination (for example SAFEs above their caps) is refused, never mis-computed
		}
		var conv Qty
		for _, c := range res.Conversions {
			if c.Shares < 0 {
				t.Fatalf("negative conversion: %+v", c)
			}
			conv += c.Shares
		}
		if conv != res.ConvertedShares || res.NewInvestorShares < 0 || res.PostRoundShares != in.ExistingShares+in.PoolTopUp+conv+res.NewInvestorShares {
			t.Fatalf("totals do not add up: %+v", res)
		}
		if sum := res.ExistingPPM + res.ConvertedPPM + res.NewInvestorPPM + res.PoolTopUpPPM; sum != 1_000_000 {
			t.Fatalf("ownership sums to %d: %+v", sum, res)
		}
	}
}

func FuzzModelRound(f *testing.F) {
	f.Add(int64(100_000_000), int64(20_000_000), int64(1_000_0000_0000), int64(5_000_000), int64(10_000_000), 2000, 0)
	f.Fuzz(func(t *testing.T, pre, inv, existing, principal, capm int64, disc, kind int) {
		in := RoundInput{Currency: "USD", PreMoneyMinor: pre, InvestmentMinor: inv, ExistingShares: Qty(existing), ConversionDate: "2026-06-01",
			Instruments: []Instrument{{Kind: []string{"safe_pre_money", "safe_post_money", "convertible_note"}[(kind%3+3)%3], PrincipalMinor: principal, Currency: "USD", CapMinor: capm, DiscountBP: disc}}}
		res, err := ModelRound(in) // must never panic
		if err != nil {
			return
		}
		if res.ConvertedShares < 0 || res.NewInvestorShares < 0 {
			t.Fatalf("negative shares: %+v", res)
		}
	})
}
