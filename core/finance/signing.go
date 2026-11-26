package finance

// In-house "simple electronic signature" of contracts (specs/049-contract-esign). A signer is
// authenticated only by access to the mailbox the sender named: a single-use, expiring link is
// mailed there, and staff never see it (it is stored as a hash). This is not an advanced or
// qualified electronic signature; the spec records the limits.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Mailer queues one message through can-core's email.send (*modclient.Client implements it).
type Mailer interface {
	SendEmail(ctx context.Context, e modclient.Email) error
}

// SignProvider is the contracts.esign_provider of the in-house signing link.
const SignProvider = "link"

// Signing request statuses.
const (
	EnvPending   = "pending"
	EnvCompleted = "completed"
	EnvDeclined  = "declined"
	EnvRevoked   = "revoked"
)

// Signer roles and statuses.
const (
	RoleCounterparty = "counterparty"
	RoleCompany      = "company"

	SignerWaiting  = "waiting"
	SignerInvited  = "invited"
	SignerSigned   = "signed"
	SignerDeclined = "declined"
)

// Outcomes of handing an invitation to email.send.
const (
	MailPending    = "pending" // not known: the request is in flight or was interrupted
	MailQueued     = "queued"
	MailNotAllowed = "not_allowed"
	MailFailed     = "failed"
)

// IP policy values (signing_record_ip).
const (
	IPTruncated = "truncated"
	IPFull      = "full"
	IPNone      = "none"
)

const (
	maxSigners      = 10
	defaultLinkDays = 7
	maxLinkDays     = 30
	maxSends        = 10 // invitations per signer and request: a stuck button is not a mail bomb
	maxReason       = 500
	mailBudget      = 20 * time.Second
	signTokenBytes  = 32
	signPathPrefix  = "/public/sign/"
)

// ConsentText is what a signer agrees to; the exact words are stored with each signature.
const ConsentText = "I have read the document shown above and I agree to sign it electronically. I understand that typing my name and pressing Sign is my signature."

// signing is the Store's configuration of the link flow.
type signing struct {
	mailer  Mailer
	baseURL string // https://finance.<domain>; "" means the public signing path is not published
	company string // the seller name printed in mails and on the signing page
	ipMode  string
	log     *slog.Logger
	wg      sync.WaitGroup // mails sent after a public request has been answered
}

// SigningAvailable reports whether contracts can be sent for signature by link.
func (s *Store) SigningAvailable() bool { return s.sign.mailer != nil && s.sign.baseURL != "" }

func (s *Store) signingReady() error {
	if s.sign.baseURL == "" {
		return fmt.Errorf("%w: signing by link is off: the public signing path is not published (company.yaml modules.finance.expose.signing, docs/finance.md)", ErrConflict)
	}
	if s.sign.mailer == nil {
		return fmt.Errorf("%w: signing by link needs can-core's email service", ErrConflict)
	}
	return nil
}

// SignerInput names one person asked to sign.
type SignerInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"` // counterparty (default) or company
}

// SigningRequest sends a draft contract for signature by emailed link.
type SigningRequest struct {
	Signers []SignerInput `json:"signers"`
	// Ordered invites the signers one after another in the order given (countersigning).
	Ordered       bool `json:"ordered"`
	ExpiresInDays int  `json:"expires_in_days"`
}

// Signer is the staff view of a signer. The link and its hash are never part of it.
type Signer struct {
	ID            int64      `json:"id"`
	Position      int        `json:"position"`
	Role          string     `json:"role"`
	Name          string     `json:"name"`
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	MailStatus    string     `json:"mail_status"`
	ExpiresAt     *time.Time `json:"expires_at"`
	Expired       bool       `json:"expired"`
	TypedName     string     `json:"typed_name,omitempty"`
	IP            string     `json:"ip,omitempty"`
	SignedAt      *time.Time `json:"signed_at"`
	DeclinedAt    *time.Time `json:"declined_at"`
	DeclineReason string     `json:"decline_reason,omitempty"`
	consent       string
}

// Envelope is one signing request.
type Envelope struct {
	ID         int64      `json:"id"`
	ContractID int64      `json:"contract_id"`
	Version    int64      `json:"version"`
	SHA256     string     `json:"sha256"`
	Title      string     `json:"title"`
	Ordered    bool       `json:"ordered"`
	LinkDays   int        `json:"link_days"`
	Status     string     `json:"status"`
	Sender     string     `json:"sender"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at"`
	ChainHead  string     `json:"chain_head"`
	Signers    []Signer   `json:"signers"`
	scrubbed   bool
}

// ---- tokens, hashes ------------------------------------------------------------------------

