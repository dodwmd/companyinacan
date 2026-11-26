package work

import "net/http"

// REST handlers of project membership, the manager reports and saved filters
// (specs/065-work-manager). The routes are registered in api.go with the others.

func (a *api) listMembers(w http.ResponseWriter, r *http.Request, p *Principal) {
	ms, err := a.store.ListMembers(r.Context(), p, r.PathValue("key"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ms})
}

func (a *api) setMember(on bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) {
		if err := a.store.SetMember(r.Context(), p, r.PathValue("key"), r.PathValue("subject"), on); err != nil {
			a.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *api) workload(w http.ResponseWriter, r *http.Request, p *Principal) {
	out, err := a.store.Workload(r.Context(), p, r.URL.Query().Get("project"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) ageing(w http.ResponseWriter, r *http.Request, p *Principal) {
	out, err := a.store.Ageing(r.Context(), p, r.URL.Query().Get("project"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) staleWork(w http.ResponseWriter, r *http.Request, p *Principal) {
	out, err := a.store.StaleWork(r.Context(), p, r.URL.Query().Get("project"), queryInt(r, "days"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) listFilters(w http.ResponseWriter, r *http.Request, p *Principal) {
	fs, err := a.store.ListFilters(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": fs})
}

func (a *api) createFilter(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewFilter
	if !decodeJSON(w, r, &in) {
		return
	}
	f, err := a.store.CreateFilter(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (a *api) getFilter(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	f, err := a.store.GetFilter(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (a *api) deleteFilter(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := a.store.DeleteFilter(r.Context(), p, id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
