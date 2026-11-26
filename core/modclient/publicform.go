package modclient

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Bounds of a form a module UI accepts without a session (sign-in, launch ticket): a token or a
// ticket, the CSRF value and a couple of fields. Anyone can post these, hundreds side by side, into
// a pod limited to 64 MiB, and parsing allocates for every field: the number of fields is bounded
// as much as the bytes.
const (
	MaxPublicFormBytes  = 4 << 10
	maxPublicFormFields = 16
	maxPublicQueryBytes = 1 << 10
)

// ErrPublicForm is what ParseForm returns for a form over those bounds.
var ErrPublicForm = errors.New("the form is too large")

type failedBody struct{ err error }

func (f failedBody) Read([]byte) (int, error) { return 0, f.err }
func (failedBody) Close() error               { return nil }

// LimitPublicForm must run before anything parses r: afterwards a form within the bounds reads as
// before, and one over them fails to parse, with neither its body nor its query turned into values.
func LimitPublicForm(w http.ResponseWriter, r *http.Request) {
	var b []byte
	var err error
	if r.Body != nil {
		b, err = io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPublicFormBytes))
	}
	q := r.URL.RawQuery
	if err == nil && (len(q) > maxPublicQueryBytes || strings.Count(q, "&")+bytes.Count(b, []byte("&")) >= maxPublicFormFields) {
		err = ErrPublicForm
	}
	if err != nil {
		r.URL.RawQuery = ""
		r.Body = failedBody{ErrPublicForm}
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
}