func newSignToken() string {
	b := make([]byte, signTokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// digest hashes a list of values unambiguously (as a JSON array).
func digest(v ...any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only strings, booleans and integers are passed
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---- evidence chain ------------------------------------------------------------------------

type evidence struct {
	Seq      int
	Kind     string
	SignerID int64
	At       time.Time
	Hash     string
	data     string
	prev     string
}

func linkHash(e *Envelope, ev evidence) string {
	return digest("can-finance-signing-v1", e.ID, e.ContractID, e.Version, e.SHA256, ev.Seq, ev.Kind, ev.SignerID, ev.data, ev.At.Unix(), ev.prev)
}

// appendEvidence adds an event to the envelope's chain, in the caller's transaction.
func (s *Store) appendEvidence(ctx context.Context, tx dbq, e *Envelope, kind string, signerID int64, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM signing_events WHERE envelope_id = ?`, e.ID).Scan(&seq); err != nil {
		return err
	}
	ev := evidence{Seq: seq + 1, Kind: kind, SignerID: signerID, At: fromUnix(ts(s.now())), data: string(b), prev: e.ChainHead}
	ev.Hash = linkHash(e, ev)
	if _, err := tx.ExecContext(ctx, `INSERT INTO signing_events (envelope_id, seq, kind, signer_id, data, at, prev_hash, hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, ev.Seq, ev.Kind, ev.SignerID, ev.data, ev.At.Unix(), ev.prev, ev.Hash); err != nil {
		return err
	}
	e.ChainHead = ev.Hash
	_, err = tx.ExecContext(ctx, `UPDATE signing_envelopes SET chain_head = ? WHERE id = ?`, ev.Hash, e.ID)
	return err
}

func signerDigest(id int64, role, name, email string) string {
	return digest("signer", id, role, name, email)
}

func signatureDigest(id int64, typed, consent, ip string, at int64) string {
	return digest("signature", id, typed, consent, ip, at)
}

// ---- loading -------------------------------------------------------------------------------

const envCols = `id, contract_id, version, sha256, title, ordered, link_days, status, sender, created_at, closed_at, chain_head, scrubbed`

func scanEnvelope(sc scanner) (*Envelope, error) {
	var e Envelope
	var created int64
	var closed sql.NullInt64
	if err := sc.Scan(&e.ID, &e.ContractID, &e.Version, &e.SHA256, &e.Title, &e.Ordered, &e.LinkDays, &e.Status, &e.Sender, &created, &closed, &e.ChainHead, &e.scrubbed); err != nil {
		return nil, err
	}
	e.CreatedAt, e.ClosedAt = fromUnix(created), nullTime(closed)
	return &e, nil
}

func (s *Store) loadSigners(ctx context.Context, q dbq, e *Envelope) error {
	rows, err := q.QueryContext(ctx, `SELECT id, position, role, name, email, status, mail_status, expires_at, typed_name, consent_text, ip, signed_at, declined_at, decline_reason
		FROM signing_signers WHERE envelope_id = ? ORDER BY position`, e.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	e.Signers = []Signer{}
	now := s.now()
	for rows.Next() {
		var g Signer
		var exp, signed, declined sql.NullInt64
		if err := rows.Scan(&g.ID, &g.Position, &g.Role, &g.Name, &g.Email, &g.Status, &g.MailStatus, &exp, &g.TypedName, &g.consent, &g.IP, &signed, &declined, &g.DeclineReason); err != nil {
			return err
		}
		g.ExpiresAt, g.SignedAt, g.DeclinedAt = nullTime(exp), nullTime(signed), nullTime(declined)
		g.Expired = e.Status == EnvPending && g.Status == SignerInvited && g.ExpiresAt != nil && !now.Before(*g.ExpiresAt)
		e.Signers = append(e.Signers, g)
	}
	return rows.Err()
}

// envelope loads one signing request with its signers; where is a fixed SQL condition.
func (s *Store) envelope(ctx context.Context, q dbq, where string, args ...any) (*Envelope, error) {
	e, err := scanEnvelope(q.QueryRowContext(ctx, `SELECT `+envCols+` FROM signing_envelopes WHERE `+where, args...)) //nolint:gosec // fixed SQL, values are parameters
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, s.loadSigners(ctx, q, e)
}

func (s *Store) latestEnvelope(ctx context.Context, q dbq, contractID int64) (*Envelope, error) {
	return s.envelope(ctx, q, `contract_id = ? ORDER BY id DESC LIMIT 1`, contractID)
}

// Signing returns the latest signing request of a contract in scope. Reading it is audited: it
// holds the signers' names and addresses.
func (s *Store) Signing(ctx context.Context, a *Principal, contractID int64) (*Envelope, error) {
	var out *Envelope
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.loadContract(ctx, tx, a, contractID); err != nil {
			return err
		}
		var err error
		if out, err = s.latestEnvelope(ctx, tx, contractID); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "contract.signing.read", fmt.Sprint(contractID), map[string]any{"envelope": out.ID})
	})
	return out, err
}

// ---- sending -------------------------------------------------------------------------------

// invitation is a mail to send after the transaction that created its token has committed. The
// token exists only here and in the mail: the database holds its hash.
type invitation struct {
	envelope, signer int64
	to, title, token string
	expires          time.Time
}

// deceptive reports characters that can make the shown text differ from the signed text: control
// characters and the bidirectional embedding, override and isolate marks.
func deceptive(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		if r == '\n' || r == '\r' || r == '\t' {
			return false
		}
		return unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
	})
}

