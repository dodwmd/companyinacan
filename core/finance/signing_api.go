package finance

import (
	"io"
	"net/http"
)

// The staff side of signing by link (specs/049-contract-esign). No response ever carries a link.

func (a *api) respondEnvelope(w http.ResponseWriter, status int, e *Envelope, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, status, e)
}

func (a *api) getSigning(w http.ResponseWriter, r *http.Request, p *Principal) {
	if id, ok := pathID(w, r, "id"); ok {
		e, err := a.store.Signing(r.Context(), p, id)
		a.respondEnvelope(w, http.StatusOK, e, err)
	}
}

func (a *api) sendForSigning(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	im, ok := ifMatchHeader(w, r)
	if !ok {
		return
	}
	var in SigningRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	e, err := a.store.SendForSigning(r.Context(), p, id, im, in)
	a.respondEnvelope(w, http.StatusCreated, e, err)
}

func (a *api) revokeSigning(w http.ResponseWriter, r *http.Request, p *Principal) {
	if id, ok := pathID(w, r, "id"); ok {
		e, err := a.store.RevokeSigning(r.Context(), p, id)
		a.respondEnvelope(w, http.StatusOK, e, err)
	}
}

func (a *api) resendSigning(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	signer, ok := pathID(w, r, "signer")
	if !ok {
		return
	}
	e, err := a.store.ResendInvitation(r.Context(), p, id, signer)
	a.respondEnvelope(w, http.StatusOK, e, err)
}

func (a *api) signingCertificate(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	text, err := a.store.Certificate(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="certificate.txt"`)
	_, _ = io.WriteString(w, text)
}
