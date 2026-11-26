package finance

// Mailing documents to the customer (specs/062-invoice-email): an invoice, credit note or
// statement goes to the party's email address through can-core's email.send, with the signed
// public link. Nothing is attached: email.send carries plain text only.
//
// Every message is a row in document_mails, written (with its audit entry) before the call, whose
// random nonce is the idempotency key: a crash between the call and recording its outcome cannot
// make a second message. No address, name or text is stored in that row, audited, logged or put in
// an event: ids, states and numbers of documents only.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Kinds of document mail.
const (
	DocDocument  = "document" // an invoice or credit note
	DocStatement = "statement"
	DocReminder  = "reminder"
)

// States of a document mail. "sent" means the SMTP relay accepted it, not that it arrived.
const (
	DMPending    = "pending" // being handed over, or interrupted while it was
	DMRetry      = "retry"   // the mail service could not take it yet (reminders only)
	DMQueued     = "queued"  // accepted by can-core's outbox
	DMSent       = "sent"
	DMFailed     = "failed"
	DMNotAllowed = "not_allowed" // refused: the address is outside finance's allow-list
	DMCancelled  = "cancelled"   // the document stopped being due before it was handed over
	DMUnknown    = "unknown"     // it may or may not have been sent
)

var documentMailStatuses = []string{DMPending, DMRetry, DMQueued, DMSent, DMFailed, DMNotAllowed, DMCancelled, DMUnknown}

const (
	mailLinkTTL        = 60 * 24 * time.Hour // how long the link in a mail works
	mailCooldown       = time.Minute         // between two manual sends of one document
	mailPerDay         = 10                  // manual sends of one document in 24 hours
	statementLines     = 50                  // open invoices listed in one statement
	coreMessageTTL     = 30 * 24 * time.Hour // can-core forgets a finished message after this
	statusRefreshLimit = 20                  // messages asked about when a page is shown
	statusSweepLimit   = 200                 // and by the hourly loop
)

// docMailing is the Store's configuration for mailing documents.
type docMailing struct {
	secret  []byte // signs the public links
	base    string // public origin of the links; "" means the links are not published
	company string
	dun     sync.Mutex // one reminder sweep at a time
}

// queueMailer is the part of *modclient.Client that also reports delivery status.
type queueMailer interface {
	QueueEmail(ctx context.Context, e modclient.Email) ([]string, error)
	EmailStatus(ctx context.Context, id string) (modclient.EmailStatus, error)
}