func checkSigners(in *SigningRequest) error {
	var p problems
	if len(in.Signers) == 0 || len(in.Signers) > maxSigners {
		p.add("signers", "give between 1 and %d signers", maxSigners)
		return p.err()
	}
	in.Signers = slices.Clone(in.Signers)
	seen := map[string]bool{}
	for i := range in.Signers {
		g := &in.Signers[i]
		f := "signers." + strconv.Itoa(i) + "."
		g.Name, g.Email = strings.TrimSpace(g.Name), strings.TrimSpace(g.Email)
		if g.Name == "" || !oneLineText(g.Name, maxName) || deceptive(g.Name) {
			p.add(f+"name", "required, one line, at most %d characters", maxName)
		}
		if g.Email == "" || !validEmail(g.Email) {
			p.add(f+"email", "not a valid email address")
		} else if k := strings.ToLower(g.Email); seen[k] {
			p.add(f+"email", "each signer needs their own address")
		} else {
			seen[k] = true
		}
		if g.Role == "" {
			g.Role = RoleCounterparty
		}
		if g.Role != RoleCounterparty && g.Role != RoleCompany {
			p.add(f+"role", "counterparty or company")
		}
	}
	if in.ExpiresInDays == 0 {
		in.ExpiresInDays = defaultLinkDays
	}
	if in.ExpiresInDays < 1 || in.ExpiresInDays > maxLinkDays {
		p.add("expires_in_days", "between 1 and %d", maxLinkDays)
	}
	return p.err()
}

// invite gives a signer a new link (any earlier one stops working) in the caller's transaction.
func (s *Store) invite(ctx context.Context, tx dbq, actor string, e *Envelope, signerID int64, email string) (invitation, error) {
	token := newSignToken()
	exp := s.now().Add(time.Duration(e.LinkDays) * 24 * time.Hour)
	if _, err := tx.ExecContext(ctx, `UPDATE signing_signers SET status = 'invited', token_hash = ?, expires_at = ?, sends = sends + 1, mail_status = ? WHERE id = ?`,
		tokenHash(token), ts(exp), MailPending, signerID); err != nil {
		return invitation{}, err
	}
	if err := s.appendEvidence(ctx, tx, e, "invited", signerID, map[string]any{"expires_at": ts(exp)}); err != nil {
		return invitation{}, err
	}
	if err := s.audit(ctx, tx, actor, "contract.signing.invite", fmt.Sprint(e.ContractID), map[string]any{"envelope": e.ID, "signer": signerID}); err != nil {
		return invitation{}, err
	}
	return invitation{envelope: e.ID, signer: signerID, to: email, title: e.Title, token: token, expires: exp}, nil
}

