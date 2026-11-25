package finance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"
)

// Seller is the issuer block printed on invoices (config, not stored per invoice).
type Seller struct {
	Name    string
	Address string
	TaxID   string
}

// ---- signed, expiring invoice links -------------------------------------------------------

// A public link is base64url("<invoice id>.<epoch>.<expiry unix>") + "." + base64url(HMAC).
// The epoch is stored on the invoice: bumping it revokes every link issued before. Nothing
// secret is in the link, only a signature, and no database lookup is needed to reject a
// forged or expired one.

const (
	maxLinkTTL     = 90 * 24 * time.Hour
	defaultLinkTTL = 14 * 24 * time.Hour
)

// ErrBadLink is returned for a link that is malformed, forged, expired or revoked. The
// public endpoint answers all of them with the same 404.
var ErrBadLink = errors.New("invalid link")

// SignLink issues a link token.
func SignLink(secret []byte, invoiceID, epoch int64, exp time.Time) string {
	payload := fmt.Sprintf("%d.%d.%d", invoiceID, epoch, exp.Unix())
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(linkMAC(secret, payload))
}

func linkMAC(secret []byte, payload string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("finance-invoice-link\x00" + payload))
	return m.Sum(nil)
}

// VerifyLink checks the signature and expiry and returns the invoice id and epoch.
func VerifyLink(secret []byte, token string, now time.Time) (id, epoch int64, err error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 200 {
		return 0, 0, ErrBadLink
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || len(secret) == 0 {
		return 0, 0, ErrBadLink
	}
	if !hmac.Equal(mac, linkMAC(secret, string(raw))) {
		return 0, 0, ErrBadLink
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 {
		return 0, 0, ErrBadLink
	}
	var n [3]int64
	for i, s := range parts {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, 0, ErrBadLink
		}
		n[i] = v
	}
	if !now.Before(time.Unix(n[2], 0)) {
		return 0, 0, ErrBadLink
	}
	return n[0], n[1], nil
}

// IssueLink returns a signed link token for an invoice in the caller's scope. Issuing a
// link for a draft is refused: drafts are not documents yet.
func (s *Store) IssueLink(ctx context.Context, a *Principal, secret []byte, id int64, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = defaultLinkTTL
	}
	if ttl > maxLinkTTL {
		return "", time.Time{}, invalid("ttl_hours", "at most %d hours", int(maxLinkTTL.Hours()))
	}
	v, err := s.GetInvoice(ctx, a, id)
	if err != nil {
		return "", time.Time{}, err
	}
	if v.Status == StDraft {
		return "", time.Time{}, fmt.Errorf("%w: approve the invoice before sharing it", ErrConflict)
	}
	exp := s.now().Add(ttl).UTC()
	if err := s.auditRead(ctx, a, "invoice.link_issue", deref(v.Number), map[string]any{"id": id, "expires_at": exp}); err != nil {
		return "", time.Time{}, err
	}
	return SignLink(secret, id, v.epoch, exp), exp, nil
}

