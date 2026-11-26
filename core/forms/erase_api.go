package forms

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The erasure part of the leads capability. Every request needs leads.erase; an address is only
// ever sent in a request body, never in a URL, and is not logged or stored.

// channels are the places lead.erased.v1 goes.
func (a *leadsAPI) channels() (out []string) {
	if a.notifier.can != nil {
		out = append(out, "can")
	}
	if a.notifier.webhook != nil {
		out = append(out, "webhook")
	}
	return out
}

// erasureNotes are the copies an erasure cannot reach, said in every answer.
var erasureNotes = []string{
	"A notification e-mail already handed over cannot be recalled: can-core keeps a queued one until it is sent (up to seven days if it cannot be), and mailboxes keep what arrived.",
	"Copies outside forms (a webhook receiver, a CRM, a newsletter list, an export) get lead.erased.v1 with the id only, where they listen for it; otherwise erase them by hand.",
	"Backups made before the erasure hold the lead until they expire; the erasure ledger erases it again after a restore.",
}

type erasedAnswer struct {
	*ErasedLead
	// UnderWay: a notification was being sent when the lead was erased and may have arrived.
	UnderWay bool     `json:"notification_was_under_way,omitempty"`
	Notes    []string `json:"notes"`
}

// eraseLead erases and stops what is being sent. A nil answer with ok means there is no such lead;
// without ok the response is written.
func (a *leadsAPI) eraseLead(w http.ResponseWriter, r *http.Request, id, actor string) (answer *erasedAnswer, ok bool) {
	out, err := a.store.Erase(r.Context(), actor, id, a.channels())
	if errors.Is(err, ErrNotFound) {
		return nil, true
	}
	underWay := false
	if out != nil { // the row is gone, whether or not the scrub worked
		underWay = a.notifier.stop(id)
		a.notifier.kick()
		a.log.Info("audit", "action", "lead.erase", "actor", actor, "lead", id, "already_erased", out.AlreadyErased)
	}
	if errors.Is(err, errScrub) {
		a.log.Error("leads api", "error", err)
		writeError(w, http.StatusInternalServerError, "scrub_failed", errScrub.Error())
		return nil, false
	}
	if err != nil {
		a.internal(w, err)
		return nil, false
	}
	return &erasedAnswer{out, underWay, erasureNotes}, true
}

// erase is DELETE /v1/leads/{id}.
func (a *leadsAPI) erase(w http.ResponseWriter, r *http.Request, p *Principal) {
	id := r.PathValue("id")
	if !leadIDRe.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "no such lead")
		return
	}
	out, ok := a.eraseLead(w, r, id, p.Subject)
	switch {
	case !ok:
	case out == nil:
		writeError(w, http.StatusNotFound, "not_found", "no such lead")
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// readBody decodes exactly one small JSON object.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// find is POST /v1/leads/find: a search for an erasure request. It is a POST so that the address
// stays out of URLs, and with them out of proxy and access logs.
func (a *leadsAPI) find(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Email string `json:"email"`
	}
	if !readBody(w, r, &in) || !validSearchAddress(strings.TrimSpace(in.Email)) {
		writeError(w, http.StatusBadRequest, "bad_request", `body must be {"email":"<one address>"}`)
		return
	}
	items, truncated, err := a.store.FindByEmail(r.Context(), p.Subject, strings.TrimSpace(in.Email))
	if err != nil {
		a.internal(w, err)
		return
	}
	a.log.Info("audit", "action", "lead.find", "actor", p.Subject, "matches", len(items))
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": truncated})
}

// erasures is GET /v1/erasures?after=N: the tombstones can-core collects.
func (a *leadsAPI) erasures(w http.ResponseWriter, r *http.Request, _ *Principal) {
	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "after is invalid")
			return
		}
		after = n
	}
	items, err := a.store.ListErasures(r.Context(), after)
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// purge is POST /v1/purge {subject, class}: the hook `can restore` replays the ledger through.
// Idempotent; an unknown lead is a no-op. Lead ids are never reused, so a replay cannot destroy
// anything submitted later.
func (a *leadsAPI) purge(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Subject string `json:"subject"`
		Class   string `json:"class"`
	}
	if !readBody(w, r, &in) || in.Class != ClassLead || !leadIDRe.MatchString(in.Subject) {
		writeError(w, http.StatusBadRequest, "bad_request", "subject must be a lead id and class "+ClassLead)
		return
	}
	if _, ok := a.eraseLead(w, r, in.Subject, p.Subject); ok {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
