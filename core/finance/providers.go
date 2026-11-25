package finance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file holds the two adapter seams that touch the outside world: card/bank payment
// providers and e-signature services. Both come with a fake that never opens a socket, so
// the whole module is testable offline. Real adapters (Stripe for payments, Documenso for
// e-signature) implement the same interfaces; docs/finance.md describes the path.

// ---- payment provider ---------------------------------------------------------------------

// CheckoutRequest asks a provider for a hosted payment page for the invoice's balance.
type CheckoutRequest struct {
	InvoiceID   int64
	Number      string
	Currency    string
	AmountMinor int64
}

// CheckoutSession is a hosted payment page.
type CheckoutSession struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PaymentNotice is a verified "payment succeeded" notification.
type PaymentNotice struct {
	EventID     string
	InvoiceID   int64
	AmountMinor int64
	Currency    string
}

// PaymentProvider is the Stripe-like adapter interface. ParseWebhook must verify the
// provider's signature and return an error for anything it cannot authenticate.
type PaymentProvider interface {
	Name() string
	CreateCheckout(ctx context.Context, r CheckoutRequest) (*CheckoutSession, error)
	ParseWebhook(h http.Header, body []byte) (*PaymentNotice, error)
}

// ErrBadSignature is returned for a webhook that does not verify.
var ErrBadSignature = errors.New("bad signature")

// FakePayments is a deterministic provider: no network, signatures like Stripe's
// ("t=<unix>,v1=<hex HMAC-SHA256 of t.body>") so a real adapter changes only the HTTP calls.
type FakePayments struct {
	Secret []byte
	Now    func() time.Time
	mu     sync.Mutex
	n      int
}

// Name implements PaymentProvider.
func (f *FakePayments) Name() string { return "fake" }

func (f *FakePayments) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// CreateCheckout implements PaymentProvider.
func (f *FakePayments) CreateCheckout(_ context.Context, r CheckoutRequest) (*CheckoutSession, error) {
	if r.AmountMinor <= 0 {
		return nil, errors.New("nothing to pay")
	}
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	id := fmt.Sprintf("fake_cs_%d_%d", r.InvoiceID, n)
	// .invalid is reserved (RFC 2606): it can never resolve, so nothing here reaches a network.
	return &CheckoutSession{ID: id, URL: "https://pay.example.invalid/checkout/" + id, ExpiresAt: f.now().Add(24 * time.Hour).UTC()}, nil
}

// SignFake produces the signature header the fake provider expects.
func SignFake(secret []byte, at time.Time, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(at.Unix(), 10) + "."))
	m.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(m.Sum(nil)))
}

const webhookTolerance = 5 * time.Minute

func verifyFake(secret []byte, now time.Time, header string, body []byte) error {
	var t int64
	var sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sig = v
		}
	}
	if t == 0 || sig == "" || len(secret) == 0 {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(t, 0)); d > webhookTolerance || d < -webhookTolerance {
		return fmt.Errorf("%w: timestamp outside the %s tolerance", ErrBadSignature, webhookTolerance)
	}
	want := SignFake(secret, time.Unix(t, 0), body)
	_, wantSig, _ := strings.Cut(want, ",v1=")
	if !hmac.Equal([]byte(wantSig), []byte(sig)) {
		return ErrBadSignature
	}
	return nil
}

// ParseWebhook implements PaymentProvider. The body is
// {"id":"evt_x","type":"payment.succeeded","invoice_id":1,"amount_minor":1000,"currency":"USD"}.
func (f *FakePayments) ParseWebhook(h http.Header, body []byte) (*PaymentNotice, error) {
	if err := verifyFake(f.Secret, f.now(), h.Get("X-Fake-Signature"), body); err != nil {
		return nil, err
	}
	var ev struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		InvoiceID   int64  `json:"invoice_id"`
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("bad webhook body: %w", err)
	}
	if ev.Type != "payment.succeeded" || ev.ID == "" || ev.InvoiceID < 1 || ev.AmountMinor < 1 {
		return nil, errors.New("not a usable payment.succeeded event")
	}
	return &PaymentNotice{EventID: ev.ID, InvoiceID: ev.InvoiceID, AmountMinor: ev.AmountMinor, Currency: ev.Currency}, nil
}

