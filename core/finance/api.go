package finance

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// api implements the REST API (specs/014-finance-basics/contracts/openapi.yaml and the
// invoices.v1, contracts.v1 and timesheets.v1 capability schemas).
type api struct {
	store      *Store
	auth       Authenticator
	log        *slog.Logger
	cfg        *Config
	maxBody    int64
	trustProxy bool
	now        func() time.Time
	linkSecret []byte
	payments   PaymentProvider
	esign      ESigner
	seller     Seller
	limiter    *rateLimiter  // per principal
	failures   *rateLimiter  // failed authentications and bad links per client address
	penalty    time.Duration // delay added to failures from an address over the limit
}

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unhealthy", "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	read, write, approve, admin := PermRead, PermWrite, PermApprove, PermAdmin
	const biz, own = false, true // own: routes an own-only (employee) token may use

	mux.HandleFunc("GET /v1/me", a.guard(read, own, a.me))

	mux.HandleFunc("GET /v1/parties", a.guard(read, biz, a.listParties))
	mux.HandleFunc("POST /v1/parties", a.guard(write, biz, a.createParty))
	mux.HandleFunc("GET /v1/parties/{id}", a.guard(read, biz, a.getParty))
	mux.HandleFunc("PATCH /v1/parties/{id}", a.guard(write, biz, a.patchParty))
	mux.HandleFunc("POST /v1/parties/{id}/erase", a.guard(admin, biz, a.eraseParty))

	mux.HandleFunc("GET /v1/tax-rates", a.guard(read, biz, a.listTaxRates))
	mux.HandleFunc("POST /v1/tax-rates", a.guard(admin, biz, a.createTaxRate))
	mux.HandleFunc("PATCH /v1/tax-rates/{code}", a.guard(admin, biz, a.patchTaxRate))
	mux.HandleFunc("GET /v1/products", a.guard(read, biz, a.listProducts))
	mux.HandleFunc("POST /v1/products", a.guard(write, biz, a.createProduct))
	mux.HandleFunc("GET /v1/products/{sku}", a.guard(read, biz, a.getProduct))
	mux.HandleFunc("PATCH /v1/products/{sku}", a.guard(write, biz, a.patchProduct))

	mux.HandleFunc("GET /v1/invoices", a.guard(read, biz, a.listInvoices))
	mux.HandleFunc("POST /v1/invoices", a.guard(write, biz, a.createInvoice))
	mux.HandleFunc("GET /v1/invoices/{id}", a.guard(read, biz, a.getInvoice))
	mux.HandleFunc("PATCH /v1/invoices/{id}", a.guard(write, biz, a.patchInvoice))
	mux.HandleFunc("DELETE /v1/invoices/{id}", a.guard(write, biz, a.deleteInvoice))
	mux.HandleFunc("POST /v1/invoices/{id}/approve", a.guard(approve, biz, a.approveInvoice))
	mux.HandleFunc("POST /v1/invoices/{id}/send", a.guard(write, biz, a.sendInvoice))
	mux.HandleFunc("POST /v1/invoices/{id}/void", a.guard(approve, biz, a.voidInvoice))
	mux.HandleFunc("POST /v1/invoices/{id}/credit-notes", a.guard(write, biz, a.createCreditNote))
	mux.HandleFunc("GET /v1/invoices/{id}/html", a.guard(read, biz, a.invoiceHTML))
	mux.HandleFunc("POST /v1/invoices/{id}/link", a.guard(write, biz, a.issueLink))
	mux.HandleFunc("POST /v1/invoices/{id}/link/revoke", a.guard(write, biz, a.revokeLinks))
	mux.HandleFunc("POST /v1/invoices/{id}/checkout", a.guard(write, biz, a.checkout))
	mux.HandleFunc("GET /v1/invoices/{id}/payments", a.guard(read, biz, a.listPayments))
	mux.HandleFunc("POST /v1/invoices/{id}/payments", a.guard(write, biz, a.recordPayment))
	mux.HandleFunc("POST /v1/payments/import", a.guard(write, biz, a.importPayments))

	mux.HandleFunc("GET /v1/recurring", a.guard(read, biz, a.listRecurring))
	mux.HandleFunc("POST /v1/recurring", a.guard(write, biz, a.createRecurring))
	mux.HandleFunc("POST /v1/recurring/run", a.guard(admin, biz, a.runRecurring))
	mux.HandleFunc("POST /v1/recurring/{id}/status", a.guard(write, biz, a.recurringStatus))

	mux.HandleFunc("GET /v1/reports/aging", a.guard(read, biz, a.aging))
	mux.HandleFunc("GET /v1/exports/invoices.csv", a.guard(read, biz, a.exportInvoices))
	mux.HandleFunc("GET /v1/exports/payments.csv", a.guard(read, biz, a.exportPayments))
	mux.HandleFunc("GET /v1/exports/expenses.csv", a.guard(read, biz, a.exportExpenses))

	mux.HandleFunc("GET /v1/contract-templates", a.guard(read, biz, a.listTemplates))
	mux.HandleFunc("POST /v1/contract-templates", a.guard(write, biz, a.createTemplate))
	mux.HandleFunc("GET /v1/contracts", a.guard(read, biz, a.listContracts))
	mux.HandleFunc("POST /v1/contracts", a.guard(write, biz, a.createContract))
	mux.HandleFunc("POST /v1/contracts/sweep", a.guard(admin, biz, a.sweepContracts))
	mux.HandleFunc("GET /v1/contracts/{id}", a.guard(read, biz, a.getContract))
	mux.HandleFunc("POST /v1/contracts/{id}/versions", a.guard(write, biz, a.newVersion))
	mux.HandleFunc("GET /v1/contracts/{id}/versions/{n}", a.guard(read, biz, a.getVersion))
	mux.HandleFunc("POST /v1/contracts/{id}/send", a.guard(write, biz, a.sendContract))
	mux.HandleFunc("POST /v1/contracts/{id}/signature", a.guard(write, biz, a.recordSignature))
	mux.HandleFunc("PATCH /v1/contracts/{id}/dates", a.guard(write, biz, a.contractDates))
	mux.HandleFunc("POST /v1/contracts/{id}/terminate", a.guard(write, biz, a.terminateContract))
	mux.HandleFunc("GET /v1/contracts/{id}/verify", a.guard(read, biz, a.verifyContract))

	mux.HandleFunc("GET /v1/timesheets", a.guard(read, own, a.listTimesheets))
	mux.HandleFunc("POST /v1/timesheets", a.guard(write, own, a.createTimesheet))
	mux.HandleFunc("POST /v1/timesheets/billing-draft", a.guard(write, biz, a.billTime))
	mux.HandleFunc("GET /v1/timesheets/{id}", a.guard(read, own, a.getTimesheet))
	mux.HandleFunc("PUT /v1/timesheets/{id}/entries", a.guard(write, own, a.setEntries))
	mux.HandleFunc("POST /v1/timesheets/{id}/submit", a.guard(write, own, a.submitTimesheet))
	mux.HandleFunc("POST /v1/timesheets/{id}/approve", a.guard(approve, biz, a.approveTimesheet))
	mux.HandleFunc("POST /v1/timesheets/{id}/reject", a.guard(approve, biz, a.rejectTimesheet))
	mux.HandleFunc("GET /v1/period-lock", a.guard(read, own, a.getLock))
	mux.HandleFunc("PUT /v1/period-lock", a.guard(admin, biz, a.setLock))

	mux.HandleFunc("GET /v1/expenses", a.guard(read, own, a.listExpenses))
	mux.HandleFunc("POST /v1/expenses", a.guard(write, own, a.createExpense))
	mux.HandleFunc("GET /v1/expenses/{id}", a.guard(read, own, a.getExpense))
	mux.HandleFunc("PUT /v1/expenses/{id}", a.guard(write, own, a.updateExpense))
	mux.HandleFunc("POST /v1/expenses/{id}/submit", a.guard(write, own, a.submitExpense))
	mux.HandleFunc("POST /v1/expenses/{id}/approve", a.guard(approve, biz, a.approveExpense))
	mux.HandleFunc("POST /v1/expenses/{id}/reject", a.guard(approve, biz, a.rejectExpense))
	mux.HandleFunc("POST /v1/expenses/{id}/reimburse", a.guard(write, biz, a.reimburseExpense))

	mux.HandleFunc("GET /v1/purchase-orders", a.guard(read, biz, a.listPOs))
	mux.HandleFunc("POST /v1/purchase-orders", a.guard(write, biz, a.createPO))
	mux.HandleFunc("GET /v1/purchase-orders/{id}", a.guard(read, biz, a.getPO))
	mux.HandleFunc("PUT /v1/purchase-orders/{id}", a.guard(write, biz, a.updatePO))
	mux.HandleFunc("POST /v1/purchase-orders/{id}/approve", a.guard(approve, biz, a.approvePO))
	mux.HandleFunc("POST /v1/purchase-orders/{id}/status", a.guard(write, biz, a.poStatus))

	mux.HandleFunc("GET /v1/tokens", a.guard(admin, biz, a.listTokens))
	mux.HandleFunc("POST /v1/tokens", a.guard(admin, biz, a.createToken))
	mux.HandleFunc("DELETE /v1/tokens/{id}", a.guard(admin, biz, a.revokeToken))
	mux.HandleFunc("GET /v1/audit", a.guard(admin, biz, a.listAudit))
	mux.HandleFunc("GET /v1/events/dead", a.guard(admin, biz, a.listDead))
	mux.HandleFunc("POST /v1/events/{id}/replay", a.guard(admin, biz, a.replayEvent))

	// Not bearer-authenticated: a signed link and provider webhooks (their own signatures).
	mux.HandleFunc("GET /public/invoices/{token}", a.publicInvoice)
	mux.HandleFunc("POST /v1/webhooks/payments", a.paymentWebhook)
	mux.HandleFunc("POST /v1/webhooks/esign", a.esignWebhook)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		mux.ServeHTTP(w, r)
	})
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, p *Principal)

