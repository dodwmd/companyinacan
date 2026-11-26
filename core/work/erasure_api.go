package work

import (
	"errors"
	"net/http"
	"strconv"
)

// erasureNotes are what erasing a person does not do, said in every answer.
var erasureNotes = []string{
	"Texts that mention the person (titles, descriptions, comments, file names) and the admin audit log are not changed: edit them by hand if they must go.",
	"Other spellings of the same person are separate identifiers: erase each one.",
	"Copies outside the tracker (webhook receivers, can-core's queued events, exports) and backups made before the erasure are not changed.",
}

// scrubPendingNote is said when the erasure is committed but the database files were not scrubbed.
const scrubPendingNote = "NOT FINISHED: the person is erased, but scrubbing the database files failed, so the identifier may still be readable in them. It is retried by itself; check the log before relying on the files or copies of them."

// erasePerson is POST /v1/people/erase. The identifier is not logged.
func (a *api) erasePerson(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in ErasurePersonRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.ErasePerson(r.Context(), p, in.Subject)
	switch {
	case errors.Is(err, errScrub):
		a.log.Error("erasure follow-up failed", "erasure", out.Subject, "error", err)
	case err != nil:
		a.fail(w, err)
		return
	}
	a.log.Info("audit", "action", "person.erase", "erasure", out.Subject, "already_erased", out.AlreadyErased)
	notes := erasureNotes
	if out.ScrubPending {
		notes = append([]string{scrubPendingNote}, erasureNotes...)
	}
	writeJSON(w, http.StatusOK, struct {
		*ErasureResult
		Notes []string `json:"notes"`
	}{out, notes})
}

// erasures is GET /v1/erasures?after=N: the tombstones, for the collector of the erasure ledger.
func (a *api) erasures(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !p.Unscoped() {
		a.fail(w, ErrForbidden)
		return
	}
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
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
