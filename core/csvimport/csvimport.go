// Package csvimport is the part of CSV import every module shares (specs/047-csv-import): a
// bounded single-pass read, header mapping, the external id rules and the report. It knows
// nothing about databases; the modules own the records and the authorization.
//
// No message made here echoes a cell value: rows hold personal data, and a report is often
// pasted into a ticket.
package csvimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits (FR-001).
const (
	MaxBytes   = 1 << 20
	MaxRows    = 2000
	MaxColumns = 64
	// maxSeparators bounds the cells of a file (commas inside quoted text count too).
	maxSeparators = MaxRows * MaxColumns
	// MaxListed caps the errors and the warnings a report lists; the counts stay complete.
	MaxListed = 200
)

// ExternalID is the column every import requires: the stable id of the record in the source.
const ExternalID = "external_id"

// externalIDRe has no "@" (an id is not an email address) and requires a letter or digit
// first (an id never starts a spreadsheet formula).
var externalIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,63}$`)

// ValidExternalID reports whether s can be used as an external id.
func ValidExternalID(s string) bool { return externalIDRe.MatchString(s) }

// Field is one value an importer reads.
type Field struct {
	Name     string
	Required bool // the column must be present; whether a cell may be empty is the module's rule
}

// FileError means the file as a whole cannot be imported.
type FileError struct {
	Msg      string
	TooLarge bool
}

func (e *FileError) Error() string { return e.Msg }

func fileErr(format string, a ...any) error { return &FileError{Msg: fmt.Sprintf(format, a...)} }

// Row is one data row. Err is set when the row cannot be used (its cell count or external id).
type Row struct {
	Line int
	Err  string
	// errColumn names the column Err is about, when it is about one.
	errColumn string
	cells     []string
	index     map[string]int
}

// Get returns the field's cell with surrounding space removed, "" when the file has no such column.
func (r Row) Get(field string) string {
	i, ok := r.index[field]
	if !ok || i >= len(r.cells) {
		return ""
	}
	return strings.TrimSpace(r.cells[i])
}

// File is a parsed CSV.
type File struct {
	Rows    []Row
	Columns map[string]string // field -> header it is read from
	Ignored []string          // headers no field uses
	SHA256  string
}

// Has reports whether the file has a column for the field.
func (f *File) Has(field string) bool { _, ok := f.Columns[field]; return ok }

func normalise(h string) string {
	return strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(strings.TrimSpace(h)))
}

// Read reads at most MaxBytes and parses them in one pass. mapping is field -> header.
func Read(r io.Reader, fields []Field, mapping map[string]string) (*File, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		var tooBig *http.MaxBytesError // a server's own smaller body limit
		if errors.As(err, &tooBig) {
			return nil, &FileError{Msg: tooLargeMsg, TooLarge: true}
		}
		return nil, fileErr("the file could not be read")
	}
	if len(data) > MaxBytes {
		return nil, &FileError{Msg: tooLargeMsg, TooLarge: true}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fileErr("the file must be UTF-8 text without NUL bytes (save it as \"CSV UTF-8\")")
	}
	// A file of nothing but separators would cost many times its size in memory to split.
	if bytes.Count(data, []byte(",")) > maxSeparators {
		return nil, &FileError{Msg: "the file has too many cells; split it", TooLarge: true}
	}
	sum := sha256.Sum256(data)
	cr := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\ufeff"))))
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, fileErr("the file is empty")
	}
	if err != nil {
		return nil, parseErr(err)
	}
	if len(header) > MaxColumns {
		return nil, fileErr("at most %d columns", MaxColumns)
	}
	f := &File{Columns: map[string]string{}, SHA256: hex.EncodeToString(sum[:])}
	index, err := mapHeader(f, header, fields, mapping)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, parseErr(err)
		}
		line, _ := cr.FieldPos(0)
		if !slices.ContainsFunc(rec, func(c string) bool { return strings.TrimSpace(c) != "" }) {
			continue // a blank ",,," row, as spreadsheets leave below the data
		}
		if len(f.Rows) == MaxRows {
			return nil, &FileError{Msg: fmt.Sprintf("at most %d rows; split the file", MaxRows), TooLarge: true}
		}
		row := Row{Line: line, cells: rec, index: index}
		id := row.Get(ExternalID)
		switch {
		case len(rec) != len(header):
			row.Err = fmt.Sprintf("the row has %d cells, the header has %d", len(rec), len(header))
		case !ValidExternalID(id):
			row.Err, row.errColumn = "required: 1 to 64 letters, digits or ._:/#-, starting with a letter or digit", ExternalID
		case seen[id] != 0:
			row.Err, row.errColumn = fmt.Sprintf("the same id is on line %d", seen[id]), ExternalID
		default:
			seen[id] = line
		}
		f.Rows = append(f.Rows, row)
	}
	if len(f.Rows) == 0 {
		return nil, fileErr("the file has a header and no rows")
	}
	return f, nil
}

var tooLargeMsg = fmt.Sprintf("the file is larger than %d bytes; split it", MaxBytes)

// parseErr keeps the position and drops everything else: encoding/csv messages carry no cell text.
func parseErr(err error) error {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return fileErr("line %d, column %d: %v", pe.Line, pe.Column, pe.Err)
	}
	return fileErr("the file is not valid CSV")
}

func mapHeader(f *File, header []string, fields []Field, mapping map[string]string) (map[string]int, error) {
	byName, byNorm := map[string]int{}, map[string]int{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		header[i] = h
		n := normalise(h)
		if n == "" {
			continue // an unnamed column cannot be read
		}
		if _, dup := byNorm[n]; dup {
			return nil, fileErr("two columns are called %q", h)
		}
		byName[h], byNorm[n] = i, i
	}
	for name := range mapping {
		if !slices.ContainsFunc(fields, func(x Field) bool { return x.Name == name }) {
			names := make([]string, len(fields))
			for i, x := range fields {
				names[i] = x.Name
			}
			return nil, fileErr("map: unknown field %q (fields: %s)", name, strings.Join(names, ", "))
		}
	}
	index, used := map[string]int{}, map[int]string{}
	// Mapped fields first, so a mapping can take a column another field would match by name.
	for _, mapped := range []bool{true, false} {
		for _, field := range fields {
			want, isMapped := mapping[field.Name]
			if isMapped != mapped {
				continue
			}
			var i int
			var ok bool
			if isMapped {
				if i, ok = byName[want]; !ok {
					i, ok = byNorm[normalise(want)]
				}
				if !ok {
					return nil, fileErr("map: the file has no column %q", want)
				}
				if other, taken := used[i]; taken {
					return nil, fileErr("map: %s and %s read the same column", other, field.Name)
				}
			} else {
				if i, ok = byNorm[field.Name]; !ok {
					continue
				}
				if _, taken := used[i]; taken {
					continue
				}
			}
			index[field.Name], used[i], f.Columns[field.Name] = i, field.Name, header[i]
		}
	}
	for _, field := range fields {
		if _, ok := index[field.Name]; !ok && field.Required {
			if len(header) == 1 && strings.ContainsAny(header[0], ";\t|") {
				return nil, fileErr("the file looks semicolon, tab or pipe separated; save it as comma-separated CSV")
			}
			return nil, fileErr("no column for %q (name a column %s, or pass map=%s=<header>)", field.Name, field.Name, field.Name)
		}
	}
	for i, h := range header {
		if _, ok := used[i]; !ok && h != "" {
			f.Ignored = append(f.Ignored, h)
		}
	}
	return index, nil
}

// Options is what a caller chooses.
type Options struct {
	Apply   bool            // false is a preview: nothing is written
	Flags   map[string]bool // the switches of one importer (people: joiners)
	Mapping map[string]string
}

// ParseQuery reads apply, map and the named boolean flags of one importer. Anything else is refused: a misspelt "apply" must not
// look like a successful import.
func ParseQuery(q url.Values, flags ...string) (Options, error) {
	o := Options{Mapping: map[string]string{}, Flags: map[string]bool{}}
	for k, v := range q {
		switch k {
		case "apply":
			if len(v) != 1 || (v[0] != "true" && v[0] != "false") {
				return o, fileErr("apply must be true or false")
			}
			o.Apply = v[0] == "true"
		case "map":
			for _, m := range v {
				field, header, ok := strings.Cut(m, "=")
				if !ok || field == "" || header == "" {
					return o, fileErr("map must be <field>=<column header>")
				}
				if _, dup := o.Mapping[field]; dup {
					return o, fileErr("map: %q is given twice", field)
				}
				o.Mapping[field] = header
			}
		default:
			if !slices.Contains(flags, k) || len(v) != 1 || (v[0] != "true" && v[0] != "false") {
				return o, fileErr("unknown or invalid query parameter %q (%s)", k, strings.Join(append([]string{"apply", "map"}, flags...), ", "))
			}
			o.Flags[k] = v[0] == "true"
		}
	}
	return o, nil
}

// Query is the inverse of ParseQuery, for clients.
func (o Options) Query() string {
	q := url.Values{}
	if o.Apply {
		q.Set("apply", "true")
	}
	for k, on := range o.Flags {
		if on {
			q.Set(k, "true")
		}
	}
	keys := make([]string, 0, len(o.Mapping))
	for k := range o.Mapping {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		q.Add("map", k+"="+o.Mapping[k])
	}
	return q.Encode()
}
