package people

import (
	"context"
	"net/http"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

// importPeople is POST /v1/import/people: the body is the CSV (specs/047-csv-import).
func (a *API) importPeople(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
	var rep *csvimport.Report
	o, err := csvimport.ParseQuery(r.URL.Query(), importJoiners)
	if err == nil {
		rep, err = a.Svc.ImportPeople(ctx, ac, r.Body, o) // Read caps the body
	}
	if status, code, ok := csvimport.Failure(err); ok {
		return status, apiError{code, err.Error()}, nil
	}
	if err != nil {
		return 0, nil, err
	}
	return rep.Status(), rep, nil
}