// SendForSigning freezes the current text of a draft contract and mails each signer (or, with
// ordered, the first one) a link. The mail outcome is in the returned request: a refused or failed
// invitation leaves the request pending, to be resent.
func (s *Store) SendForSigning(ctx context.Context, a *Principal, id int64, ifMatch *int64, in SigningRequest) (*Envelope, error) {
	if err := checkSigners(&in); err != nil {
		return nil, err
	}
	if err := s.signingReady(); err != nil {
		return nil, err
	}
	var envID int64
	var mails []invitation
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		mails = nil
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
		party, err := s.loadPartyRow(ctx, tx, a, c.PartyID)
		if err != nil {
			return err
		}
		if party.Erased {
			return invalid("party_id", "the party has been erased")
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM contract_versions WHERE contract_id = ? AND version = ?`, id, c.CurrentVersion).Scan(&body); err != nil {
			return err
		}
		if hashBody(body) != c.SHA256 {
			return fmt.Errorf("%w: the stored text does not match its hash; it cannot be sent", ErrConflict)
		}
		if deceptive(body) || deceptive(c.Title) || !utf8.ValidString(body) {
			return invalid("body", "the text contains control or text-direction characters that could show something other than what is signed")
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `INSERT INTO signing_envelopes (contract_id, version, sha256, title, ordered, link_days, status, sender, created_at) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			id, c.CurrentVersion, c.SHA256, c.Title, in.Ordered, in.ExpiresInDays, a.Subject, ts(now))
		if err != nil {
			return err
		}
		envID, _ = res.LastInsertId()
		e, err := s.envelope(ctx, tx, `id = ?`, envID)
		if err != nil {
			return err
		}
		ids := make([]int64, len(in.Signers))
		digests := make([]string, len(in.Signers))
		for i, g := range in.Signers {
			r, err := tx.ExecContext(ctx, `INSERT INTO signing_signers (envelope_id, position, role, name, email, status) VALUES (?, ?, ?, ?, ?, 'waiting')`, envID, i+1, g.Role, g.Name, g.Email)
			if err != nil {
				return err
			}
			ids[i], _ = r.LastInsertId()
			digests[i] = signerDigest(ids[i], g.Role, g.Name, g.Email)
		}
		if err := s.appendEvidence(ctx, tx, e, "sent", 0, map[string]any{"signers": digests, "ordered": in.Ordered, "title_sha256": hashBody(c.Title), "sender_sha256": digest(a.Subject)}); err != nil {
			return err
		}
		for i, g := range in.Signers {
			if in.Ordered && i > 0 {
				break
			}
			m, err := s.invite(ctx, tx, a.Subject, e, ids[i], g.Email)
			if err != nil {
				return err
			}
			mails = append(mails, m)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = 'sent', sent_once = 1, esign_provider = ?, esign_id = '', version = version + 1, updated_by = ?, updated_at = ? WHERE id = ?`, SignProvider, a.Subject, ts(now), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "contract.signing.send", fmt.Sprint(id), map[string]any{"envelope": envID, "sha256": c.SHA256, "signers": len(in.Signers), "ordered": in.Ordered})
	})
	if err != nil {
		return nil, err
	}
	return s.mailAndReload(ctx, envID, mails)
}

// mailAndReload sends the invitations of a committed change and returns the request as it then is.
// A caller that goes away must not leave a stored invitation unsent.
func (s *Store) mailAndReload(ctx context.Context, envID int64, mails []invitation) (*Envelope, error) {
	bg := context.WithoutCancel(ctx)
	s.sendInvitations(bg, mails)
	rctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	return s.envelope(rctx, s.db, `id = ?`, envID)
}

// ResendInvitation mails a signer whose turn it is a new link; the old one stops working.
func (s *Store) ResendInvitation(ctx context.Context, a *Principal, contractID, signerID int64) (*Envelope, error) {
	if err := s.signingReady(); err != nil {
		return nil, err
	}
	var mails []invitation
	var envID int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		mails = nil
		if _, err := s.loadContract(ctx, tx, a, contractID); err != nil {
			return err
		}
		e, err := s.latestEnvelope(ctx, tx, contractID)
		if err != nil {
			return err
		}
		envID = e.ID
		i := slices.IndexFunc(e.Signers, func(g Signer) bool { return g.ID == signerID })
		if i < 0 {
			return ErrNotFound
		}
		if e.Status != EnvPending || e.Signers[i].Status != SignerInvited {
			return fmt.Errorf("%w: only a signer who has been invited and has not answered, in an open request, can be sent a new link", ErrConflict)
		}
		var sends int
		if err := tx.QueryRowContext(ctx, `SELECT sends FROM signing_signers WHERE id = ?`, signerID).Scan(&sends); err != nil {
			return err
		}
		if sends >= maxSends {
			return fmt.Errorf("%w: this signer has been sent %d links; revoke the request and send the contract again", ErrConflict, sends)
		}
		m, err := s.invite(ctx, tx, a.Subject, e, signerID, e.Signers[i].Email)
		if err != nil {
			return err
		}
		mails = append(mails, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.mailAndReload(ctx, envID, mails)
}

// closeEnvelope ends an open request without a signature: every link dies and the contract is a
// draft again.
func (s *Store) closeEnvelope(ctx context.Context, tx dbq, actor string, e *Envelope, status, kind string, signerID int64, data any) error {
	now := ts(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE signing_signers SET token_hash = NULL WHERE envelope_id = ? AND token_hash IS NOT NULL`, e.ID); err != nil {
		return err
	}
	if err := s.appendEvidence(ctx, tx, e, kind, signerID, data); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE signing_envelopes SET status = ?, closed_at = ? WHERE id = ?`, status, now, e.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = 'draft', esign_provider = '', version = version + 1, updated_at = ? WHERE id = ? AND status = 'sent'`, now, e.ContractID); err != nil {
		return err
	}
	return s.audit(ctx, tx, actor, "contract.signing."+kind, fmt.Sprint(e.ContractID), map[string]any{"envelope": e.ID})
}

// revokePending revokes the open request of a contract, if there is one.
func (s *Store) revokePending(ctx context.Context, tx dbq, actor string, contractID int64, reason string) (bool, error) {
	e, err := s.envelope(ctx, tx, `contract_id = ? AND status = 'pending'`, contractID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.closeEnvelope(ctx, tx, actor, e, EnvRevoked, "revoked", 0, map[string]any{"reason": reason})
}

// RevokeSigning cancels an open signing request. A completed one cannot be revoked.
func (s *Store) RevokeSigning(ctx context.Context, a *Principal, contractID int64) (*Envelope, error) {
	var out *Envelope
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.loadContract(ctx, tx, a, contractID); err != nil {
			return err
		}
		done, err := s.revokePending(ctx, tx, a.Subject, contractID, "staff")
		if err != nil {
			return err
		}
		if !done {
			return fmt.Errorf("%w: there is no open signing request to revoke", ErrConflict)
		}
		out, err = s.latestEnvelope(ctx, tx, contractID)
		return err
	})
	return out, err
}

// ---- the signer's side ---------------------------------------------------------------------

// SignerView is what the holder of a live link may see.
type SignerView struct {
	Company string
	Title   string
	Body    string
	SHA256  string
	Name    string // as the sender wrote it
	Version int64
	// ContractID names the download file only.
	ContractID int64
}

