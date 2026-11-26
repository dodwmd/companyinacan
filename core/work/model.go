package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Errors the store returns; the API maps them to HTTP statuses.
var (
	ErrNotFound            = errors.New("not found")
	ErrConflict            = errors.New("conflict")
	ErrVersionMismatch     = errors.New("version mismatch")
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	ErrForbidden           = errors.New("forbidden")
)

// ProblemField names one invalid input.
type ProblemField struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// InvalidError is a validation failure carrying every problem found.
type InvalidError struct{ Problems []ProblemField }

func (e *InvalidError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Field + ": " + p.Message
	}
	return strings.Join(parts, "; ")
}

func invalid(field, format string, a ...any) *InvalidError {
	return &InvalidError{Problems: []ProblemField{{Field: field, Message: fmt.Sprintf(format, a...)}}}
}

// problems collects validation problems and turns them into one error.
type problems []ProblemField

func (p *problems) add(field, format string, a ...any) {
	*p = append(*p, ProblemField{Field: field, Message: fmt.Sprintf(format, a...)})
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return &InvalidError{Problems: p}
}

// Issue types. Hierarchy: epic > story/task/bug > subtask.
var IssueTypes = []string{"epic", "story", "task", "bug", "subtask"}

// Link types. "blocks" and "duplicates" are directed and acyclic; "relates" is symmetric.
var LinkTypes = []string{"blocks", "relates", "duplicates"}

// Field limits.
const (
	maxTitle       = 200
	maxDescription = 50000
	maxComment     = 20000
	maxLabels      = 20
	maxSubject     = 254
	maxBulk        = 100
)

var (
	projectKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	labelRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,39}$`)
	filenameRE   = regexp.MustCompile(`^[^/\\\x00-\x1f]{1,255}$`)
	sha256RE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Issue is the API view of an issue.
type Issue struct {
	Key            string     `json:"key"`
	Project        string     `json:"project"`
	Type           string     `json:"type"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	Priority       string     `json:"priority"`
	SLAHint        SLATarget  `json:"sla_hint"`
	Status         string     `json:"status"`
	StatusCategory string     `json:"status_category"`
	Reporter       string     `json:"reporter"`
	Assignee       *string    `json:"assignee"`
	DueDate        *string    `json:"due_date"`
	Estimate       *float64   `json:"estimate"`
	Parent         *string    `json:"parent"`
	SprintID       *int64     `json:"sprint_id"`
	Labels         []string   `json:"labels"`
	Rank           string     `json:"rank"`
	Version        int64      `json:"version"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
}

// IssueDetail adds the relations shown on the issue page.
type IssueDetail struct {
	Issue
	Children    []string     `json:"children"`
	Links       []Link       `json:"links"`
	Watchers    []string     `json:"watchers"`
	Attachments []Attachment `json:"attachments"`
}

// Link is one relation seen from an issue. Direction is "outbound" when this issue is the
// source ("blocks" means this issue blocks the other) and "inbound" otherwise.
type Link struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Direction string `json:"direction"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	Status    string `json:"status"`
}

// Comment is a comment on an issue.
type Comment struct {
	ID        int64     `json:"id"`
	Issue     string    `json:"issue"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Activity is one entry of an issue's history.
type Activity struct {
	ID    int64           `json:"id"`
	Actor string          `json:"actor"`
	Kind  string          `json:"kind"`
	Data  json.RawMessage `json:"data"`
	At    time.Time       `json:"at"`
}

// Attachment is attachment metadata. The tracker stores no bytes yet; URL, when set, points
// to wherever the file lives (for example the files module later).
type Attachment struct {
	ID          int64     `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type,omitempty"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      string    `json:"sha256,omitempty"`
	URL         string    `json:"url,omitempty"`
	AddedBy     string    `json:"added_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// Project is a space that owns issues, a workflow and sprints.
type Project struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Workflow    Workflow  `json:"workflow"`
	SLA         SLA       `json:"sla"`
	Archived    bool      `json:"archived"`
	IssueCount  int64     `json:"issue_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Sprint is a time-boxed set of issues.
type Sprint struct {
	ID        int64     `json:"id"`
	Project   string    `json:"project"`
	Name      string    `json:"name"`
	Goal      string    `json:"goal"`
	State     string    `json:"state"`
	StartsOn  *string   `json:"starts_on"`
	EndsOn    *string   `json:"ends_on"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Opt is an optional JSON field that tells "absent" from "null" from "a value", which a
// PATCH needs: absent leaves the field alone, null clears it.
type Opt[T any] struct {
	Set  bool
	Null bool
	V    T
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.V)
}

// CreateIssue is the body of an issue create. Status, reporter, key, version and
// timestamps are not inputs: the server owns them.
type CreateIssue struct {
	Type           string   `json:"type"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Priority       string   `json:"priority"`
	Assignee       *string  `json:"assignee"`
	DueDate        *string  `json:"due_date"`
	Estimate       *float64 `json:"estimate"`
	Parent         *string  `json:"parent"`
	SprintID       *int64   `json:"sprint_id"`
	Labels         []string `json:"labels"`
	IdempotencyKey string   `json:"idempotency_key"`

	// imported marks a row of a CSV import: history that asks nobody's attention now.
	imported bool
}

// Patch is the body of an issue update (and the `set` of a bulk update). Every field is
// optional; unknown fields are rejected by the decoder.
type Patch struct {
	Title        Opt[string]   `json:"title"`
	Description  Opt[string]   `json:"description"`
	Priority     Opt[string]   `json:"priority"`
	Assignee     Opt[string]   `json:"assignee"`
	DueDate      Opt[string]   `json:"due_date"`
	Estimate     Opt[float64]  `json:"estimate"`
	Parent       Opt[string]   `json:"parent"`
	SprintID     Opt[int64]    `json:"sprint_id"`
	Labels       Opt[[]string] `json:"labels"`
	LabelsAdd    []string      `json:"labels_add"`
	LabelsRemove []string      `json:"labels_remove"`
	// Version is the optimistic-concurrency guard (the same number as the ETag).
	Version Opt[int64] `json:"version"`
}

func validSubject(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= maxSubject && !strings.ContainsAny(s, "\x00\r\n\t") && strings.TrimSpace(s) == s
}

// hasControl reports whether s contains a control character (newlines, bells, escapes...). A title
// is one line of plain text: it is echoed into logs, webhook receivers and terminals.
func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

func validText(s string, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, 0)
}

func validDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Format(time.DateOnly) == s
}

func validLabels(field string, ls []string, p *problems) {
	if len(ls) > maxLabels {
		p.add(field, "at most %d labels", maxLabels)
		return
	}
	for _, l := range ls {
		if !labelRE.MatchString(l) {
			p.add(field, "label %q must match %s", l, labelRE)
		}
	}
}

func dedupe(ls []string) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

func (c *CreateIssue) validate() error {
	var p problems
	if !slices.Contains(IssueTypes, c.Type) {
		p.add("type", "must be one of %s", strings.Join(IssueTypes, ", "))
	}
	if strings.TrimSpace(c.Title) == "" || !validText(c.Title, maxTitle) || hasControl(c.Title) {
		p.add("title", "required, one line, at most %d characters", maxTitle)
	}
	if !validText(c.Description, maxDescription) {
		p.add("description", "at most %d characters", maxDescription)
	}
	if c.Priority == "" {
		c.Priority = "P2"
	}
	if !ValidPriority(c.Priority) {
		p.add("priority", "must be P0, P1, P2 or P3")
	}
	if c.Assignee != nil && !validSubject(*c.Assignee) {
		p.add("assignee", "must be a non-empty identifier of at most %d characters", maxSubject)
	}
	if c.DueDate != nil && !validDate(*c.DueDate) {
		p.add("due_date", "must be YYYY-MM-DD")
	}
	if c.Estimate != nil && (*c.Estimate < 0 || *c.Estimate > 10000) {
		p.add("estimate", "must be between 0 and 10000")
	}
	if c.Parent != nil && !issueKeyRE.MatchString(*c.Parent) {
		p.add("parent", "must be an issue key like ACME-12")
	}
	validLabels("labels", c.Labels, &p)
	if len(c.IdempotencyKey) > 128 || (c.IdempotencyKey != "" && len(c.IdempotencyKey) < 8) {
		p.add("idempotency_key", "must be 8 to 128 characters")
	}
	return p.err()
}

func (pt *Patch) validate() error {
	var p problems
	if pt.Title.Set && (pt.Title.Null || strings.TrimSpace(pt.Title.V) == "" || !validText(pt.Title.V, maxTitle) || hasControl(pt.Title.V)) {
		p.add("title", "required, one line, at most %d characters", maxTitle)
	}
	if pt.Description.Set && (pt.Description.Null || !validText(pt.Description.V, maxDescription)) {
		p.add("description", "at most %d characters (use \"\" to clear)", maxDescription)
	}
	if pt.Priority.Set && (pt.Priority.Null || !ValidPriority(pt.Priority.V)) {
		p.add("priority", "must be P0, P1, P2 or P3")
	}
	if pt.Assignee.Set && !pt.Assignee.Null && !validSubject(pt.Assignee.V) {
		p.add("assignee", "must be a non-empty identifier of at most %d characters or null", maxSubject)
	}
	if pt.DueDate.Set && !pt.DueDate.Null && !validDate(pt.DueDate.V) {
		p.add("due_date", "must be YYYY-MM-DD or null")
	}
	if pt.Estimate.Set && !pt.Estimate.Null && (pt.Estimate.V < 0 || pt.Estimate.V > 10000) {
		p.add("estimate", "must be between 0 and 10000 or null")
	}
	if pt.Parent.Set && !pt.Parent.Null && !issueKeyRE.MatchString(pt.Parent.V) {
		p.add("parent", "must be an issue key like ACME-12 or null")
	}
	if pt.Labels.Set {
		if pt.Labels.Null {
			p.add("labels", "use [] to clear labels")
		}
		validLabels("labels", pt.Labels.V, &p)
	}
	validLabels("labels_add", pt.LabelsAdd, &p)
	validLabels("labels_remove", pt.LabelsRemove, &p)
	if pt.Labels.Set && (len(pt.LabelsAdd) > 0 || len(pt.LabelsRemove) > 0) {
		p.add("labels", "cannot be combined with labels_add or labels_remove")
	}
	if pt.SprintID.Set && !pt.SprintID.Null && pt.SprintID.V < 1 {
		p.add("sprint_id", "must be a sprint id or null")
	}
	return p.err()
}

func (pt *Patch) empty() bool {
	return !pt.Title.Set && !pt.Description.Set && !pt.Priority.Set && !pt.Assignee.Set && !pt.DueDate.Set &&
		!pt.Estimate.Set && !pt.Parent.Set && !pt.SprintID.Set && !pt.Labels.Set &&
		len(pt.LabelsAdd) == 0 && len(pt.LabelsRemove) == 0
}