// guard authenticates, rate-limits, checks the permission and caps the body before the
// handler runs. Every request is authenticated first, so failures from a shared address (an
// ingress, a NAT) never lock out a caller who has a valid token; a valid token is limited only
// by its own per-token budget (rate_limit_per_minute, 600 by default). Only failures are
// counted per client address; past the limit a failure is answered 429 after a
// short delay, which makes a flood of guesses slow and costly without affecting valid callers.
// An own-only token (given to an employee for their timesheets and expenses) is refused on
// every route that is not marked ownable.
func (a *api) guard(perm string, ownable bool, next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.Authenticate(r)
		if err != nil {
			a.reject(w, r)
			return
		}
		key := p.TokenID
		if key == "" {
			key = "subject:" + p.Subject
		}
		if ok, wait := a.limiter.allow(key); !ok {
			tooMany(w, wait)
			return
		}
		if !p.Can(perm) || (p.OwnOnly && !ownable) {
			writeError(w, http.StatusForbidden, "forbidden", "missing scope "+perm)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, a.maxBody)
		next(w, r, p)
	}
}

// reject answers an unauthenticated request. A failure is counted against the client
// address; past the limit it is answered 429 after a short delay, which makes a flood of
// guesses slow and costly. It never affects a request that authenticated.
func (a *api) reject(w http.ResponseWriter, r *http.Request) {
	if ok, wait := a.failures.allow(clientIP(r, a.trustProxy)); !ok {
		select {
		case <-time.After(a.penalty):
		case <-r.Context().Done():
		}
		tooMany(w, wait)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="can-finance"`)
	writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
}

func (a *api) fail(w http.ResponseWriter, err error) { writeStoreError(w, a.log, err) }

func queryInt(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

// pathID reads a positive integer path value; anything else is a 404, like an unknown id.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n < 1 {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return 0, false
	}
	return n, true
}

// ifMatchHeader parses If-Match; false means the header was malformed and the answer is written.
func ifMatchHeader(w http.ResponseWriter, r *http.Request) (*int64, bool) {
	v, ok := parseIfMatch(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match must be a version number, as in the ETag")
	}
	return v, ok
}

func validIdemHeader(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return "", true
	}
	if len(k) < 8 || len(k) > 128 || strings.ContainsAny(k, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "bad_request", "Idempotency-Key must be 8 to 128 characters")
		return "", false
	}
	return k, true
}

func (a *api) page(w http.ResponseWriter, items any, next int64) {
	out := map[string]any{"items": items}
	if next > 0 {
		out["next_cursor"] = encodeCursor(next)
	}
	writeJSON(w, http.StatusOK, out)
}

// cursor reads the cursor query parameter; false means it was malformed and the answer is written.
func cursor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	n, ok := decodeCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
	}
	return n, ok
}

func (a *api) me(w http.ResponseWriter, _ *http.Request, p *Principal) {
	parties, projects := p.Parties, p.Projects
	if parties == nil {
		parties = []int64{}
	}
	if projects == nil {
		projects = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subject": p.Subject, "kind": p.Kind, "token_id": p.TokenID, "scopes": p.Permissions, "parties": parties, "projects": projects, "own_only": p.OwnOnly})
}