// openLink resolves a token to its signer. Every reason a link does not work (unknown, used,
// expired, replaced, revoked, a closed request, a changed contract) is the same ErrNotFound.
func (s *Store) openLink(ctx context.Context, q dbq, token string) (*Envelope, *Signer, string, error) {
	if len(token) != base64.RawURLEncoding.EncodedLen(signTokenBytes) || s.sign.baseURL == "" {
		return nil, nil, "", ErrNotFound
	}
	var envID, signerID int64
	var body string
	err := q.QueryRowContext(ctx, `SELECT e.id, g.id, v.body FROM signing_signers g
		JOIN signing_envelopes e ON e.id = g.envelope_id
		JOIN contracts c ON c.id = e.contract_id AND c.status = 'sent' AND c.esign_provider = ? AND c.current_version = e.version
		JOIN contract_versions v ON v.contract_id = c.id AND v.version = e.version
		WHERE g.token_hash = ? AND g.status = 'invited' AND g.expires_at > ? AND e.status = 'pending'`, SignProvider, tokenHash(token), ts(s.now())).Scan(&envID, &signerID, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", ErrNotFound
	}
	if err != nil {
		return nil, nil, "", err
	}
	e, err := s.envelope(ctx, q, `id = ?`, envID)
	if err != nil {
		return nil, nil, "", err
	}
	if hashBody(body) != e.SHA256 {
		s.sign.log.Error("signing: the stored text no longer matches the hash that was sent; the link is not served", "contract_id", e.ContractID)
		return nil, nil, "", ErrNotFound
	}
	i := slices.IndexFunc(e.Signers, func(g Signer) bool { return g.ID == signerID })
	if i < 0 {
		return nil, nil, "", ErrNotFound
	}
	return e, &e.Signers[i], body, nil
}

func (s *Store) view(e *Envelope, g *Signer, body string) *SignerView {
	return &SignerView{Company: s.sign.company, Title: e.Title, Body: body, SHA256: e.SHA256, Name: g.Name, Version: e.Version, ContractID: e.ContractID}
}

// ViewSigning returns the frozen document for a live link. Opening a link consumes nothing: mail
// scanners follow links.
func (s *Store) ViewSigning(ctx context.Context, token string) (*SignerView, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	e, g, body, err := s.openLink(rctx, s.rdb, token)
	if err != nil {
		return nil, err
	}
	return s.view(e, g, body), nil
}

// SignInput is what a signer submits.
type SignInput struct {
	TypedName string
	Consent   bool
	SHA256    string // the hash of the document the form was shown with
	IP        string // the client address; stored according to the IP policy
}

// recordedIP applies the IP policy. truncated keeps a /24 (IPv4) or /48 (IPv6).
func recordedIP(mode, ip string) string {
	p := net.ParseIP(ip)
	if p == nil || mode == IPNone {
		return ""
	}
	if mode == IPFull {
		return p.String()
	}
	if v4 := p.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return p.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

// SignReceipt is what a signer is shown right after signing.
type SignReceipt struct {
	View      SignerView
	TypedName string
	SignedAt  time.Time
	Completed bool
	ChainHead string
}

// Sign records a signature, uses up the link and, when it was the last one, completes the contract.
func (s *Store) Sign(ctx context.Context, token string, in SignInput) (*SignReceipt, error) {
	in.TypedName = strings.TrimSpace(in.TypedName)
	var out *SignReceipt
	var mails []invitation
	var done *Envelope
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		mails, done = nil, nil
		e, g, body, err := s.openLink(ctx, tx, token)
		if err != nil {
			return err
		}
		var p problems
		if in.TypedName == "" || !oneLineText(in.TypedName, maxName) || deceptive(in.TypedName) {
			p.add("typed_name", "type your full name")
		}
		if !in.Consent {
			p.add("consent", "tick the box to agree")
		}
		if in.SHA256 != e.SHA256 {
			p.add("document", "the form does not belong to this document; open the link again")
		}
		if err := p.err(); err != nil {
			return err
		}
		now := ts(s.now())
		ip := recordedIP(s.sign.ipMode, in.IP)
		actor := "signer:" + strconv.FormatInt(g.ID, 10)
		if _, err := tx.ExecContext(ctx, `UPDATE signing_signers SET status = 'signed', token_hash = NULL, typed_name = ?, consent_text = ?, ip = ?, signed_at = ? WHERE id = ?`,
			in.TypedName, ConsentText, ip, now, g.ID); err != nil {
			return err
		}
		if err := s.appendEvidence(ctx, tx, e, "signed", g.ID, map[string]any{"digest": signatureDigest(g.ID, in.TypedName, ConsentText, ip, now)}); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actor, "contract.signing.signed", fmt.Sprint(e.ContractID), map[string]any{"envelope": e.ID, "sha256": e.SHA256}); err != nil {
			return err
		}
		out = &SignReceipt{View: *s.view(e, g, body), TypedName: in.TypedName, SignedAt: fromUnix(now)}
		next := slices.IndexFunc(e.Signers, func(x Signer) bool { return x.ID != g.ID && x.Status != SignerSigned })
		if next < 0 {
			if err := s.complete(ctx, tx, actor, e); err != nil {
				return err
			}
			out.Completed = true
			if done, err = s.envelope(ctx, tx, `id = ?`, e.ID); err != nil {
				return err
			}
		} else if e.Ordered && e.Signers[next].Status == SignerWaiting {
			m, err := s.invite(ctx, tx, actor, e, e.Signers[next].ID, e.Signers[next].Email)
			if err != nil {
				return err
			}
			mails = append(mails, m)
		}
		out.ChainHead = e.ChainHead
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The signer is not kept waiting for the relay.
	bg := context.WithoutCancel(ctx)
	s.sign.wg.Add(1)
	go func() {
		defer s.sign.wg.Done()
		s.sendInvitations(bg, mails)
		if done != nil {
			s.sendCompletion(bg, done)
		}
	}()
	return out, nil
}

