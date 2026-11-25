package equity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// Round modelling ("what if we raise a round"). EVERYTHING HERE IS ILLUSTRATIVE: a documented
// simple formula over the recorded data, not a legal conversion calculation. Real SAFE and note
// terms (pro-rata rights, MFN, conversion mechanics, day counts, which securities count in the
// capitalisation) differ per instrument and per lawyer. The result is labelled illustrative and
// nothing computed here is ever written to the ledger.
//
// The formula (docs/equity.md):
//
//	S      = fully diluted shares before the round, plus any option-pool top-up, excluding
//	         the converting instruments below.
//	P      = pre_money / S                              (round price per share)
//	amount = principal (+ simple interest for a note: principal x rate x days / 365)
//	discount price  D = P x (1 - discount)              (D = P without a discount)
//	pre-money SAFE and convertible note:  price = min(D, cap / S);  shares = amount / price
//	post-money SAFE: the holder owns amount / cap of the capitalisation C that includes every
//	         post-money SAFE, so C = (S + shares of the other instruments) / (1 - sum of amount/cap)
//	         and shares = amount / cap x C; if the discount gives more shares (amount / D), that
//	         wins and the instrument leaves the cap group; repeated until stable.
//	new investor shares = investment / P
//
// Shares are rounded down to 4 decimal places; prices are shown to 6 decimal places.

// Instrument is a convertible instrument as modelling data.
type Instrument struct {
	ID             string `json:"id,omitempty"`
	StakeholderID  string `json:"stakeholder_id,omitempty"`
	Kind           string `json:"kind"` // safe_pre_money | safe_post_money | convertible_note
	PrincipalMinor int64  `json:"principal_minor"`
	Currency       string `json:"currency"`
	CapMinor       int64  `json:"cap_minor,omitempty"`   // valuation cap (0 = none)
	DiscountBP     int    `json:"discount_bp,omitempty"` // 2000 = 20% discount
	InterestBP     int    `json:"interest_bp,omitempty"` // notes: simple annual interest
	IssuedOn       string `json:"issued_on,omitempty"`
}

// RoundInput is a modelled round.
type RoundInput struct {
	Currency        string       `json:"currency"`
	PreMoneyMinor   int64        `json:"pre_money_minor"`
	InvestmentMinor int64        `json:"investment_minor"`
	PoolTopUp       Qty          `json:"pool_top_up"`
	ConversionDate  string       `json:"conversion_date"`
	Instruments     []Instrument `json:"instruments"`
	// ExistingShares is the fully diluted share count before the round. The service fills it
	// from the cap table; the pure function takes it as given.
	ExistingShares Qty `json:"existing_shares"`
}

// Conversion is one instrument's modelled conversion.
type Conversion struct {
	ID              string `json:"id,omitempty"`
	StakeholderID   string `json:"stakeholder_id,omitempty"`
	Kind            string `json:"kind"`
	ConvertingMinor int64  `json:"converting_minor"`
	Method          string `json:"method"` // cap | discount | round_price
	ConversionPrice string `json:"conversion_price"`
	Shares          Qty    `json:"shares"`
}

// RoundResult is the modelled outcome.
type RoundResult struct {
	Illustrative      bool         `json:"illustrative"`
	Notice            string       `json:"notice"`
	Currency          string       `json:"currency"`
	PricePerShare     string       `json:"price_per_share"`
	PreRoundShares    Qty          `json:"pre_round_shares"`
	PoolTopUp         Qty          `json:"pool_top_up"`
	Conversions       []Conversion `json:"conversions"`
	ConvertedShares   Qty          `json:"converted_shares"`
	NewInvestorShares Qty          `json:"new_investor_shares"`
	PostRoundShares   Qty          `json:"post_round_shares"`
	ExistingPPM       int64        `json:"existing_holders_ownership_ppm"`
	ConvertedPPM      int64        `json:"converted_instruments_ownership_ppm"`
	NewInvestorPPM    int64        `json:"new_investor_ownership_ppm"`
	PoolTopUpPPM      int64        `json:"pool_top_up_ownership_ppm"`
	PostMoneyMinor    int64        `json:"post_money_minor"`
}

