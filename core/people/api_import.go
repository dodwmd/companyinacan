package people

import (
	"context"
	"errors"
	"net/http"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

// importPeople is POST /v1/import/people: the body is the CSV (specs/047-csv-import).
func (a *API) importPeople(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
	o, err := csvimport.ParseQuery(r.URL.Query())
	if err != nil {
		return 0, nil, err
	}
	rep, err := a.Svc.ImportPeople(ctx, ac, r.Body, o) // Read caps the body
	if err != nil {
		return 0, nil, err
	}
	if o.Apply && rep.HasErrors() {
		return http.StatusUnprocessableEntity, rep, nil
	}
	return http.StatusOK, rep, nil
}

// importFailure maps the errors of a file as a whole.
func importFailure(w http.ResponseWriter, err error) bool {
	var fe *csvimport.FileError
	switch {
	case errors.As(err, &fe) && fe.TooLarge:
		writeJSON(w, http.StatusRequestEntityTooLarge, apiError{"too_large", fe.Msg})
	case errors.As(err, &fe):
		writeJSON(w, http.StatusBadRequest, apiError{"invalid", fe.Msg})
	case errors.Is(err, csvimport.ErrBusy):
		writeJSON(w, http.StatusConflict, apiError{"busy", err.Error()})
	default:
		return false
	}
	return true
}
