package finance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Contract statuses.
const (
	CtDraft      = "draft"
	CtSent       = "sent"
	CtSigned     = "signed"
	CtActive     = "active"
	CtExpired    = "expired"
	CtTerminated = "terminated"
)

var placeholderRE = regexp.MustCompile(`\{\{([a-z][a-z0-9_]{0,39})\}\}`)

// RenderTemplate fills {{name}} placeholders. It is deliberately not a template language: a
// single pass of plain substitution, no expressions, no includes, and a value that itself
// contains {{x}} is not expanded again. Every placeholder must have a value and every value
// must be used, so a typo cannot silently ship a half-filled contract.
func RenderTemplate(body string, vars map[string]string) (string, error) {
	want := placeholders_(body)
	var p problems
	for _, n := range want {
		if _, ok := vars[n]; !ok {
			p.add("variables."+n, "missing")
		}
	}
	for n, v := range vars {
		if !varRE.MatchString(n) {
			p.add("variables."+n, "not a valid variable name")
		} else if !slices.Contains(want, n) {
			p.add("variables."+n, "the template has no {{%s}} placeholder", n)
		}
		if !validText(v, 2000) {
			p.add("variables."+n, "at most 2000 characters")
		}
	}
	if err := p.err(); err != nil {
		return "", err
	}
	return placeholderRE.ReplaceAllStringFunc(body, func(m string) string {
		return vars[m[2:len(m)-2]]
	}), nil
}

func placeholders_(body string) []string {
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(body, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

func hashBody(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// ---- templates --------------------------------------------------------------------------

// ContractTemplate is a reusable body with {{variables}}.
type ContractTemplate struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Body      string   `json:"body"`
	Variables []string `json:"variables"`
	Version   int64    `json:"version"`
}

// NewTemplate is the body of a template create.
type NewTemplate struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

func scanTemplate(sc scanner) (*ContractTemplate, error) {
	var t ContractTemplate
	var vars string
	if err := sc.Scan(&t.ID, &t.Name, &t.Body, &vars, &t.Version); err != nil {
		return nil, err
	}
	return &t, json.Unmarshal([]byte(vars), &t.Variables)
}

const tmplCols = `id, name, body, variables, version`

// CreateTemplate adds a template; its variables are the placeholders in the body.
func (s *Store) CreateTemplate(ctx context.Context, a *Principal, in NewTemplate) (*ContractTemplate, error) {
	var p problems
	if !oneLineText(in.Name, maxName) || strings.TrimSpace(in.Name) == "" {
		p.add("name", "required, one line")
	}
	if strings.TrimSpace(in.Body) == "" || !validText(in.Body, maxBody) {
		p.add("body", "required, at most %d characters", maxBody)
	}
	vars := placeholders_(in.Body)
	if len(vars) > maxVars {
		p.add("body", "at most %d variables", maxVars)
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	vj, _ := json.Marshal(vars)
	var out *ContractTemplate
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := ts(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO contract_templates (name, body, variables, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, in.Name, in.Body, string(vj), a.Subject, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%w: template name exists", ErrConflict)
			}
			return err
		}
		id, _ := res.LastInsertId()
		out, err = scanTemplate(tx.QueryRowContext(ctx, `SELECT `+tmplCols+` FROM contract_templates WHERE id = ?`, id))
		return err
	})
	return out, err
}

// ListTemplates lists templates.
func (s *Store) ListTemplates(ctx context.Context) ([]ContractTemplate, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+tmplCols+` FROM contract_templates ORDER BY name LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ContractTemplate{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// ---- contracts --------------------------------------------------------------------------

// ContractVersion is one immutable rendering of a contract's text.
type ContractVersion struct {
	Version   int64             `json:"version"`
	Body      string            `json:"body"`
	Variables map[string]string `json:"variables"`
	SHA256    string            `json:"sha256"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
}

// Contract is the API view of a contract.
type Contract struct {
	ID             int64      `json:"id"`
	PartyID        int64      `json:"party_id"`
	TemplateID     *int64     `json:"template_id"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	CurrentVersion int64      `json:"current_version"`
	SHA256         string     `json:"sha256"`
	StartDate      *string    `json:"start_date"`
	EndDate        *string    `json:"end_date"`
	RenewalDate    *string    `json:"renewal_date"`
	ReminderDays   int        `json:"reminder_days"`
	ESignProvider  string     `json:"esign_provider"`
	SignedAt       *time.Time `json:"signed_at"`
	SignedSHA256   string     `json:"signed_sha256"`
	TerminatedAt   *time.Time `json:"terminated_at"`
	CreatedBy      string     `json:"created_by"`
	Version        int64      `json:"version"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	updatedBy      string
	esignID        string
}

// NewContract is the body of a contract create. Give a template and its variables, or a body.
type NewContract struct {
	PartyID      int64             `json:"party_id"`
	Title        string            `json:"title"`
	TemplateID   *int64            `json:"template_id"`
	Body         string            `json:"body"`
	Variables    map[string]string `json:"variables"`
	StartDate    *string           `json:"start_date"`
	EndDate      *string           `json:"end_date"`
	RenewalDate  *string           `json:"renewal_date"`
	ReminderDays *int              `json:"reminder_days"`
}

// ContractDates changes the schedule metadata, which may change while a contract is live;
// the text cannot.
type ContractDates struct {
	StartDate    Opt[string] `json:"start_date"`
	EndDate      Opt[string] `json:"end_date"`
	RenewalDate  Opt[string] `json:"renewal_date"`
	ReminderDays *int        `json:"reminder_days"`
}

const ctCols = `c.id, c.party_id, c.template_id, c.title, c.status, c.current_version, v.sha256, c.start_date, c.end_date, c.renewal_date, c.reminder_days, c.esign_provider, c.esign_id, c.signed_at, c.signed_sha256, c.terminated_at, c.created_by, c.updated_by, c.version, c.created_at, c.updated_at`
const ctFrom = ` FROM contracts c JOIN contract_versions v ON v.contract_id = c.id AND v.version = c.current_version`

func scanContract(sc scanner) (*Contract, error) {
	var c Contract
	var tmpl sql.NullInt64
	var st, en, rn sql.NullString
	var signed, term sql.NullInt64
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.PartyID, &tmpl, &c.Title, &c.Status, &c.CurrentVersion, &c.SHA256, &st, &en, &rn, &c.ReminderDays, &c.ESignProvider, &c.esignID, &signed, &c.SignedSHA256, &term, &c.CreatedBy, &c.updatedBy, &c.Version, &created, &updated); err != nil {
		return nil, err
	}
	if tmpl.Valid {
		c.TemplateID = &tmpl.Int64
	}
	c.StartDate, c.EndDate, c.RenewalDate = nullStr(st), nullStr(en), nullStr(rn)
	c.SignedAt, c.TerminatedAt = nullTime(signed), nullTime(term)
	c.CreatedAt, c.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &c, nil
}

