package csvimport

import (
	"errors"
	"net/http"
)

// What a row does.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionUnchanged = "unchanged"
	ActionError     = "error"
)

// ErrRollback is returned from inside a module's write transaction to discard it: a preview,
// or an apply with at least one row error (all or nothing).
var ErrRollback = errors.New("import rolled back")

// ErrBusy means another import is running in this module.
var ErrBusy = errors.New("another import is running; try again when it has finished")

// Message is an error or warning about one line.
type Message struct {
	Line    int    `json:"line"`
	Column  string `json:"column,omitempty"`
	Message string `json:"message"`
}

// Item is the outcome of one row. Changed holds field names, never values. ID is the record
// the row matched, or the new one once it has been written.
type Item struct {
	Line       int      `json:"line"`
	ExternalID string   `json:"external_id,omitempty"`
	Action     string   `json:"action"`
	ID         string   `json:"id,omitempty"`
	Changed    []string `json:"changed,omitempty"`

	report *Report
}

// Report is the answer to a preview or an apply.
type Report struct {
	Applied    bool              `json:"applied"`
	DryRun     bool              `json:"dry_run"`
	Rows       int               `json:"rows"`
	Create     int               `json:"create"`
	Update     int               `json:"update"`
	Unchanged  int               `json:"unchanged"`
	Failed     int               `json:"failed"`
	ErrorCount int               `json:"error_count"`
	Errors     []Message         `json:"errors"`
	Warnings   []Message         `json:"warnings"`
	Columns    map[string]string `json:"columns"`
	Ignored    []string          `json:"ignored_columns"`
	Items      []*Item           `json:"items"`
}

// NewReport starts a report for a file.
func NewReport(f *File, o Options) *Report {
	ignored := f.Ignored
	if ignored == nil {
		ignored = []string{}
	}
	return &Report{DryRun: !o.Apply, Rows: len(f.Rows), Errors: []Message{}, Warnings: []Message{}, Columns: f.Columns, Ignored: ignored, Items: make([]*Item, 0, len(f.Rows))}
}

// Row starts the item for a row, recording the row's own error if it has one. Call it once
// per row, in file order.
func (r *Report) Row(row Row) *Item {
	it := &Item{Line: row.Line, report: r}
	if id := row.Get(ExternalID); ValidExternalID(id) {
		it.ExternalID = id
	}
	r.Items = append(r.Items, it)
	if row.Err != "" {
		it.Error(row.errColumn, row.Err)
	}
	return it
}

// Error marks the row failed. An import with any failed row writes nothing.
func (it *Item) Error(column, msg string) {
	r := it.report
	r.ErrorCount++
	if len(r.Errors) < MaxListed {
		r.Errors = append(r.Errors, Message{it.Line, column, msg})
	}
	it.Action = ActionError
}

// Warn notes something that does not stop the import.
func (it *Item) Warn(column, msg string) {
	if len(it.report.Warnings) < MaxListed {
		it.report.Warnings = append(it.report.Warnings, Message{it.Line, column, msg})
	}
}

// Failed reports whether the row has an error.
func (it *Item) Failed() bool { return it.Action == ActionError }

// Create records that the row makes a new record; id is "" in a preview.
func (it *Item) Create(id string) {
	if !it.Failed() {
		it.Action, it.ID = ActionCreate, id
	}
}

// Update records the changed field names of an existing record; none means unchanged.
func (it *Item) Update(id string, changed []string) {
	if it.Failed() {
		return
	}
	it.ID, it.Changed = id, changed
	it.Action = ActionUnchanged
	if len(changed) > 0 {
		it.Action = ActionUpdate
	}
}

// Status is the HTTP status of a report: 422 when an apply wrote nothing because of row errors.
func (r *Report) Status() int {
	if !r.DryRun && r.HasErrors() {
		return http.StatusUnprocessableEntity
	}
	return http.StatusOK
}

// Failure maps the errors that concern a request as a whole to an HTTP status and error
// code, so every module answers alike. The message is err.Error().
func Failure(err error) (status int, code string, ok bool) {
	var fe *FileError
	switch {
	case errors.As(err, &fe) && fe.TooLarge:
		return http.StatusRequestEntityTooLarge, "too_large", true
	case errors.As(err, &fe):
		return http.StatusBadRequest, "invalid", true
	case errors.Is(err, ErrBusy):
		return http.StatusConflict, "busy", true
	}
	return 0, "", false
}

// HasErrors reports whether anything failed.
func (r *Report) HasErrors() bool { return r.ErrorCount > 0 }

// Finish counts the rows. Call it after the transaction ended without an unexpected error.
// A preview and a failed apply are not applied, and name no record that does not exist.
func (r *Report) Finish() {
	r.Applied = !r.DryRun && !r.HasErrors()
	r.Create, r.Update, r.Unchanged, r.Failed = 0, 0, 0, 0
	for _, it := range r.Items {
		switch it.Action {
		case ActionCreate:
			r.Create++
			if !r.Applied {
				it.ID = ""
			}
		case ActionUpdate:
			r.Update++
		case ActionError:
			r.Failed++
		default:
			r.Unchanged++
		}
	}
}