// CreateCheckout asks the configured provider for a payment page for an invoice's balance.
func (s *Store) CreateCheckout(ctx context.Context, a *Principal, prov PaymentProvider, invoiceID int64) (*CheckoutSession, error) {
	if prov == nil {
		return nil, fmt.Errorf("%w: no payment provider is configured", ErrConflict)
	}
	inv, err := s.GetInvoice(ctx, a, invoiceID)
	if err != nil {
		return nil, err
	}
	if inv.Kind != KindInvoice || (inv.Status != StApproved && inv.Status != StSent && inv.Status != StPartiallyPaid) || inv.BalanceMinor <= 0 {
		return nil, fmt.Errorf("%w: invoice is %s and has nothing to pay", ErrConflict, inv.Status)
	}
	return prov.CreateCheckout(ctx, CheckoutRequest{InvoiceID: inv.ID, Number: deref(inv.Number), Currency: inv.Currency, AmountMinor: inv.BalanceMinor})
}

// RecordProviderPayment records a verified provider notification. It is idempotent on the
// provider's event id.
func (s *Store) RecordProviderPayment(ctx context.Context, prov PaymentProvider, n *PaymentNotice) (*Payment, bool, error) {
	var out *Payment
	var dup bool
	err := s.inTx(ctx, func(tx2 *sql.Tx) error {
		var err error
		out, dup, err = s.recordPaymentTx(ctx, tx2, nil, "provider:"+prov.Name(), n.InvoiceID,
			PaymentInput{AmountMinor: n.AmountMinor, Currency: n.Currency, Method: prov.Name(), Reference: n.EventID, ExternalID: n.EventID}, "provider")
		return err
	})
	return out, dup, err
}

// ---- e-signature --------------------------------------------------------------------------

// SignRequest asks an e-signature service to collect signatures on a document.
type SignRequest struct {
	ContractID int64
	Title      string
	Body       string
	SHA256     string   // hash of the body the signers will see
	Signers    []string // recipient addresses, handed to the service and not stored here
}

// SignNotice is a verified "document signed" notification.
type SignNotice struct {
	EventID    string
	ExternalID string
	SHA256     string // hash of the document the service says was signed
}

// ESigner is the e-signature adapter interface. A Documenso adapter creates a document from
// the body, adds recipients and registers a webhook; the fake does none of that.
type ESigner interface {
	Name() string
	Request(ctx context.Context, r SignRequest) (externalID string, err error)
	ParseWebhook(h http.Header, body []byte) (*SignNotice, error)
}

// FakeESign never touches a network. Its webhook uses the same signature as FakePayments.
type FakeESign struct {
	Secret []byte
	Now    func() time.Time
	mu     sync.Mutex
	n      int
}

// Name implements ESigner.
func (f *FakeESign) Name() string { return "fake" }

// Request implements ESigner.
func (f *FakeESign) Request(_ context.Context, r SignRequest) (string, error) {
	if len(r.Signers) == 0 {
		return "", errors.New("at least one signer is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return fmt.Sprintf("fake_doc_%d_%d", r.ContractID, f.n), nil
}

// ParseWebhook implements ESigner. Body: {"id","type":"document.signed","document_id","sha256"}.
func (f *FakeESign) ParseWebhook(h http.Header, body []byte) (*SignNotice, error) {
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	if err := verifyFake(f.Secret, now, h.Get("X-Fake-Signature"), body); err != nil {
		return nil, err
	}
	var ev struct {
		ID, Type   string
		DocumentID string `json:"document_id"`
		SHA256     string `json:"sha256"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("bad webhook body: %w", err)
	}
	if ev.Type != "document.signed" || ev.ID == "" || ev.DocumentID == "" {
		return nil, errors.New("not a usable document.signed event")
	}
	return &SignNotice{EventID: ev.ID, ExternalID: ev.DocumentID, SHA256: ev.SHA256}, nil
}
