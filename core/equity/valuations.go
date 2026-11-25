package equity

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Valuation is a 409A (or other) valuation record kept as DATA. The service does not compute a
// valuation, check that a grant's strike is at or above fair market value, or enforce any
// expiry: those rules are jurisdiction-specific and belong to counsel (docs/equity.md).
type Valuation struct {
	ID            string `json:"id"`
	EntityID      string `json:"entity_id"`
	ClassID       string `json:"class_id,omitempty"`
	Kind          string `json:"kind"`
	EffectiveDate string `json:"effective_date"`
	ExpiresOn     string `json:"expires_on,omitempty"`
	PriceMinor    int64  `json:"price_minor"`
	Currency      string `json:"currency"`
	Notes         string `json:"notes,omitempty"`
}

// ValuationInput records a valuation.
type ValuationInput struct {
	ClassID       string `json:"class_id"`
	Kind          string `json:"kind"`
	EffectiveDate string `json:"effective_date"`
	ExpiresOn     string `json:"expires_on"`
	PriceMinor    int64  `json:"price_minor"`
	Currency      string `json:"currency"`
	Notes         string `json:"notes"`
}

// CreateValuation appends a valuation record. The price and notes are sealed. finance, board or admin.
func (s *Service) CreateValuation(ctx context.Context, a Actor, entityID string, in ValuationInput) (*Valuation, error) {
	if err := s.authorize(ctx, a, ActValuation, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	ent, err := s.entity(ctx, s.db, entityID)
	if err != nil {
		return nil, err
	}
	switch in.Kind {
	case "409a", "independent", "board", "other":
	default:
		return nil, invalid("kind must be 409a, independent, board or other")
	}
	eff, err := parseDate(in.EffectiveDate)
	if err != nil {
		return nil, err
	}
	if in.ExpiresOn != "" {
		exp, err := parseDate(in.ExpiresOn)
		if err != nil {
			return nil, err
		}
		if !exp.After(eff) {
			return nil, invalid("expires_on must be after effective_date")
		}
	}
	if err := validMinor("price_minor", in.PriceMinor); err != nil {
		return nil, err
	}
	if in.Currency != ent.Currency {
		return nil, invalid("currency must be the entity's currency (%s)", ent.Currency)
	}
	if in.ClassID != "" {
		c, err := s.getClass(ctx, s.db, in.ClassID)
		if err != nil || c.EntityID != entityID {
			return nil, invalid("class_id is not a share class of this entity")
		}
	}
	if err := validText("notes", in.Notes, 2000, false); err != nil {
		return nil, err
	}
	v := &Valuation{ID: newID("val"), EntityID: entityID, ClassID: in.ClassID, Kind: in.Kind, EffectiveDate: in.EffectiveDate, ExpiresOn: in.ExpiresOn,
		PriceMinor: in.PriceMinor, Currency: in.Currency, Notes: in.Notes}
	fmv, _ := json.Marshal(map[string]any{"price_minor": in.PriceMinor, "currency": in.Currency})
	sealed, err := s.keys.Seal(string(fmv), v.ID+":fmv")
	if err != nil {
		return nil, err
	}
	var notes any
	if in.Notes != "" {
		if notes, err = s.keys.Seal(in.Notes, v.ID+":notes"); err != nil {
			return nil, err
		}
	}
	err = s.tx(ctx, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO valuations (id, entity_id, class_id, kind, effective_date, expires_on, sens_fmv, sens_notes, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			v.ID, entityID, nullStr(in.ClassID), in.Kind, in.EffectiveDate, in.ExpiresOn, sealed, notes, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		return nil, err
	}
	return v, s.auditWrite(ctx, a, "valuation.create", "valuation:"+v.ID, []string{FieldValuation})
}

// ListValuations lists an entity's valuation records, newest first. Audited per record before return.
func (s *Service) ListValuations(ctx context.Context, a Actor, entityID string) ([]Valuation, error) {
	if err := s.authorize(ctx, a, ActValuation, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, class_id, kind, effective_date, expires_on, sens_fmv, sens_notes FROM valuations WHERE entity_id = ? ORDER BY effective_date DESC, created_at DESC LIMIT 200`, entityID)
	if err != nil {
		return nil, err
	}
	type raw struct {
		v          Valuation
		fmv, notes sql.NullString
	}
	var rs []raw
	for rows.Next() {
		var r raw
		var cls sql.NullString
		if err := rows.Scan(&r.v.ID, &cls, &r.v.Kind, &r.v.EffectiveDate, &r.v.ExpiresOn, &r.fmv, &r.notes); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.v.EntityID, r.v.ClassID = entityID, cls.String
		rs = append(rs, r)
	}
	_ = rows.Close()
	res := make([]string, len(rs))
	for i, r := range rs {
		res[i] = "valuation:" + r.v.ID
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldValuation}); err != nil {
		return nil, err
	}
	out := []Valuation{}
	for _, r := range rs {
		if r.fmv.Valid {
			plain, err := s.keys.Open(r.fmv.String, r.v.ID+":fmv")
			if err != nil {
				return nil, err
			}
			var m struct {
				PriceMinor int64  `json:"price_minor"`
				Currency   string `json:"currency"`
			}
			if err := json.Unmarshal([]byte(plain), &m); err != nil {
				return nil, err
			}
			r.v.PriceMinor, r.v.Currency = m.PriceMinor, m.Currency
		}
		if r.notes.Valid {
			n, err := s.keys.Open(r.notes.String, r.v.ID+":notes")
			if err != nil {
				return nil, err
			}
			r.v.Notes = n
		}
		out = append(out, r.v)
	}
	return out, nil
}