// priceStr renders minor units per share (a rational) as a major-unit decimal with 6 places,
// half up.
func priceStr(r *big.Rat, cur string) string {
	exp := currencyExp[cur]
	scaled := new(big.Rat).Mul(r, big.NewRat(1_000_000, 1))
	scaled.Quo(scaled, new(big.Rat).SetInt(pow10(exp)))
	n := new(big.Int).Add(new(big.Int).Mul(scaled.Num(), big.NewInt(2)), scaled.Denom())
	n.Quo(n, new(big.Int).Mul(scaled.Denom(), big.NewInt(2)))
	s := n.String()
	for len(s) <= 6 {
		s = "0" + s
	}
	return s[:len(s)-6] + "." + s[len(s)-6:]
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// floorQty converts a rational number of shares to Qty, rounding down.
func floorQty(r *big.Rat) (Qty, error) {
	v := new(big.Int).Mul(r.Num(), big.NewInt(int64(QtyScale)))
	v.Quo(v, r.Denom())
	if !v.IsInt64() || v.Int64() < 0 || v.Int64() > int64(MaxQty)*4 {
		return 0, invalid("modelled share count is out of range")
	}
	return Qty(v.Int64()), nil
}

func ratQty(q Qty) *big.Rat { return big.NewRat(int64(q), int64(QtyScale)) }

func validInstrument(i int, ins Instrument, cur string) error {
	if ins.Currency != cur {
		return invalid("instrument %d: currency must be the round's currency", i)
	}
	switch ins.Kind {
	case "safe_pre_money", "safe_post_money", "convertible_note":
	default:
		return invalid("instrument %d: kind must be safe_pre_money, safe_post_money or convertible_note", i)
	}
	if ins.PrincipalMinor <= 0 || ins.PrincipalMinor > maxMinor || ins.CapMinor < 0 || ins.CapMinor > maxMinor || ins.DiscountBP < 0 || ins.DiscountBP >= 10000 || ins.InterestBP < 0 || ins.InterestBP > 100000 {
		return invalid("instrument %d: principal, cap, discount or interest out of range", i)
	}
	if ins.IssuedOn != "" {
		if _, err := parseDate(ins.IssuedOn); err != nil {
			return err
		}
	}
	return nil
}

// ModelRound computes the illustrative outcome. It is pure and total: bad input is an error,
// never a panic.
func ModelRound(in RoundInput) (*RoundResult, error) {
	if err := validCurrency(in.Currency); err != nil {
		return nil, err
	}
	if in.PreMoneyMinor <= 0 || in.PreMoneyMinor > maxMinor || in.InvestmentMinor < 0 || in.InvestmentMinor > maxMinor {
		return nil, invalid("pre_money_minor must be positive and investment_minor not negative (within range)")
	}
	if in.PoolTopUp < 0 || in.PoolTopUp > MaxQty || in.ExistingShares < 0 || in.ExistingShares > MaxQty {
		return nil, invalid("share counts out of range")
	}
	if len(in.Instruments) > 200 {
		return nil, invalid("at most 200 instruments")
	}
	conv, err := parseDate(in.ConversionDate)
	if err != nil {
		return nil, err
	}
	S := ratQty(in.ExistingShares + in.PoolTopUp)
	if S.Sign() <= 0 {
		return nil, invalid("there are no shares to price a round against")
	}
	P := new(big.Rat).Quo(new(big.Rat).SetInt64(in.PreMoneyMinor), S)
	res := &RoundResult{Illustrative: true, Notice: Notice, Currency: in.Currency, PricePerShare: priceStr(P, in.Currency),
		PreRoundShares: in.ExistingShares, PoolTopUp: in.PoolTopUp, Conversions: []Conversion{}}

	type item struct {
		in      Instrument
		amount  *big.Rat
		disc    *big.Rat // discount price, or P
		shares  *big.Rat
		method  string
		price   *big.Rat
		capGrp  bool // post-money SAFE converting at its cap
		capFrac *big.Rat
	}
	items := make([]*item, len(in.Instruments))
	others := new(big.Rat)
	for i, ins := range in.Instruments {
		if err := validInstrument(i, ins, in.Currency); err != nil {
			return nil, err
		}
		it := &item{in: ins, amount: new(big.Rat).SetInt64(ins.PrincipalMinor), method: "round_price"}
		if ins.Kind == "convertible_note" {
			if ins.InterestBP > 0 && ins.IssuedOn != "" {
				issued, err := parseDate(ins.IssuedOn)
				if err != nil {
					return nil, err
				}
				if days := int64(conv.Sub(issued).Hours() / 24); days > 0 {
					interest := new(big.Rat).SetInt64(ins.PrincipalMinor)
					interest.Mul(interest, big.NewRat(int64(ins.InterestBP)*days, 10000*365))
					it.amount.Add(it.amount, interest)
				}
			}
		}
		it.disc = new(big.Rat).Set(P)
		if ins.DiscountBP > 0 {
			it.disc.Mul(P, big.NewRat(int64(10000-ins.DiscountBP), 10000))
		}
		items[i] = it
		if ins.Kind == "safe_post_money" {
			if ins.CapMinor == 0 {
				// no cap: it behaves like a discounted conversion at the round price
				it.shares = new(big.Rat).Quo(it.amount, it.disc)
				it.price = it.disc
				if ins.DiscountBP > 0 {
					it.method = "discount"
				}
				others.Add(others, it.shares)
				continue
			}
			it.capGrp = true
			it.capFrac = new(big.Rat).Quo(it.amount, new(big.Rat).SetInt64(ins.CapMinor))
			continue
		}
		it.price = it.disc
		if ins.DiscountBP > 0 {
			it.method = "discount"
		}
		if ins.CapMinor > 0 {
			if cp := new(big.Rat).Quo(new(big.Rat).SetInt64(ins.CapMinor), S); cp.Cmp(it.price) < 0 {
				it.price, it.method = cp, "cap"
			}
		}
		it.shares = new(big.Rat).Quo(it.amount, it.price)
		others.Add(others, it.shares)
	}
	// post-money SAFEs: iterate until no instrument would rather take its discount
	for pass := 0; pass <= len(items); pass++ {
		F := new(big.Rat)
		for _, it := range items {
			if it.capGrp {
				F.Add(F, it.capFrac)
			}
		}
		if F.Sign() == 0 {
			break
		}
		if F.Cmp(big.NewRat(1, 1)) >= 0 {
			return nil, invalid("the post-money SAFEs' amounts are at or above their caps: no conversion exists")
		}
		C := new(big.Rat).Quo(new(big.Rat).Add(S, others), new(big.Rat).Sub(big.NewRat(1, 1), F))
		flipped := false
		for _, it := range items {
			if !it.capGrp {
				continue
			}
			capShares := new(big.Rat).Mul(it.capFrac, C)
			if it.in.DiscountBP > 0 {
				if dShares := new(big.Rat).Quo(it.amount, it.disc); dShares.Cmp(capShares) > 0 {
					it.capGrp, it.shares, it.price, it.method = false, dShares, it.disc, "discount"
					others.Add(others, dShares)
					flipped = true
					continue
				}
			}
			it.shares, it.method = capShares, "cap"
			it.price = new(big.Rat).Quo(it.amount, capShares)
		}
		if !flipped {
			break
		}
		for _, it := range items { // recompute from a clean slate next pass
			if it.capGrp {
				it.shares = nil
			}
		}
	}
	var convShares Qty
	totalShares := new(big.Rat)
	for _, it := range items {
		if it.shares == nil {
			return nil, errors.New("round model did not converge")
		}
		q, err := floorQty(it.shares)
		if err != nil {
			return nil, err
		}
		convShares += q
		totalShares.Add(totalShares, ratQty(q))
		cv := Conversion{ID: it.in.ID, StakeholderID: it.in.StakeholderID, Kind: it.in.Kind, ConversionPrice: priceStr(it.price, in.Currency), Shares: q, Method: it.method}
		cv.ConvertingMinor, _ = new(big.Int).Quo(it.amount.Num(), it.amount.Denom()).Int64(), 0
		res.Conversions = append(res.Conversions, cv)
	}
	res.ConvertedShares = convShares
	newShares := new(big.Rat).Quo(new(big.Rat).SetInt64(in.InvestmentMinor), P)
	if res.NewInvestorShares, err = floorQty(newShares); err != nil {
		return nil, err
	}
	res.PostRoundShares = in.ExistingShares + in.PoolTopUp + res.ConvertedShares + res.NewInvestorShares
	ppm := allocatePPM([]Qty{in.ExistingShares, res.ConvertedShares, res.NewInvestorShares, in.PoolTopUp})
	res.ExistingPPM, res.ConvertedPPM, res.NewInvestorPPM, res.PoolTopUpPPM = ppm[0], ppm[1], ppm[2], ppm[3]
	pm := new(big.Rat).Mul(P, ratQty(res.PostRoundShares))
	pmi := new(big.Int).Quo(pm.Num(), pm.Denom())
	if !pmi.IsInt64() {
		return nil, invalid("post-money value is out of range")
	}
	res.PostMoneyMinor = pmi.Int64()
	return res, nil
}

// ---- recorded convertibles --------------------------------------------------------------------

// ConvertibleInput records a convertible instrument as data (a SAFE or a note). It is never
// converted by this service: conversion is a legal event that is recorded as a ledger issuance
// once it has happened.
type ConvertibleInput struct {
	StakeholderID  string `json:"stakeholder_id"`
	Kind           string `json:"kind"`
	IssuedOn       string `json:"issued_on"`
	PrincipalMinor int64  `json:"principal_minor"`
	Currency       string `json:"currency"`
	CapMinor       int64  `json:"cap_minor"`
	DiscountBP     int    `json:"discount_bp"`
	InterestBP     int    `json:"interest_bp"`
}

// Convertible is a recorded instrument.
type Convertible struct {
	ID       string `json:"id"`
	EntityID string `json:"entity_id"`
	Status   string `json:"status"`
	ConvertibleInput
}

// CreateConvertible records an instrument. The terms are sealed. finance, board or admin.
func (s *Service) CreateConvertible(ctx context.Context, a Actor, entityID string, in ConvertibleInput) (*Convertible, error) {
	if err := s.authorize(ctx, a, ActConvertible, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	if err := validInstrument(0, Instrument{Kind: in.Kind, PrincipalMinor: in.PrincipalMinor, Currency: in.Currency, CapMinor: in.CapMinor, DiscountBP: in.DiscountBP, InterestBP: in.InterestBP, IssuedOn: in.IssuedOn}, in.Currency); err != nil {
		return nil, err
	}
	if _, err := parseDate(in.IssuedOn); err != nil {
		return nil, err
	}
	st, err := s.stakeholder(ctx, s.db, in.StakeholderID)
	if err != nil || st.EntityID != entityID {
		return nil, invalid("stakeholder_id is not a stakeholder of this entity")
	}
	c := &Convertible{ID: newID("cvt"), EntityID: entityID, Status: "outstanding", ConvertibleInput: in}
	terms, _ := json.Marshal(in)
	sealed, err := s.keys.Seal(string(terms), c.ID+":terms")
	if err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "convertible.create", "convertible:"+c.ID, []string{FieldConvertible}, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO convertibles (id, entity_id, stakeholder_id, kind, issued_on, sens_terms, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			c.ID, entityID, in.StakeholderID, in.Kind, in.IssuedOn, sealed, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) loadConvertibles(ctx context.Context, q querier, entityID string) ([]Convertible, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, status, sens_terms FROM convertibles WHERE entity_id = ? ORDER BY created_at, id LIMIT 1000`, entityID)
	if err != nil {
		return nil, err
	}
	type raw struct{ id, status, sealed string }
	var rs []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.id, &r.status, &r.sealed); err != nil {
			_ = rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	_ = rows.Close()
	out := []Convertible{}
	for _, r := range rs {
		plain, err := s.keys.Open(r.sealed, r.id+":terms")
		if err != nil {
			return nil, fmt.Errorf("convertible %s: %w", r.id, err)
		}
		c := Convertible{ID: r.id, EntityID: entityID, Status: r.status}
		if err := json.Unmarshal([]byte(plain), &c.ConvertibleInput); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ListConvertibles lists recorded instruments. The read is audited per instrument first.
func (s *Service) ListConvertibles(ctx context.Context, a Actor, entityID string) ([]Convertible, error) {
	if err := s.authorize(ctx, a, ActConvertible, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	cs, err := s.loadConvertibles(ctx, s.db, entityID)
	if err != nil {
		return nil, err
	}
	res := make([]string, len(cs))
	for i, c := range cs {
		res[i] = "convertible:" + c.ID
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldConvertible}); err != nil {
		return nil, err
	}
	return cs, nil
}

// WhatIfRequest asks for a modelled round on top of the entity's current cap table.
type WhatIfRequest struct {
	AsOf            string `json:"as_of"`
	PreMoneyMinor   int64  `json:"pre_money_minor"`
	InvestmentMinor int64  `json:"investment_minor"`
	PoolTopUp       Qty    `json:"pool_top_up"`
	ConversionDate  string `json:"conversion_date"`
	// UseRecorded includes the entity's outstanding recorded convertibles; Extra adds
	// hypothetical ones.
	UseRecorded bool         `json:"use_recorded_convertibles"`
	Extra       []Instrument `json:"extra_instruments"`
}

// WhatIf models a round against the fully diluted cap table. Nothing is stored. Reads holdings
// and convertible terms, so it is audited first. finance, board or admin.
func (s *Service) WhatIf(ctx context.Context, a Actor, entityID string, req WhatIfRequest) (*RoundResult, error) {
	if err := s.authorize(ctx, a, ActWhatIf, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	ent, err := s.entity(ctx, s.db, entityID)
	if err != nil {
		return nil, err
	}
	if req.AsOf == "" {
		req.AsOf = s.today()
	}
	if req.ConversionDate == "" {
		req.ConversionDate = req.AsOf
	}
	if _, err := parseDate(req.AsOf); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	ct, _, err := s.capTable(ctx, s.db, entityID, req.AsOf, true)
	if err != nil {
		return nil, err
	}
	ins := append([]Instrument{}, req.Extra...)
	fields := []string{FieldHoldings}
	res := []string{"captable:" + entityID + "@" + req.AsOf}
	if req.UseRecorded {
		cs, err := s.loadConvertibles(ctx, s.db, entityID)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if c.Status != "outstanding" {
				continue
			}
			ins = append(ins, Instrument{ID: c.ID, StakeholderID: c.StakeholderID, Kind: c.Kind, PrincipalMinor: c.PrincipalMinor, Currency: c.Currency,
				CapMinor: c.CapMinor, DiscountBP: c.DiscountBP, InterestBP: c.InterestBP, IssuedOn: c.IssuedOn})
			res = append(res, "convertible:"+c.ID)
		}
		fields = append(fields, FieldConvertible)
	}
	if err := s.auditReadMany(ctx, a, "whatif", res, fields); err != nil {
		return nil, err
	}
	return ModelRound(RoundInput{Currency: ent.Currency, PreMoneyMinor: req.PreMoneyMinor, InvestmentMinor: req.InvestmentMinor, PoolTopUp: req.PoolTopUp,
		ConversionDate: req.ConversionDate, Instruments: ins, ExistingShares: ct.Totals.FullyDiluted})
}
