package board

import (
	"context"
	"net/http"
)

// Handler returns the HTTP handler. Every route except /healthz authenticates first and then asks the
// Service, which asks the policy layer.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Svc.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, apiError{"unhealthy", "database unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	a.routes = []string{"GET /healthz"}
	route := func(pattern string, fn handlerFn) {
		a.routes = append(a.routes, pattern)
		mux.HandleFunc(pattern, a.wrap(fn))
	}
	svc := func() *Service { return a.Svc }
	idemKey := func(r *http.Request) string { return r.Header.Get("Idempotency-Key") }
	type ctxT = context.Context
	pv := func(r *http.Request, n string) string { return r.PathValue(n) }
	q := func(r *http.Request, n string) string { return r.URL.Query().Get(n) }

	// ---- bodies and members
	route("POST /v1/bodies", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in BodyInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		b, err := svc().CreateBody(ctx, ac, in)
		if err != nil {
			return result{}, err
		}
		return created(b)
	})
	route("GET /v1/bodies", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListBodies(ctx, ac)
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/bodies/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		b, err := svc().GetBody(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(b)
	})
	route("POST /v1/bodies/{id}/members", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in MemberInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		m, err := svc().AddMember(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(m)
	})
	route("GET /v1/bodies/{id}/members", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListMembers(ctx, ac, pv(r, "id"), q(r, "as_of"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("POST /v1/bodies/{id}/members/{mid}/end", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			EndDate string `json:"end_date"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		m, err := svc().EndMember(ctx, ac, pv(r, "id"), pv(r, "mid"), in.EndDate)
		if err != nil {
			return result{}, err
		}
		return ok(m)
	})

	// ---- meetings, agenda, attendance, quorum, pack
	route("POST /v1/bodies/{id}/meetings", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in MeetingInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		m, err := svc().ScheduleMeeting(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(m)
	})
	route("GET /v1/bodies/{id}/meetings", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListMeetings(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/meetings/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		m, err := svc().GetMeeting(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(m)
	})
	route("POST /v1/meetings/{id}/status", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Status string `json:"status"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		m, err := svc().SetMeetingStatus(ctx, ac, pv(r, "id"), in.Status)
		if err != nil {
			return result{}, err
		}
		return ok(m)
	})
	route("POST /v1/meetings/{id}/agenda", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in AgendaInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		it, err := svc().AddAgendaItem(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(it)
	})
	route("GET /v1/meetings/{id}/agenda", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListAgenda(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("PUT /v1/meetings/{id}/attendance/{person}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in AttendanceInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		at, err := svc().RecordAttendance(ctx, ac, pv(r, "id"), pv(r, "person"), in.Status)
		if err != nil {
			return result{}, err
		}
		return ok(at)
	})
	route("GET /v1/meetings/{id}/attendance", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListAttendance(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/meetings/{id}/quorum", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		qr, err := svc().Quorum(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(qr)
	})
	route("POST /v1/meetings/{id}/pack/items", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in PackInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		it, err := svc().AddPackItem(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(it)
	})
	route("POST /v1/meetings/{id}/pack/issue", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		m, err := svc().IssuePack(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(m)
	})
	route("GET /v1/meetings/{id}/pack", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		p, err := svc().GetPack(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(p)
	})

	// ---- resolutions
	route("POST /v1/bodies/{id}/resolutions", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in ResolutionInput
		raw, err := decode(r, &in)
		if err != nil {
			return result{}, err
		}
		id, replay, err := svc().CreateResolution(ctx, ac, pv(r, "id"), in, idemKey(r), raw)
		if err != nil {
			return result{}, err
		}
		v, err := svc().GetResolution(ctx, ac, id)
		if err != nil {
			return result{}, err
		}
		if replay {
			return ok(v)
		}
		return created(v)
	})
	route("GET /v1/bodies/{id}/resolutions", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListResolutions(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/bodies/{id}/resolutions/export.csv", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		w, err := svc().ResolutionsCSV(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return result{status: http.StatusOK, ctype: "text/csv; charset=utf-8", raw: w}, nil
	})
	route("GET /v1/resolutions/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		v, err := svc().GetResolution(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(v)
	})
	route("POST /v1/resolutions/{id}/amend", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in AmendInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		v, err := svc().AmendResolution(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return ok(v)
	})
	simple := func(name string, fn func(*Service, ctxT, Actor, string) (*Resolution, error)) {
		route("POST /v1/resolutions/{id}/"+name, func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
			v, err := fn(svc(), ctx, ac, pv(r, "id"))
			if err != nil {
				return result{}, err
			}
			return ok(v)
		})
	}
	simple("propose", (*Service).ProposeResolution)
	simple("open-vote", (*Service).OpenVote)
	simple("close", (*Service).CloseVote)
	simple("withdraw", (*Service).WithdrawResolution)
	route("POST /v1/resolutions/{id}/vote", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in VoteInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		v, err := svc().CastVote(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return ok(v)
	})
	route("POST /v1/resolutions/{id}/conflicts", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in ConflictInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		c, err := svc().DeclareConflict(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(c)
	})
	route("GET /v1/resolutions/{id}/votes", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListVotes(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("POST /v1/resolution-requests", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in ResolutionRequest
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		o, err := svc().RequestResolution(ctx, ac, in)
		if err != nil {
			return result{}, err
		}
		return created(o)
	})
	route("GET /v1/resolution-outcomes", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		o, err := svc().OutcomeByRef(ctx, ac, q(r, "type"), q(r, "ref"))
		if err != nil {
			return result{}, err
		}
		return ok(o)
	})

	// ---- minutes and action items
	route("POST /v1/meetings/{id}/minutes", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Content string `json:"content"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		m, err := svc().DraftMinutes(ctx, ac, pv(r, "id"), in.Content)
		if err != nil {
			return result{}, err
		}
		return created(m)
	})
	route("GET /v1/meetings/{id}/minutes", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListMinutes(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/minutes/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		m, err := svc().GetMinutes(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(m)
	})
	route("PUT /v1/minutes/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Content string `json:"content"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		m, err := svc().EditMinutes(ctx, ac, pv(r, "id"), in.Content)
		if err != nil {
			return result{}, err
		}
		return ok(m)
	})
	route("POST /v1/minutes/{id}/approve", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		m, err := svc().ApproveMinutes(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(m)
	})
	route("POST /v1/meetings/{id}/actions", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in ActionInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		it, err := svc().CreateAction(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(it)
	})
	route("GET /v1/meetings/{id}/actions", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListActions(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/actions", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListActions(ctx, ac, "")
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/actions/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		it, err := svc().GetAction(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(it)
	})
	route("PATCH /v1/actions/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Status string `json:"status"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		it, err := svc().UpdateAction(ctx, ac, pv(r, "id"), in.Status)
		if err != nil {
			return result{}, err
		}
		return ok(it)
	})

	// ---- legal hold, erasure
	route("PUT /v1/people/{person}/legal-hold", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Hold   bool   `json:"hold"`
			Reason string `json:"reason"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		if err := svc().SetLegalHold(ctx, ac, pv(r, "person"), in.Hold, in.Reason); err != nil {
			return result{}, err
		}
		return noContent()
	})
	route("POST /v1/people/{person}/erasure", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		e, err := svc().RequestErasure(ctx, ac, pv(r, "person"))
		if err != nil {
			return result{}, err
		}
		return ok(e)
	})
	route("GET /v1/erasures", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListErasures(ctx, ac, qint(r, "after"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})

	// ---- entity module
	route("POST /v1/legal-entities", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in LegalEntityInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		e, err := svc().CreateLegalEntity(ctx, ac, in)
		if err != nil {
			return result{}, err
		}
		return created(e)
	})
	route("GET /v1/legal-entities", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListLegalEntities(ctx, ac)
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/legal-entities/{id}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		e, err := svc().GetLegalEntity(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(e)
	})
	route("POST /v1/legal-entities/{id}/status", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Status string `json:"status"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		e, err := svc().SetEntityStatus(ctx, ac, pv(r, "id"), in.Status)
		if err != nil {
			return result{}, err
		}
		return ok(e)
	})
	route("GET /v1/legal-entities/{id}/identifiers", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		i, err := svc().GetIdentifiers(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(i)
	})
	route("POST /v1/legal-entities/{id}/contacts", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in ContactInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		c, err := svc().AddContact(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(c)
	})
	route("GET /v1/legal-entities/{id}/contacts", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListContacts(ctx, ac, pv(r, "id"), q(r, "as_of"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("POST /v1/legal-entities/{id}/registers/{register}/entries", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in EntryInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		e, err := svc().RecordEntry(ctx, ac, pv(r, "id"), pv(r, "register"), in)
		if err != nil {
			return result{}, err
		}
		return created(e)
	})
	route("GET /v1/legal-entities/{id}/registers/{register}", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		v, err := svc().ViewRegister(ctx, ac, pv(r, "id"), pv(r, "register"), q(r, "as_of"), q(r, "known_at"), q(r, "history") == "true")
		if err != nil {
			return result{}, err
		}
		return ok(v)
	})
	route("GET /v1/legal-entities/{id}/registers/{register}/export.csv", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		w, err := svc().RegisterCSV(ctx, ac, pv(r, "id"), pv(r, "register"), q(r, "as_of"))
		if err != nil {
			return result{}, err
		}
		return result{status: http.StatusOK, ctype: "text/csv; charset=utf-8", raw: w}, nil
	})
	route("GET /v1/legal-entities/{id}/officers", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		v, err := svc().ViewRegister(ctx, ac, pv(r, "id"), RegDirectors, q(r, "as_of"), "", false)
		if err != nil {
			return result{}, err
		}
		return ok(v)
	})
	route("POST /v1/register-entries/{id}/attest", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		e, err := svc().AttestEntry(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return ok(e)
	})
	route("POST /v1/legal-entities/{id}/obligations", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in ObligationInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		o, err := svc().CreateObligation(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(o)
	})
	route("GET /v1/legal-entities/{id}/filings", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListFilings(ctx, ac, pv(r, "id"), q(r, "status"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("POST /v1/filings/{id}/result", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in FilingResult
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		f, err := svc().MarkFiling(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return ok(f)
	})
	route("POST /v1/legal-entities/{id}/documents", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in DocumentInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		d, err := svc().AddDocument(ctx, ac, pv(r, "id"), in)
		if err != nil {
			return result{}, err
		}
		return created(d)
	})
	route("GET /v1/legal-entities/{id}/documents", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListDocuments(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("POST /v1/legal-entities/{id}/share-classes", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		var in struct {
			ClassRef string `json:"class_ref"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		l, err := svc().LinkShareClass(ctx, ac, pv(r, "id"), in.ClassRef)
		if err != nil {
			return result{}, err
		}
		return created(l)
	})
	route("GET /v1/legal-entities/{id}/share-classes", func(ctx ctxT, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListShareClassLinks(ctx, ac, pv(r, "id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	return mux
}