// complete marks the request and the contract signed, in the caller's transaction.
func (s *Store) complete(ctx context.Context, tx dbq, actor string, e *Envelope) error {
	now := s.now()
	if err := s.appendEvidence(ctx, tx, e, "completed", 0, map[string]any{}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE signing_envelopes SET status = 'completed', closed_at = ? WHERE id = ?`, ts(now), e.ID); err != nil {
		return err
	}
	var party int64
	var start sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT party_id, start_date FROM contracts WHERE id = ?`, e.ContractID).Scan(&party, &start); err != nil {
		return err
	}
	status := CtSigned
	if !start.Valid || start.String <= dateOf(now) {
		status = CtActive
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contracts SET status = ?, signed_at = ?, signed_sha256 = ?, version = version + 1, updated_at = ? WHERE id = ?`, status, ts(now), e.SHA256, ts(now), e.ContractID); err != nil {
		return err
	}
	if err := s.audit(ctx, tx, actor, "contract.signed", fmt.Sprint(e.ContractID), map[string]any{"sha256": e.SHA256, "envelope": e.ID}); err != nil {
		return err
	}
	return s.emit(ctx, tx, EventContractSigned, contractSignedData{ContractID: e.ContractID, PartyID: party, Version: e.Version, SHA256: e.SHA256, SignedAt: now.UTC()})
}

// Decline records a refusal: the whole request ends and the contract is a draft again.
func (s *Store) Decline(ctx context.Context, token, reason, clientAddr string) error {
	reason = strings.TrimSpace(strings.ReplaceAll(reason, "\r\n", "\n"))
	return s.inTx(ctx, func(tx *sql.Tx) error {
		e, g, _, err := s.openLink(ctx, tx, token)
		if err != nil {
			return err
		}
		if !validText(reason, maxReason) || deceptive(reason) {
			return invalid("reason", "at most %d characters", maxReason)
		}
		now := ts(s.now())
		ip := recordedIP(s.sign.ipMode, clientAddr)
		if _, err := tx.ExecContext(ctx, `UPDATE signing_signers SET status = 'declined', token_hash = NULL, declined_at = ?, decline_reason = ?, ip = ? WHERE id = ?`, now, reason, ip, g.ID); err != nil {
			return err
		}
		return s.closeEnvelope(ctx, tx, "signer:"+strconv.FormatInt(g.ID, 10), e, EnvDeclined, "declined", g.ID,
			map[string]any{"digest": signatureDigest(g.ID, reason, "", ip, now)})
	})
}

// ---- mail ----------------------------------------------------------------------------------

// mailSubject keeps a subject within email.send's one-line, 200-byte rule.
func mailSubject(prefix, title string) string {
	s := prefix + strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, title)
	for len(s) > 190 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

func (s *Store) mail(ctx context.Context, to, subject, text string) error {
	var r [12]byte
	_, _ = rand.Read(r[:])
	return s.sign.mailer.SendEmail(ctx, modclient.Email{To: []string{to}, Subject: subject, Text: text, SourceID: "ctsign-" + hex.EncodeToString(r[:])})
}

// mailOutcome maps an error of email.send to a stored status. Error texts are not kept: they may
// quote addresses.
func mailOutcome(err error) string {
	var se *modclient.StatusError
	switch {
	case err == nil:
		return MailQueued
	case errors.As(err, &se) && se.Status == http.StatusForbidden:
		return MailNotAllowed
	}
	return MailFailed
}

// sendInvitations mails each link to its signer's address and nowhere else, then records the
// outcome. The link is never logged.
func (s *Store) sendInvitations(ctx context.Context, mails []invitation) {
	if len(mails) == 0 {
		return
	}
	from := s.sign.company
	if from == "" {
		from = "A company you deal with"
	}
	sendCtx, cancel := context.WithTimeout(ctx, mailBudget)
	defer cancel()
	for _, m := range mails {
		text := from + " asks you to read and sign a document:\n\n    " + m.title + "\n\n" +
			"Open this link to read it, then sign or decline:\n\n" + s.sign.baseURL + signPathPrefix + m.token + "\n\n" +
			"The link is for you alone: do not forward it. It works until " + m.expires.UTC().Format("2006-01-02 15:04 UTC") + " and can be used once.\n\n" +
			"If you did not expect this, ignore this message; nothing is signed unless you sign.\n"
		status := mailOutcome(s.mail(sendCtx, m.to, mailSubject("Please sign: ", m.title), text))
		if status != MailQueued {
			s.sign.log.Warn("signing invitation not queued", "envelope", m.envelope, "signer", m.signer, "outcome", status)
		}
		rctx, done := context.WithTimeout(ctx, 5*time.Second)
		err := s.inTx(rctx, func(tx *sql.Tx) error {
			// Only while this is still the signer's live link (or it was just used): a newer
			// invitation has its own outcome.
			if _, err := tx.ExecContext(rctx, `UPDATE signing_signers SET mail_status = ? WHERE id = ? AND (token_hash = ? OR token_hash IS NULL)`, status, m.signer, tokenHash(m.token)); err != nil {
				return err
			}
			return s.audit(rctx, tx, "can-finance", "contract.signing.mail", strconv.FormatInt(m.envelope, 10), map[string]any{"signer": m.signer, "outcome": status})
		})
		done()
		if err != nil {
			s.sign.log.Error("signing: recording the mail outcome", "envelope", m.envelope, "error", err)
		}
	}
}

// sendCompletion mails every signer the signers' copy of the certificate.
func (s *Store) sendCompletion(ctx context.Context, e *Envelope) {
	ctx, cancel := context.WithTimeout(ctx, mailBudget)
	defer cancel()
	text := "Everyone has signed. Keep this message: it is your record of what was signed.\n\n" + s.certificate(e, nil, false)
	for _, g := range e.Signers {
		if err := s.mail(ctx, g.Email, mailSubject("Signed: ", e.Title), text); err != nil {
			s.sign.log.Warn("signing completion mail not queued", "envelope", e.ID, "signer", g.ID, "outcome", mailOutcome(err))
		}
	}
}

// ---- certificate ---------------------------------------------------------------------------

// certificate renders the completion certificate. The staff copy adds addresses, the recorded
// client addresses, the sender and the evidence chain; the signers' copy does not give one party
// another's address.
func (s *Store) certificate(e *Envelope, chain []evidence, staff bool) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	when := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	w("CERTIFICATE OF COMPLETION (simple electronic signature)")
	w("")
	if s.sign.company != "" {
		w("Issued by:        %s", s.sign.company)
	}
	w("Document:         %s", e.Title)
	w("Reference:        contract %d, version %d, signing request %d", e.ContractID, e.Version, e.ID)
	w("Document SHA-256: %s", e.SHA256)
	w("Sent:             %s", when(&e.CreatedAt))
	if staff {
		w("Sent by:          %s", e.Sender)
	}
	w("Completed:        %s", when(e.ClosedAt))
	w("")
	w("Signatures")
	for _, g := range e.Signers {
		w("%d. %s (%s)", g.Position, g.Name, g.Role)
		w("   Typed signature: %s", g.TypedName)
		w("   Signed:          %s", when(g.SignedAt))
		if staff {
			w("   Link sent to:    %s", g.Email)
			if g.IP != "" {
				w("   Client address:  %s", g.IP)
			}
		}
	}
	w("")
	w("Each signer agreed to: %q", ConsentText)
	w("")
	w("Evidence chain head: %s", e.ChainHead)
	if staff {
		for _, ev := range chain {
			w("  %2d %-9s %s %s", ev.Seq, ev.Kind, when(&ev.At), ev.Hash)
		}
	}
	w("")
	w("What this shows and does not show: each signer was identified only by access to the email address")
	w("the sender gave, and typed their own name. No identity document was checked and no cryptographic")
	w("signature or trusted timestamp was made. This is not an advanced or qualified electronic signature.")
	w("Whether it is enough for a given document depends on the law that applies; this is not legal advice.")
	return b.String()
}

func (s *Store) chain(ctx context.Context, q dbq, envID int64) ([]evidence, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq, kind, signer_id, data, at, prev_hash, hash FROM signing_events WHERE envelope_id = ? ORDER BY seq`, envID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []evidence
	for rows.Next() {
		var ev evidence
		var at int64
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.SignerID, &ev.data, &at, &ev.prev, &ev.Hash); err != nil {
			return nil, err
		}
		ev.At = fromUnix(at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Certificate returns the staff copy of the completion certificate of a signed contract. Audited.
func (s *Store) Certificate(ctx context.Context, a *Principal, contractID int64) (string, error) {
	var out string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.loadContract(ctx, tx, a, contractID); err != nil {
			return err
		}
		e, err := s.envelope(ctx, tx, `contract_id = ? AND status = 'completed' ORDER BY id DESC LIMIT 1`, contractID)
		if err != nil {
			return err
		}
		chain, err := s.chain(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		out = s.certificate(e, chain, true)
		return s.audit(ctx, tx, a.Subject, "contract.signing.certificate", fmt.Sprint(contractID), map[string]any{"envelope": e.ID})
	})
	return out, err
}

// ---- verification --------------------------------------------------------------------------

// verifySigning checks every signing request of a contract: the chain, the records against the
// chain, the frozen hash against the stored version, and the contract's signed state.
func (s *Store) verifySigning(ctx context.Context, q dbq, c *Contract) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM signing_envelopes WHERE contract_id = ? ORDER BY id`, c.ID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var out []string
	completed := false
	for _, id := range ids {
		e, err := s.envelope(ctx, q, `id = ?`, id)
		if err != nil {
			return nil, err
		}
		chain, err := s.chain(ctx, q, id)
		if err != nil {
			return nil, err
		}
		var body string
		err = q.QueryRowContext(ctx, `SELECT body FROM contract_versions WHERE contract_id = ? AND version = ?`, c.ID, e.Version).Scan(&body)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		for _, p := range checkEnvelope(e, chain, err == nil && hashBody(body) == e.SHA256) {
			out = append(out, fmt.Sprintf("signing request %d: %s", id, p))
		}
		if e.Status == EnvCompleted {
			completed = true
			if c.SignedAt == nil || c.SignedSHA256 != e.SHA256 || c.CurrentVersion != e.Version {
				out = append(out, "the contract's signed record does not match the completed signing request")
			}
		}
	}
	if c.ESignProvider == SignProvider && c.SignedAt != nil && !completed {
		out = append(out, "the contract is marked signed by link but there is no completed signing request")
	}
	return out, nil
}

// checkEnvelope compares one request with its evidence chain.
func checkEnvelope(e *Envelope, chain []evidence, textOK bool) []string {
	var out []string
	bad := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	if !textOK {
		bad("the text no longer matches the hash that was sent")
	}
	var sent struct {
		Signers []string `json:"signers"`
		Ordered bool     `json:"ordered"`
		Title   string   `json:"title_sha256"`
		Sender  string   `json:"sender_sha256"`
	}
	decisions := map[int64]string{}
	prev, last := "", ""
	for i, ev := range chain {
		if ev.Seq != i+1 || ev.prev != prev || linkHash(e, ev) != ev.Hash {
			bad("evidence event %d does not fit the chain", ev.Seq)
		}
		prev, last = ev.Hash, ev.Kind
		switch ev.Kind {
		case "sent":
			if err := json.Unmarshal([]byte(ev.data), &sent); err != nil {
				bad("evidence event %d is unreadable", ev.Seq)
			}
		case "signed", "declined":
			var d struct {
				Digest string `json:"digest"`
			}
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
				bad("evidence event %d is unreadable", ev.Seq)
			}
			decisions[ev.SignerID] = d.Digest
		}
	}
	if len(chain) == 0 || prev != e.ChainHead {
		bad("the chain does not end where the request says it does")
	}
	if sent.Title != hashBody(e.Title) || sent.Sender != digest(e.Sender) || sent.Ordered != e.Ordered {
		bad("the title, sender or signing order was changed")
	}
	closing := map[string]string{EnvCompleted: "completed", EnvDeclined: "declined", EnvRevoked: "revoked"}[e.Status]
	if (closing != "" && last != closing) || (closing == "" && (last == "completed" || last == "declined" || last == "revoked")) {
		bad("the status does not match the evidence")
	}
	if len(sent.Signers) != len(e.Signers) {
		bad("the list of signers was changed")
	}
	for i, g := range e.Signers {
		if !e.scrubbed && (i >= len(sent.Signers) || sent.Signers[i] != signerDigest(g.ID, g.Role, g.Name, g.Email)) {
			bad("signer %d was changed", g.Position)
		}
		d, has := decisions[g.ID]
		decided := g.Status == SignerSigned || g.Status == SignerDeclined
		if decided != has {
			bad("signer %d's status does not match the evidence", g.Position)
			continue
		}
		if e.Status == EnvCompleted && g.Status != SignerSigned {
			bad("completed without signer %d", g.Position)
		}
		if !decided || e.scrubbed {
			continue
		}
		var got string
		switch {
		case g.Status == SignerSigned && g.SignedAt != nil:
			got = signatureDigest(g.ID, g.TypedName, g.consent, g.IP, g.SignedAt.Unix())
		case g.Status == SignerDeclined && g.DeclinedAt != nil:
			got = signatureDigest(g.ID, g.DeclineReason, "", g.IP, g.DeclinedAt.Unix())
		}
		if got != d {
			bad("the record of signer %d was changed", g.Position)
		}
	}
	return out
}

// ---- erasure and retention -----------------------------------------------------------------

// eraseSigning runs inside the party erasure (the purge gate is open): open requests are revoked
// and the personal data of requests that did not complete is removed. Completed requests are the
// signed contract's evidence and are retained with it.
func (s *Store) eraseSigning(ctx context.Context, tx dbq, actor string, partyID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.contract_id, e.status FROM signing_envelopes e JOIN contracts c ON c.id = e.contract_id WHERE c.party_id = ? AND e.status <> 'completed' AND e.scrubbed = 0`, partyID)
	if err != nil {
		return err
	}
	type row struct {
		id, contract int64
		status       string
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.contract, &r.status); err != nil {
			_ = rows.Close()
			return err
		}
		list = append(list, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range list {
		if r.status == EnvPending {
			if _, err := s.revokePending(ctx, tx, actor, r.contract, "erasure"); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE signing_signers SET name = '', email = '', typed_name = '', consent_text = '', ip = '', decline_reason = '' WHERE envelope_id = ?`, r.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE signing_envelopes SET scrubbed = 1 WHERE id = ?`, r.id); err != nil {
			return err
		}
	}
	return nil
}

// purgeSigning removes the signing records of the contracts the retention purge is about to delete;
// contracts is a fixed query selecting their ids.
func purgeSigning(ctx context.Context, tx dbq, contracts string, args ...any) error {
	envs := `SELECT id FROM signing_envelopes WHERE contract_id IN (` + contracts + `)`
	for _, q := range []string{`DELETE FROM signing_events WHERE envelope_id IN (` + envs + `)`,
		`DELETE FROM signing_signers WHERE envelope_id IN (` + envs + `)`,
		`DELETE FROM signing_envelopes WHERE contract_id IN (` + contracts + `)`} {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil { //nolint:gosec // fixed SQL, values are parameters
			return err
		}
	}
	return nil
}