// DocumentMail is one message about a document. It holds no address.
type DocumentMail struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	InvoiceID *int64    `json:"invoice_id"`
	PartyID   int64     `json:"party_id"`
	Step      int       `json:"step,omitempty"`
	Status    string    `json:"status"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// PDFIncomplete is set on the answer to a send when the linked PDF shows "?" for some text.
	PDFIncomplete bool `json:"pdf_incomplete,omitempty"`

	nonce, coreID string
	linkExpires   int64
}

const mailCols = `id, kind, invoice_id, party_id, step, status, nonce, core_id, link_expires, created_by, created_at, updated_at`

func scanMail(sc scanner) (*DocumentMail, error) {
	var m DocumentMail
	var inv sql.NullInt64
	var c, u int64
	if err := sc.Scan(&m.ID, &m.Kind, &inv, &m.PartyID, &m.Step, &m.Status, &m.nonce, &m.coreID, &m.linkExpires, &m.CreatedBy, &c, &u); err != nil {
		return nil, err
	}
	if inv.Valid {
		m.InvoiceID = &inv.Int64
	}
	m.CreatedAt, m.UpdatedAt = fromUnix(c), fromUnix(u)
	return &m, nil
}

func queryMails(ctx context.Context, q dbq, where string, args ...any) ([]DocumentMail, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+mailCols+` FROM document_mails WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []DocumentMail{}
	for rows.Next() {
		m, err := scanMail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// MailReady reports whether documents can be emailed.
func (s *Store) MailReady() bool {
	return s.sign.mailer != nil && s.docs.base != "" && len(s.docs.secret) > 0
}

func (s *Store) mailReady() error {
	if s.docs.base == "" {
		return fmt.Errorf("%w: emailing is off: the public invoice link is not published (company.yaml modules.finance.expose.public, docs/finance.md)", ErrConflict)
	}
	if !s.MailReady() {
		return fmt.Errorf("%w: emailing needs can-core's email service", ErrConflict)
	}
	return nil
}

// recipient checks that a party can be written to.
func recipient(p *Party) error {
	if p.Erased {
		return fmt.Errorf("%w: the party has been erased", ErrConflict)
	}
	if p.Email == "" {
		return invalid("email", "the customer has no email address")
	}
	return nil
}

// claimMail writes the row of a message about to be sent, with its audit entry. For a reminder it
// returns nil when the step was claimed before.
func (s *Store) claimMail(ctx context.Context, tx dbq, actor, kind, target string, invoiceID *int64, partyID int64, step int) (*DocumentMail, error) {
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	now := s.now()
	res, err := tx.ExecContext(ctx, `INSERT INTO document_mails (kind, invoice_id, party_id, step, status, nonce, link_expires, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		kind, invoiceID, partyID, step, "finmail-"+hex.EncodeToString(r[:]), ts(now.Add(mailLinkTTL)), actor, ts(now), ts(now))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	detail := map[string]any{"mail": id, "kind": kind, "party_id": partyID}
	if invoiceID != nil {
		detail["id"] = *invoiceID
	}
	if step > 0 {
		detail["step"] = step
	}
	if err := s.audit(ctx, tx, actor, "document.email", target, detail); err != nil {
		return nil, err
	}
	return scanMail(tx.QueryRowContext(ctx, `SELECT `+mailCols+` FROM document_mails WHERE id = ?`, id))
}

// manualLimit refuses a double click and a mail bomb. where selects the earlier sends of the
// same document.
func (s *Store) manualLimit(ctx context.Context, tx dbq, where string, arg int64) error {
	var recent, day int
	now := s.now()
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE created_at > ? AND status IN ('pending', 'queued', 'sent', 'unknown')), COUNT(*) FROM document_mails WHERE `+where+` AND created_at > ?`,
		ts(now.Add(-mailCooldown)), arg, ts(now.Add(-24*time.Hour))).Scan(&recent, &day); err != nil {
		return err
	}
	if recent > 0 {
		return fmt.Errorf("%w: this was emailed moments ago; wait a minute before sending it again", ErrConflict)
	}
	if day >= mailPerDay {
		return fmt.Errorf("%w: this was emailed %d times in the last 24 hours", ErrConflict, day)
	}
	return nil
}

// EmailInvoice mails an issued invoice or credit note to its party. When the mail service accepts
// it, an approved document becomes sent. The returned record says what happened.
func (s *Store) EmailInvoice(ctx context.Context, a *Principal, id int64) (*DocumentMail, error) {
	if err := s.mailReady(); err != nil {
		return nil, err
	}
	var m *DocumentMail
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		v, err := s.loadInvoice(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if v.Status == StDraft || v.Status == StVoid {
			return fmt.Errorf("%w: only an issued document that is not void can be emailed (it is %s)", ErrConflict, v.Status)
		}
		p, err := s.loadPartyRow(ctx, tx, nil, v.PartyID)
		if err != nil {
			return err
		}
		if err := recipient(p); err != nil {
			return err
		}
		if err := s.manualLimit(ctx, tx, `kind = 'document' AND invoice_id = ?`, id); err != nil {
			return err
		}
		m, err = s.claimMail(ctx, tx, a.Subject, DocDocument, deref(v.Number), &v.ID, v.PartyID, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.deliverMail(ctx, m.ID)
}

// EmailStatement mails a party the list of what it still owes, with a link to each invoice.
func (s *Store) EmailStatement(ctx context.Context, a *Principal, partyID int64) (*DocumentMail, error) {
	if err := s.wholeParty(a, partyID); err != nil {
		return nil, err
	}
	if err := s.mailReady(); err != nil {
		return nil, err
	}
	var m *DocumentMail
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		p, err := scanParty(tx.QueryRowContext(ctx, partySelect+` WHERE id = ?`, partyID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := recipient(p); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) `+openInvoices, partyID).Scan(&open); err != nil {
			return err
		}
		if open == 0 {
			return fmt.Errorf("%w: nothing is outstanding for this customer", ErrConflict)
		}
		if err := s.manualLimit(ctx, tx, `kind = 'statement' AND party_id = ?`, partyID); err != nil {
			return err
		}
		m, err = s.claimMail(ctx, tx, a.Subject, DocStatement, strconv.FormatInt(partyID, 10), nil, partyID, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.deliverMail(ctx, m.ID)
}

// wholeParty refuses a caller who cannot see everything about a party: a project-scoped token
// would get, or send, a partial picture.
func (s *Store) wholeParty(a *Principal, partyID int64) error {
	if !a.CanParty(partyID) {
		return ErrNotFound
	}
	if len(a.Projects) > 0 {
		return ErrForbidden
	}
	return nil
}

const openInvoices = `FROM invoices WHERE party_id = ? AND kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid') AND total_minor - paid_minor - credited_minor > 0`

// publicLink is the link a mail carries. It depends only on what is stored, so that handing the
// same message over twice sends the same bytes.
func (s *Store) publicLink(v *Invoice, expires int64) string {
	return s.docs.base + "/public/invoices/" + url.PathEscape(SignLink(s.docs.secret, v.ID, v.epoch, time.Unix(expires, 0)))
}

func (s *Store) sender() string {
	if s.docs.company != "" {
		return s.docs.company
	}
	return "A company you deal with"
}

// composeMail builds the message of a row from the records as they are now. A nil message with no
// error means it must not be sent any more.
func (s *Store) composeMail(ctx context.Context, tx dbq, m *DocumentMail) (*modclient.Email, error) {
	p, err := scanParty(tx.QueryRowContext(ctx, partySelect+` WHERE id = ?`, m.PartyID))
	if err != nil {
		return nil, err
	}
	if recipient(p) != nil {
		return nil, nil
	}
	all := &Principal{}
	var subject string
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	until := fromUnix(m.linkExpires).Format(time.DateOnly)
	w("Hello,")
	w("")
	if m.Kind == DocStatement {
		subject = "Statement from " + s.sender()
		w("This is your statement from %s as of %s. Still to be paid:", s.sender(), dateOf(m.CreatedAt))
		rows, err := tx.QueryContext(ctx, `SELECT id `+openInvoices+` ORDER BY due_date, id`, m.PartyID)
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
		if len(ids) == 0 {
			return nil, nil
		}
		owing := map[string]int64{}
		for i, id := range ids {
			v, err := s.loadInvoice(ctx, tx, all, id)
			if err != nil {
				return nil, err
			}
			owing[v.Currency] += v.BalanceMinor
			if i >= statementLines {
				continue
			}
			w("")
			w("%s, issued %s, due %s: %s %s", deref(v.Number), v.IssueDate, v.DueDate, FormatMinor(v.BalanceMinor, v.Currency), v.Currency)
			w("%s", s.publicLink(v, m.linkExpires))
		}
		if n := len(ids) - statementLines; n > 0 {
			w("")
			w("%d further invoices are not listed; they are included in the total.", n)
		}
		w("")
		for _, t := range sumByCurrency(owing) {
			w("Total outstanding: %s %s", FormatMinor(t.Minor, t.Currency), t.Currency)
		}
	} else {
		if m.InvoiceID == nil {
			return nil, nil
		}
		v, err := s.loadInvoice(ctx, tx, all, *m.InvoiceID)
		if err != nil {
			return nil, err
		}
		if v.Status == StDraft || v.Status == StVoid {
			return nil, nil
		}
		what, number := "invoice", deref(v.Number)
		if v.Kind == KindCreditNote {
			what = "credit note"
		}
		link := s.publicLink(v, m.linkExpires)
		if m.Kind == DocReminder {
			subject = "Payment reminder: invoice " + number + " from " + s.sender()
			w("Our records show that invoice %s from %s was due on %s and has not been paid in full.", number, s.sender(), v.DueDate)
			w("")
			w("Balance due: %s %s", FormatMinor(v.BalanceMinor, v.Currency), v.Currency)
			w("")
			w("If you have paid it in the last few days, or if you disagree with it, please tell us and ignore this message.")
		} else {
			subject = strings.ToUpper(what[:1]) + what[1:] + " " + number + " from " + s.sender()
			w("%s has sent you %s %s.", s.sender(), what, number)
			w("")
			w("Total: %s %s", FormatMinor(v.TotalMinor, v.Currency), v.Currency)
			if v.Kind == KindInvoice {
				w("Balance due: %s %s", FormatMinor(v.BalanceMinor, v.Currency), v.Currency)
				w("Due date: %s", v.DueDate)
			}
		}
		w("")
		w("View it online:")
		w("%s", link)
		w("")
		w("Download it as a PDF (nothing is attached to this message):")
		w("%s/pdf", link)
	}
	w("")
	w("The links work until %s. Anyone who has a link can see the document: forward it with care.", until)
	w("Replies to this address are not read; contact %s the way you usually do.", s.sender())
	return &modclient.Email{To: []string{p.Email}, Subject: mailSubject("", subject), Text: b.String(), SourceID: m.nonce}, nil
}

// deliverMail hands one pending (or waiting) row to the mail service and records the outcome. It
// returns the row as it is afterwards.
func (s *Store) deliverMail(ctx context.Context, id int64) (*DocumentMail, error) {
	var m *DocumentMail
	var msg *modclient.Email
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		msg = nil
		var err error
		if m, err = scanMail(tx.QueryRowContext(ctx, `SELECT `+mailCols+` FROM document_mails WHERE id = ?`, id)); err != nil {
			return err
		}
		if m.Status != DMPending && m.Status != DMRetry {
			return nil
		}
		// A reminder is checked again here: payment, voiding, a dispute or a stop since it was
		// claimed cancels it.
		due := true
		if m.Kind == DocReminder {
			if due, err = s.reminderDue(ctx, tx, *m.InvoiceID); err != nil {
				return err
			}
		}
		if due {
			if msg, err = s.composeMail(ctx, tx, m); err != nil {
				return err
			}
		}
		if msg == nil {
			return s.setMailStatus(ctx, tx, "can-finance", m, DMCancelled, "")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if msg != nil {
		status, coreID, lost := s.handOver(ctx, m.Kind, *msg)
		// Recorded even if the caller has gone: the message may already be on its way.
		rctx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if err := s.inTx(rctx, func(tx *sql.Tx) error {
			cur, err := scanMail(tx.QueryRowContext(rctx, `SELECT `+mailCols+` FROM document_mails WHERE id = ?`, id))
			if err != nil {
				return err
			}
			// It may have left, whatever becomes of the row: the next reminder keeps its distance.
			if lost {
				if _, err := tx.ExecContext(rctx, `UPDATE document_mails SET handed_at = ? WHERE id = ?`, ts(s.now()), id); err != nil {
					return err
				}
			}
			// A row cancelled while the call was in flight still records an acceptance.
			if cur.Status != DMPending && cur.Status != DMRetry && status != DMQueued {
				return nil
			}
			if err := s.setMailStatus(rctx, tx, "can-finance", cur, status, coreID); err != nil {
				return err
			}
			if status == DMQueued && cur.Kind == DocDocument {
				return s.markSent(rctx, tx, cur)
			}
			return nil
		}); err != nil {
			s.sign.log.Error("document mail: recording the outcome", "mail", id, "error", err)
			return nil, err
		}
		if status != DMQueued {
			s.sign.log.Warn("document mail not queued", "mail", id, "outcome", status)
		}
		ctx = rctx
	}
	return scanMail(s.db.QueryRowContext(ctx, `SELECT `+mailCols+` FROM document_mails WHERE id = ?`, id))
}

// handOver calls email.send. Error texts are not kept or logged: they may quote addresses.
//
// A reply that is lost leaves it open whether the message was accepted. A reminder is tried again
// later, and lost is reported so that the gap to the next one counts from now. A manual send is
// asked once more at once with the same key, which cannot make a second message; with still no
// answer it is "unknown", never "failed". A request that never left (connection refused) is not a
// lost reply: nothing was sent.
func (s *Store) handOver(ctx context.Context, kind string, msg modclient.Email) (status, coreID string, lost bool) {
	sendCtx, cancel := context.WithTimeout(ctx, mailBudget)
	defer cancel()
	status, coreID = s.sendOnce(sendCtx, kind, msg)
	if status == DMUnknown && kind != DocReminder {
		if again, id := s.sendOnce(sendCtx, kind, msg); again == DMQueued {
			return again, id, false
		}
	}
	if status == DMUnknown && kind == DocReminder {
		return DMRetry, "", true
	}
	return status, coreID, false
}

func (s *Store) sendOnce(ctx context.Context, kind string, msg modclient.Email) (status, coreID string) {
	var ids []string
	var err error
	if q, ok := s.sign.mailer.(queueMailer); ok {
		ids, err = q.QueueEmail(ctx, msg)
	} else {
		err = s.sign.mailer.SendEmail(ctx, msg)
	}
	if len(ids) > 0 {
		coreID = ids[0]
	}
	if err == nil {
		return DMQueued, coreID
	}
	var se *modclient.StatusError
	if !errors.As(err, &se) {
		if !errors.Is(err, modclient.ErrNotSent) {
			return DMUnknown, ""
		}
		if kind == DocReminder {
			return DMRetry, ""
		}
		return DMFailed, ""
	}
	switch se.Status {
	case http.StatusConflict:
		// The idempotency key is already known: an earlier attempt was accepted.
		return DMQueued, ""
	case http.StatusForbidden:
		return DMNotAllowed, ""
	case http.StatusBadRequest:
		return DMFailed, ""
	case http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		// can-core says it did not take it.
		if kind == DocReminder {
			return DMRetry, ""
		}
		return DMFailed, ""
	}
	return DMUnknown, ""
}

func (s *Store) setMailStatus(ctx context.Context, tx dbq, actor string, m *DocumentMail, status, coreID string) error {
	if coreID == "" {
		coreID = m.coreID
	}
	if status == m.Status && coreID == m.coreID {
		return nil
	}
	// handed_at is when it left (or may have left) for the mail service: reminders are spaced from it.
	// A waiting row that is accepted counts from now, whatever a lost reply stamped before.
	now := ts(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE document_mails SET status = ?, core_id = ?, updated_at = ?,
		handed_at = CASE WHEN ? IN ('queued', 'unknown') AND (handed_at = 0 OR status IN ('pending', 'retry')) THEN ? ELSE handed_at END WHERE id = ?`, status, coreID, now, status, now, m.ID); err != nil {
		return err
	}
	return s.audit(ctx, tx, actor, "document.email.outcome", strconv.FormatInt(m.ID, 10), map[string]any{"status": status})
}

// markSent records that a document has left the building, as "mark as sent" does. That includes
// one that was partly paid before anyone sent it: only a sent invoice is ever reminded about.
func (s *Store) markSent(ctx context.Context, tx dbq, m *DocumentMail) error {
	v, err := s.loadInvoice(ctx, tx, &Principal{}, *m.InvoiceID)
	if err != nil {
		return err
	}
	if v.SentAt != nil || v.Status == StDraft || v.Status == StVoid {
		return nil
	}
	return s.sendTx(ctx, tx, m.CreatedBy, v)
}

// RefreshMails asks the mail service what became of queued messages: all of them, or those of one
// invoice or party. It also gives up on a manual send that was interrupted: nobody retries those,
// and whether the message left cannot be known.
func (s *Store) RefreshMails(ctx context.Context, invoiceID, partyID int64) error {
	stuck, err := queryMails(ctx, s.db, `status = 'pending' AND kind <> 'reminder' AND updated_at < ? LIMIT ?`, ts(s.now().Add(-time.Hour)), statusRefreshLimit)
	if err != nil {
		return err
	}
	for _, m := range stuck {
		if err := s.inTx(ctx, func(tx *sql.Tx) error { return s.setMailStatus(ctx, tx, "can-finance", &m, DMUnknown, "") }); err != nil {
			return err
		}
	}
	q, ok := s.sign.mailer.(queueMailer)
	if !ok {
		return nil
	}
	limit := statusRefreshLimit
	if invoiceID == 0 && partyID == 0 {
		limit = statusSweepLimit
	}
	// A row without a can-core id (a repeated request) cannot be asked about; it only ages out, and
	// must not keep the others from being looked at.
	list, err := queryMails(ctx, s.db, `status = 'queued' AND (core_id <> '' OR created_at < ?) AND (? = 0 OR invoice_id = ?) AND (? = 0 OR party_id = ?) ORDER BY id LIMIT ?`,
		ts(s.now().Add(-coreMessageTTL)), invoiceID, invoiceID, partyID, partyID, limit)
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, m := range list {
		status := DMQueued
		if m.coreID != "" {
			st, err := q.EmailStatus(sendCtx, m.coreID)
			var se *modclient.StatusError
			switch {
			case errors.As(err, &se) && se.Status == http.StatusNotFound:
				status = DMUnknown
			case err != nil:
				return nil // can-core is not answering: what is stored stands
			case st.Status == "sent":
				status = DMSent
			case st.Status == "dead":
				status = DMFailed
			}
		}
		if status == DMQueued && s.now().Sub(m.CreatedAt) > coreMessageTTL {
			status = DMUnknown
		}
		if status == DMQueued {
			continue
		}
		if err := s.inTx(ctx, func(tx *sql.Tx) error { return s.setMailStatus(ctx, tx, "can-finance", &m, status, "") }); err != nil {
			return err
		}
	}
	return nil
}

// ListInvoiceMails returns the messages about one document, newest first, and its reminder state.
func (s *Store) ListInvoiceMails(ctx context.Context, a *Principal, id int64) ([]DocumentMail, *ReminderState, error) {
	v, err := s.GetInvoice(ctx, a, id)
	if err != nil {
		return nil, nil, err
	}
	if err := s.RefreshMails(ctx, id, 0); err != nil {
		return nil, nil, err
	}
	list, err := queryMails(ctx, s.db, `invoice_id = ? ORDER BY id DESC LIMIT 50`, id)
	if err != nil {
		return nil, nil, err
	}
	st, err := s.reminderState(ctx, id, v.PartyID)
	return list, st, err
}

// ListPartyMails returns the messages sent to a party, newest first, and whether reminders are
// stopped for it.
func (s *Store) ListPartyMails(ctx context.Context, a *Principal, partyID int64) ([]DocumentMail, *ReminderState, error) {
	if err := s.wholeParty(a, partyID); err != nil {
		return nil, nil, err
	}
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM parties WHERE id = ?`, partyID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	} else if err != nil {
		return nil, nil, err
	}
	if err := s.RefreshMails(ctx, 0, partyID); err != nil {
		return nil, nil, err
	}
	list, err := queryMails(ctx, s.db, `party_id = ? ORDER BY id DESC LIMIT 50`, partyID)
	if err != nil {
		return nil, nil, err
	}
	st, err := s.reminderState(ctx, 0, partyID)
	return list, st, err
}

// mailLoop sends due payment reminders and asks what became of queued mail, hourly.
func (a *App) mailLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	if p, err := a.store.Dunning(ctx); err == nil && p.RestoredAt != nil {
		a.log.Warn("payment reminders are off since a restore: turn them on again in finance, Settings, Payment reminders")
	}
	for {
		if n, err := a.store.RunDunning(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("payment reminders", "error", err)
		} else if n > 0 {
			a.log.Info("payment reminders queued", "count", n)
		}
		if err := a.store.RefreshMails(ctx, 0, 0); err != nil && ctx.Err() == nil {
			a.log.Error("mail delivery status", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// eraseMail runs inside a party erasure: what has not been handed over never will be. A message
// already in can-core's outbox cannot be recalled (docs/email.md, "Data kept").
func (s *Store) eraseMail(ctx context.Context, tx dbq, actor string, partyID int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE document_mails SET status = 'cancelled', updated_at = ? WHERE party_id = ? AND status IN ('pending', 'retry')`, ts(s.now()), partyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return s.audit(ctx, tx, actor, "document.email.cancel", strconv.FormatInt(partyID, 10), map[string]any{"count": n, "reason": "erasure"})
	}
	return nil
}