// PublicInvoice resolves a link to the invoice it shows. Every failure is ErrNotFound-like
// from the caller's view (the API answers 404 for all of them). Views are audited, coalesced
// to one entry per invoice per ten minutes so a valid link cannot be used to flood the log.
func (s *Store) PublicInvoice(ctx context.Context, secret []byte, token string) (*Invoice, error) {
	id, epoch, err := VerifyLink(secret, token, s.now())
	if err != nil {
		return nil, ErrNotFound
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	v, err := s.loadInvoice(rctx, s.rdb, &Principal{}, id)
	if err != nil || v.epoch != epoch || v.Status == StDraft {
		return nil, ErrNotFound
	}
	target := deref(v.Number)
	var last sql.NullInt64
	_ = s.rdb.QueryRowContext(rctx, `SELECT MAX(at) FROM audit_log WHERE action = 'invoice.public_view' AND target = ?`, target).Scan(&last)
	if !last.Valid || ts(s.now())-last.Int64 >= 600 {
		if err := s.auditRead(ctx, &Principal{Subject: "public-link"}, "invoice.public_view", target, map[string]any{"id": id}); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// ---- HTML ---------------------------------------------------------------------------------

var invoiceTmpl = template.Must(template.New("invoice").Funcs(template.FuncMap{
	"money": FormatMinor,
	"qty": func(milli int64) string {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%03d", milli/1000, milli%1000), "0"), ".")
	},
	"pct": func(ppm int64) string {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%04d", ppm/10000, ppm%10000), "0"), ".")
	},
	"bp": func(b int64) string {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%02d", b/100, b%100), "0"), ".")
	},
}).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{if .Number}}{{.Number}}{{else}}Draft{{end}}</title>
<style>
body{font:14px/1.45 system-ui,sans-serif;color:#111;max-width:46em;margin:2em auto;padding:0 1em}
h1{font-size:1.6em;margin:.2em 0}
table{border-collapse:collapse;width:100%;margin:1em 0}
th,td{text-align:left;padding:.35em .5em;border-bottom:1px solid #ccc;vertical-align:top}
th.n,td.n{text-align:right;font-variant-numeric:tabular-nums}
.meta{display:flex;gap:2em;flex-wrap:wrap}.meta div{min-width:12em;white-space:pre-line}
.void{border:3px solid #a00;color:#a00;padding:.4em 1em;font-weight:700;display:inline-block;transform:rotate(-3deg)}
.pay{white-space:pre-wrap;border:1px solid #ccc;padding:.6em .8em;margin-top:1em}
.foot{color:#555;font-size:12px;margin-top:2em}
@media print{body{margin:0;max-width:none}.noprint{display:none}}
</style></head><body>
<h1>{{if eq .Kind "credit_note"}}Credit note{{else}}Invoice{{end}} {{if .Number}}{{.Number}}{{end}}</h1>
{{if eq .Status "void"}}<p class="void">VOID{{if .VoidReason}}: {{.VoidReason}}{{end}}</p>{{end}}
{{if eq .Status "draft"}}<p class="void">DRAFT</p>{{end}}
<div class="meta">
<div><strong>From</strong>
{{.Seller.Name}}
{{.Seller.Address}}{{if .Seller.TaxID}}
Tax ID: {{.Seller.TaxID}}{{end}}</div>
<div><strong>To</strong>
{{with .PartySnapshot}}{{.Name}}
{{.Address}}{{if .TaxID}}
Tax ID: {{.TaxID}}{{end}}{{else}}(buyer is frozen when the invoice is approved){{end}}</div>
<div>Issued: {{.IssueDate}}
Due: {{.DueDate}}
Currency: {{.Currency}}{{if ne .Currency .BaseCurrency}}
Rate: 1 {{.Currency}} = {{.FXRate}} {{.BaseCurrency}}{{end}}</div>
</div>
<table><thead><tr><th>Description</th><th class="n">Qty</th><th class="n">Unit price</th><th class="n">Disc. %</th><th class="n">Tax %</th><th class="n">Amount</th></tr></thead><tbody>
{{range .Lines}}<tr><td>{{.Description}}</td><td class="n">{{qty .QuantityMilli}}</td><td class="n">{{money .UnitPriceMinor $.Currency}}</td><td class="n">{{bp .DiscountBP}}</td><td class="n">{{pct .TaxRatePPM}}</td><td class="n">{{money .TotalMinor $.Currency}}</td></tr>
{{end}}</tbody>
<tfoot>
<tr><td colspan="5" class="n">Subtotal</td><td class="n">{{money .SubtotalMinor .Currency}}</td></tr>
{{if .DiscountMinor}}<tr><td colspan="5" class="n">Discounts</td><td class="n">-{{money .DiscountMinor .Currency}}</td></tr>{{end}}
<tr><td colspan="5" class="n">Tax</td><td class="n">{{money .TaxMinor .Currency}}</td></tr>
<tr><th colspan="5" class="n">Total {{.Currency}}</th><th class="n">{{money .TotalMinor .Currency}}</th></tr>
{{if .PaidMinor}}<tr><td colspan="5" class="n">Paid</td><td class="n">-{{money .PaidMinor .Currency}}</td></tr>{{end}}
{{if .CreditedMinor}}<tr><td colspan="5" class="n">Credited</td><td class="n">-{{money .CreditedMinor .Currency}}</td></tr>{{end}}
{{if eq .Kind "invoice"}}<tr><th colspan="5" class="n">Balance due</th><th class="n">{{money .BalanceMinor .Currency}}</th></tr>{{end}}
</tfoot></table>
{{if .Notes}}<p>{{.Notes}}</p>{{end}}
{{if and .PaymentInstructions (eq .Kind "invoice")}}<div class="pay"><strong>How to pay</strong>
{{.PaymentInstructions}}</div>{{end}}
<p class="foot">Generated by can-finance. This document is produced by software and is not tax, accounting or legal advice.</p>
</body></html>`))

type invoiceView struct {
	*Invoice
	Seller Seller
	Number string
}

// RenderInvoiceHTML renders a print-friendly invoice. html/template escapes every value, so a
// description such as "<script>" is shown as text.
func RenderInvoiceHTML(v *Invoice, seller Seller) ([]byte, error) {
	var b bytes.Buffer
	num := ""
	if v.Number != nil {
		num = *v.Number
	}
	if err := invoiceTmpl.Execute(&b, invoiceView{Invoice: v, Seller: seller, Number: num}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