func optDate(p *problems, f string, v *string) any {
	if v == nil || *v == "" {
		return nil
	}
	if !validDate(*v) {
		p.add(f, "use YYYY-MM-DD")
		return nil
	}
	return *v
}

func (s *Store) loadContract(ctx context.Context, q dbq, a *Principal, id int64) (*Contract, error) {
	var w where
	w.and("(c.id = ?)", id)
	cl, args := partyClause(a, "c.party_id")
	w.scope(cl, args)
	c, err := scanContract(q.QueryRowContext(ctx, `SELECT `+ctCols+ctFrom+` WHERE `+w.sql(), w.args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// CreateContract creates a draft contract with version 1.
func (s *Store) CreateContract(ctx context.Context, a *Principal, in NewContract) (*Contract, error) {
	var p problems
	if !oneLineText(in.Title, maxName) || strings.TrimSpace(in.Title) == "" {
		p.add("title", "required, one line")
	}
	start := optDate(&p, "start_date", in.StartDate)
	end := optDate(&p, "end_date", in.EndDate)
	renew := optDate(&p, "renewal_date", in.RenewalDate)
	days := 30
	if in.ReminderDays != nil {
		days = *in.ReminderDays
	}
	if days < 0 || days > 365 {
		p.add("reminder_days", "between 0 and 365")
	}
	if start != nil && end != nil && end.(string) < start.(string) {
		p.add("end_date", "must not be before start_date")
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	var out *Contract
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		party, err := s.loadPartyRow(ctx, tx, a, in.PartyID)
		if err != nil {
			return err
		}
		if party.Erased {
			return invalid("party_id", "the party has been erased")
		}
		body, vars := in.Body, in.Variables
		if in.TemplateID != nil {
			t, err := scanTemplate(tx.QueryRowContext(ctx, `SELECT `+tmplCols+` FROM contract_templates WHERE id = ?`, *in.TemplateID))
			if err != nil {
				return invalid("template_id", "no such template")
			}
			if in.Body != "" {
				return invalid("body", "give a template or a body, not both")
			}
			body = t.Body
		}
		if strings.TrimSpace(body) == "" || !validText(body, maxBody) {
			return invalid("body", "give a template_id or a body of at most %d characters", maxBody)
		}
		rendered, err := RenderTemplate(body, vars)
		if err != nil {
			return err
		}
		now := ts(s.now())
		var tmpl any
		if in.TemplateID != nil {
			tmpl = *in.TemplateID
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO contracts (party_id, template_id, title, status, start_date, end_date, renewal_date, reminder_days, created_by, updated_by, created_at, updated_at) VALUES (?, ?, ?, 'draft', ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.PartyID, tmpl, in.Title, start, end, renew, days, a.Subject, a.Subject, now, now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if err := s.insertVersion(ctx, tx, id, 1, rendered, vars, a.Subject); err != nil {
			return err
		}
		out, err = s.loadContract(ctx, tx, a, id)
		return err
	})
	return out, err
}

func (s *Store) insertVersion(ctx context.Context, tx dbq, id, version int64, body string, vars map[string]string, by string) error {
	if vars == nil {
		vars = map[string]string{}
	}
	vj, _ := json.Marshal(vars)
	_, err := tx.ExecContext(ctx, `INSERT INTO contract_versions (contract_id, version, body, variables, sha256, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, version, body, string(vj), hashBody(body), by, ts(s.now()))
	return err
}

// GetContract returns a contract in scope.
func (s *Store) GetContract(ctx context.Context, a *Principal, id int64) (*Contract, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	return s.loadContract(rctx, s.rdb, a, id)
}

// ListContracts lists contracts in scope, newest first.
func (s *Store) ListContracts(ctx context.Context, a *Principal, status string, partyID, before int64, limit int) ([]Contract, int64, error) {
	limit = clampLimit(limit)
	var w where
	cl, args := partyClause(a, "c.party_id")
	w.scope(cl, args)
	if before > 0 {
		w.and("(c.id < ?)", before)
	}
	if status != "" {
		w.and("(c.status = ?)", status)
	}
	if partyID > 0 {
		w.and("(c.party_id = ?)", partyID)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+ctCols+ctFrom+` WHERE `+w.sql()+` ORDER BY c.id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []Contract{}
	for rows.Next() {
		c, err := scanContract(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *c)
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, rows.Err()
}

// GetVersion returns one version of a contract's text.
func (s *Store) GetVersion(ctx context.Context, a *Principal, id, version int64) (*ContractVersion, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	if _, err := s.loadContract(rctx, s.rdb, a, id); err != nil {
		return nil, err
	}
	var v ContractVersion
	var vars string
	var c int64
	err := s.rdb.QueryRowContext(rctx, `SELECT version, body, variables, sha256, created_by, created_at FROM contract_versions WHERE contract_id = ? AND version = ?`, id, version).Scan(&v.Version, &v.Body, &vars, &v.SHA256, &v.CreatedBy, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt = fromUnix(c)
	return &v, json.Unmarshal([]byte(vars), &v.Variables)
}

// NewVersion adds a version of the text. It is allowed while the contract is a draft or sent
// (a sent contract falls back to draft: the signature request is void, since the text the
// signers saw is no longer current). A signed contract's text never changes: terminate it
// and draw up a new one.
func (s *Store) NewVersion(ctx context.Context, a *Principal, id int64, ifMatch *int64, body string, vars map[string]string) (*Contract, error) {
	var out *Contract
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := s.loadContract(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != c.Version {
			return ErrVersionMismatch
		}
		if c.Status != CtDraft && c.Status != CtSent {
			return fmt.Errorf("%w: a %s contract's text cannot change; terminate it and create a new contract", ErrConflict, c.Status)
		}
		tmplBody := body
		if tmplBody == "" && c.TemplateID != nil {
			if err := tx.QueryRowContext(ctx, `SELECT body FROM contract_templates WHERE id = ?`, *c.TemplateID).Scan(&tmplBody); err != nil {
				return err
			}
		}
		if strings.TrimSpace(tmplBody) == "" || !validText(tmplBody, maxBody) {
			return invalid("body", "required (a template-based contract may omit it to re-render the template)")
		}
		rendered, err := RenderTemplate(tmplBody, vars)
		if err != nil {
			return err
		}
		if err := s.insertVersion(ctx, tx, id, c.CurrentVersion+1, rendered, vars, a.Subject); err != nil {
			return err
		}
		// Links to the old text die with it, in the same transaction.
		if _, err := s.revokePending(ctx, tx, a.Subject, id, "new_version"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET current_version = current_version + 1, status = 'draft', esign_provider = '', esign_id = '', version = version + 1, updated_by = ?, updated_at = ? WHERE id = ?`, a.Subject, ts(s.now()), id); err != nil {
			return err
		}
		out, err = s.loadContract(ctx, tx, a, id)
		return err
	})
	return out, err
}

// SendRequest asks for signatures. Manual means the signing happens outside this system
// (wet ink, another tool) and is recorded later with RecordSignature.
type SendRequest struct {
	Signers []string `json:"signers"`
	Manual  bool     `json:"manual"`
}

// SendContract moves a draft to sent and, unless manual, asks the e-signature service to
// collect signatures on the current text, identified by its hash. Signer addresses go to
// the service and are not stored here.
func (s *Store) SendContract(ctx context.Context, a *Principal, es ESigner, id int64, ifMatch *int64, in SendRequest) (*Contract, error) {
	if !in.Manual {
		if len(in.Signers) == 0 || len(in.Signers) > 10 {
			return nil, invalid("signers", "give between 1 and 10 addresses, or manual: true")
		}
		for _, e := range in.Signers {
			if e == "" || !validEmail(e) {
				return nil, invalid("signers", "not a valid email address")
			}
		}
		if es == nil {
			return nil, fmt.Errorf("%w: no e-signature provider is configured; send with manual: true", ErrConflict)
		}
	}
	var out *Contract
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := s.loadContract(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != c.Version {
			return ErrVersionMismatch
		}
		if c.Status != CtDraft {
			return fmt.Errorf("%w: only a draft can be sent (it is %s)", ErrConflict, c.Status)
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM contract_versions WHERE contract_id = ? AND version = ?`, id, c.CurrentVersion).Scan(&body); err != nil {
			return err
		}
		prov, ext := "manual", ""
		if !in.Manual {
			if ext, err = es.Request(ctx, SignRequest{ContractID: id, Title: c.Title, Body: body, SHA256: c.SHA256, Signers: in.Signers}); err != nil {
				return fmt.Errorf("e-signature request failed: %w", err)
			}
			prov = es.Name()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = 'sent', sent_once = 1, esign_provider = ?, esign_id = ?, version = version + 1, updated_by = ?, updated_at = ? WHERE id = ?`, prov, ext, a.Subject, ts(s.now()), id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "contract.send", fmt.Sprint(id), map[string]any{"provider": prov, "sha256": c.SHA256, "signers": len(in.Signers)}); err != nil {
			return err
		}
		out, err = s.loadContract(ctx, tx, a, id)
		return err
	})
	return out, err
}

type contractSignedData struct {
	ContractID int64     `json:"contract_id"`
	PartyID    int64     `json:"party_id"`
	Version    int64     `json:"version"`
	SHA256     string    `json:"sha256"`
	SignedAt   time.Time `json:"signed_at"`
}

type contractSummaryData struct {
	Count int `json:"count"`
}

type contractExpiringData struct {
	ContractID  int64  `json:"contract_id"`
	PartyID     int64  `json:"party_id"`
	RenewalDate string `json:"renewal_date"`
	DaysLeft    int    `json:"days_left"`
}

// RecordSignature records that a sent contract was signed. sha256 is the hash of the document
// the signers actually signed (from the e-signature service, or from the manual record); it
// must equal the hash of the current text, otherwise the signature is for something else and
// is refused. a is nil for a verified provider webhook.
func (s *Store) RecordSignature(ctx context.Context, a *Principal, id int64, externalID, docSHA, actor string) (*Contract, error) {
	if !sha256RE.MatchString(docSHA) {
		return nil, invalid("sha256", "required: the hex SHA-256 of the signed document")
	}
	var out *Contract
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		scope := a
		if scope == nil {
			scope = &Principal{}
		}
		var c *Contract
		var err error
		if id == 0 {
			var cid int64
			if err = tx.QueryRowContext(ctx, `SELECT id FROM contracts WHERE esign_id = ? AND esign_id <> ''`, externalID).Scan(&cid); err != nil {
				return ErrNotFound
			}
			id = cid
		}
		if c, err = s.loadContract(ctx, tx, scope, id); err != nil {
			return err
		}
		if c.Status == CtSigned || c.Status == CtActive {
			if c.SignedSHA256 == docSHA {
				out = c // a repeated notification
				return nil
			}
		}
		if c.Status != CtSent {
			return fmt.Errorf("%w: only a sent contract can be signed (it is %s)", ErrConflict, c.Status)
		}
		if c.ESignProvider == SignProvider {
			// Only the signers' own links complete such a request: nobody records it by hand.
			return fmt.Errorf("%w: this contract is out for signature by link; revoke that request first", ErrConflict)
		}
		if externalID != "" && c.esignID != "" && c.esignID != externalID {
			return fmt.Errorf("%w: the document id does not belong to this contract", ErrConflict)
		}
		if docSHA != c.SHA256 {
			return invalid("sha256", "the signed document's hash does not match the current text of the contract; it was changed or is a different document")
		}
		now := s.now()
		status := CtSigned
		if c.StartDate == nil || *c.StartDate <= dateOf(now) {
			status = CtActive
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = ?, signed_at = ?, signed_sha256 = ?, version = version + 1, updated_at = ? WHERE id = ?`, status, ts(now), docSHA, ts(now), id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actor, "contract.signed", fmt.Sprint(id), map[string]any{"sha256": docSHA}); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventContractSigned, contractSignedData{ContractID: id, PartyID: c.PartyID, Version: c.CurrentVersion, SHA256: docSHA, SignedAt: now.UTC()}); err != nil {
			return err
		}
		out, err = s.loadContract(ctx, tx, scope, id)
		return err
	})
	return out, err
}

// FindByESignID resolves a provider's document id to a contract id (0 when unknown).
func (s *Store) FindByESignID(ctx context.Context, ext string) int64 {
	var id int64
	_ = s.rdb.QueryRowContext(ctx, `SELECT id FROM contracts WHERE esign_id = ? AND esign_id <> ''`, ext).Scan(&id)
	return id
}

// UpdateContractDates changes start, end, renewal and the reminder lead time. Changing the
// renewal date re-arms the reminder.
func (s *Store) UpdateContractDates(ctx context.Context, a *Principal, id int64, ifMatch *int64, in ContractDates) (*Contract, error) {
	var out *Contract
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := s.loadContract(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != c.Version {
			return ErrVersionMismatch
		}
		if c.Status == CtExpired || c.Status == CtTerminated {
			return fmt.Errorf("%w: the contract is %s", ErrConflict, c.Status)
		}
		var p problems
		apply := func(f string, o Opt[string], dst **string) {
			if !o.Set {
				return
			}
			if o.Null || o.V == "" {
				*dst = nil
			} else if !validDate(o.V) {
				p.add(f, "use YYYY-MM-DD")
			} else {
				v := o.V
				*dst = &v
			}
		}
		apply("start_date", in.StartDate, &c.StartDate)
		apply("end_date", in.EndDate, &c.EndDate)
		apply("renewal_date", in.RenewalDate, &c.RenewalDate)
		if in.ReminderDays != nil {
			if *in.ReminderDays < 0 || *in.ReminderDays > 365 {
				p.add("reminder_days", "between 0 and 365")
			}
			c.ReminderDays = *in.ReminderDays
		}
		if c.StartDate != nil && c.EndDate != nil && *c.EndDate < *c.StartDate {
			p.add("end_date", "must not be before start_date")
		}
		if err := p.err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET start_date = ?, end_date = ?, renewal_date = ?, reminder_days = ?, reminded_for = CASE WHEN renewal_date IS ? THEN reminded_for ELSE '' END, version = version + 1, updated_by = ?, updated_at = ? WHERE id = ?`,
			c.StartDate, c.EndDate, c.RenewalDate, c.ReminderDays, c.RenewalDate, a.Subject, ts(s.now()), id); err != nil {
			return err
		}
		out, err = s.loadContract(ctx, tx, a, id)
		return err
	})
	return out, err
}

// TerminateContract ends a signed or active contract early.
func (s *Store) TerminateContract(ctx context.Context, a *Principal, id int64, ifMatch *int64, reason string) (*Contract, error) {
	if strings.TrimSpace(reason) == "" || !oneLineText(reason, 500) {
		return nil, invalid("reason", "required, one line, at most 500 characters")
	}
	var out *Contract
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := s.loadContract(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != c.Version {
			return ErrVersionMismatch
		}
		if c.Status != CtSigned && c.Status != CtActive {
			return fmt.Errorf("%w: only a signed or active contract can be terminated (it is %s)", ErrConflict, c.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = 'terminated', terminated_at = ?, terminate_reason = ?, version = version + 1, updated_by = ?, updated_at = ? WHERE id = ?`, ts(s.now()), reason, a.Subject, ts(s.now()), id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "contract.terminate", fmt.Sprint(id), map[string]any{"reason": reason}); err != nil {
			return err
		}
		out, err = s.loadContract(ctx, tx, a, id)
		return err
	})
	return out, err
}

// VerifyReport is the result of an integrity check.
type VerifyReport struct {
	ContractID int64    `json:"contract_id"`
	OK         bool     `json:"ok"`
	Versions   int      `json:"versions"`
	Problems   []string `json:"problems"`
}

// VerifyContract recomputes the hash of every stored version and compares it with the hash
// recorded when the version was written, and the signed hash with the current version's.
func (s *Store) VerifyContract(ctx context.Context, a *Principal, id int64) (*VerifyReport, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	// One read transaction: a signature committed while this runs must not look like tampering.
	tx, err := s.rdb.BeginTx(rctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := s.loadContract(rctx, tx, a, id)
	if err != nil {
		return nil, err
	}
	rep := &VerifyReport{ContractID: id, OK: true, Problems: []string{}}
	rows, err := tx.QueryContext(rctx, `SELECT version, body, sha256 FROM contract_versions WHERE contract_id = ? ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v int64
		var body, sum string
		if err := rows.Scan(&v, &body, &sum); err != nil {
			return nil, err
		}
		rep.Versions++
		if hashBody(body) != sum {
			rep.OK = false
			rep.Problems = append(rep.Problems, fmt.Sprintf("version %d: stored text no longer matches its recorded hash", v))
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	signing, err := s.verifySigning(rctx, tx, c)
	if err != nil {
		return nil, err
	}
	if len(signing) > 0 {
		rep.OK = false
		rep.Problems = append(rep.Problems, signing...)
	}
	if c.SignedSHA256 != "" && c.SignedSHA256 != c.SHA256 {
		rep.OK = false
		rep.Problems = append(rep.Problems, "the signed hash differs from the current version's hash")
	}
	return rep, rows.Err()
}

// SweepContracts applies the date-driven transitions and raises renewal reminders as of a
// date: signed contracts whose start has come become active, active contracts past their
// end date expire, and an active contract whose renewal date is within reminder_days gets
// one contract.expiring.v1 event (re-armed if the renewal date changes). It returns the
// number of reminders raised.
func (s *Store) SweepContracts(ctx context.Context, asOf string) (int, error) {
	if !validDate(asOf) {
		return 0, invalid("as_of", "use YYYY-MM-DD")
	}
	reminders := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = 'active', version = version + 1 WHERE status = 'signed' AND (start_date IS NULL OR start_date <= ?)`, asOf); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = 'expired', version = version + 1 WHERE status IN ('signed', 'active') AND end_date IS NOT NULL AND end_date < ?`, asOf); err != nil {
			return err
		}
		const pending = `status = 'active' AND renewal_date IS NOT NULL AND reminded_for <> renewal_date AND date(renewal_date, '-' || reminder_days || ' days') <= ?`
		var waiting int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contracts WHERE `+pending, asOf).Scan(&waiting); err != nil {
			return err
		}
		// Many at once become one event with a count (summaryAbove, reminders.go).
		if waiting > summaryAbove {
			if _, err := tx.ExecContext(ctx, `UPDATE contracts SET reminded_for = renewal_date WHERE `+pending, asOf); err != nil {
				return err
			}
			reminders = waiting
			return s.emit(ctx, tx, EventContractExpiring, contractSummaryData{Count: waiting})
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, party_id, renewal_date, reminder_days FROM contracts WHERE `+pending+` ORDER BY id`, asOf)
		if err != nil {
			return err
		}
		type due struct {
			id, party int64
			renewal   string
		}
		var ds []due
		for rows.Next() {
			var d due
			var days int
			if err := rows.Scan(&d.id, &d.party, &d.renewal, &days); err != nil {
				_ = rows.Close()
				return err
			}
			ds = append(ds, d)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, d := range ds {
			left := int(parseDate(d.renewal).Sub(parseDate(asOf)).Hours() / 24)
			if _, err := tx.ExecContext(ctx, `UPDATE contracts SET reminded_for = ? WHERE id = ?`, d.renewal, d.id); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, EventContractExpiring, contractExpiringData{ContractID: d.id, PartyID: d.party, RenewalDate: d.renewal, DaysLeft: left}); err != nil {
				return err
			}
			reminders++
		}
		return nil
	})
	return reminders, err
}
