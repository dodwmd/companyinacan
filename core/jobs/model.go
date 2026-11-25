package jobs

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Job lifecycle. A job is a requisition and its public posting in one record: draft, open,
// paused, closed. Only open jobs appear on the careers page and in the feeds.
const (
	StatusDraft  = "draft"
	StatusOpen   = "open"
	StatusPaused = "paused"
	StatusClosed = "closed"
)

// Approval states of a requisition (see Approver).
const (
	ApprovalNone     = "none"
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
)

// Application statuses. An active application sits in a pipeline stage; the other three
// are terminal (a rejected or withdrawn application keeps the stage it ended in).
const (
	AppActive    = "active"
	AppRejected  = "rejected"
	AppWithdrawn = "withdrawn"
	AppHired     = "hired"
)

// Remote working arrangements.
var RemoteKinds = []string{"onsite", "hybrid", "remote"}

// EmploymentTypes maps the API value to the schema.org employmentType.
var EmploymentTypes = map[string]string{
	"full_time": "FULL_TIME", "part_time": "PART_TIME", "contract": "CONTRACTOR",
	"temporary": "TEMPORARY", "intern": "INTERN", "volunteer": "VOLUNTEER", "other": "OTHER",
}

// SalaryPeriods maps the API value to the schema.org QuantitativeValue unitText.
var SalaryPeriods = map[string]string{"hour": "HOUR", "day": "DAY", "week": "WEEK", "month": "MONTH", "year": "YEAR"}

// Location is where the work happens. Country is an ISO 3166-1 alpha-2 code.
type Location struct {
	City       string `json:"city,omitempty"`
	Region     string `json:"region,omitempty"`
	Country    string `json:"country,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
}

// Salary is a pay range. It is always visible to staff; Job.PublishSalary decides whether the
// careers page and feeds show it.
type Salary struct {
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Currency string  `json:"currency"`
	Period   string  `json:"period"`
}

// Section is one headed block of the job description. Bodies are plain text (blank lines
// separate paragraphs); the renderers escape them, so no markup can be injected.
type Section struct {
	Heading string `json:"heading"`
	Body    string `json:"body"`
}

// Job is a requisition plus its posting.
type Job struct {
	ID                 string     `json:"id"`
	Slug               string     `json:"slug"`
	Title              string     `json:"title"`
	Department         string     `json:"department"`
	Location           Location   `json:"location"`
	Remote             string     `json:"remote"`
	ApplicantCountries []string   `json:"applicant_countries"`
	EmploymentType     string     `json:"employment_type"`
	Salary             *Salary    `json:"salary"`
	PublishSalary      bool       `json:"publish_salary"`
	Sections           []Section  `json:"sections"`
	HiringManager      string     `json:"hiring_manager"`
	Headcount          int        `json:"headcount"`
	Status             string     `json:"status"`
	Approval           string     `json:"approval"`
	ValidThrough       *time.Time `json:"valid_through"`
	OpenedAt           *time.Time `json:"opened_at"`
	ClosedAt           *time.Time `json:"closed_at"`
	CloseReason        string     `json:"close_reason"`
	CreatedBy          string     `json:"created_by"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	Version            int64      `json:"version"`
}

// NewJob is the body of a job create.
type NewJob struct {
	Title              string    `json:"title"`
	Department         string    `json:"department"`
	Location           Location  `json:"location"`
	Remote             string    `json:"remote"`
	ApplicantCountries []string  `json:"applicant_countries"`
	EmploymentType     string    `json:"employment_type"`
	Salary             *Salary   `json:"salary"`
	PublishSalary      bool      `json:"publish_salary"`
	Sections           []Section `json:"sections"`
	HiringManager      string    `json:"hiring_manager"`
	Headcount          int       `json:"headcount"`
	ValidThrough       *string   `json:"valid_through"` // YYYY-MM-DD
}

// JobPatch changes a draft, paused or open job. Absent fields stay as they are.
type JobPatch struct {
	Title              *string    `json:"title"`
	Department         *string    `json:"department"`
	Location           *Location  `json:"location"`
	Remote             *string    `json:"remote"`
	ApplicantCountries *[]string  `json:"applicant_countries"`
	EmploymentType     *string    `json:"employment_type"`
	Salary             *Salary    `json:"salary"`
	ClearSalary        bool       `json:"clear_salary"`
	PublishSalary      *bool      `json:"publish_salary"`
	Sections           *[]Section `json:"sections"`
	HiringManager      *string    `json:"hiring_manager"`
	Headcount          *int       `json:"headcount"`
	ValidThrough       *string    `json:"valid_through"`
}

var (
	idRE      = regexp.MustCompile(`^(job|cand|app|sc)_[0-9a-f]{16}$`)
	countryRE = regexp.MustCompile(`^[A-Z]{2}$`)
	currRE    = regexp.MustCompile(`^[A-Z]{3}$`)
	subjectRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+:-]{0,127}$`)
	slugRE    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

func validSubject(s string) bool { return subjectRE.MatchString(s) }

// validate checks a complete job. It is used for create, and for patches after they are applied.
func (j *Job) validate() error {
	var p problems
	p.text("title", j.Title, 120, true, false)
	p.text("department", j.Department, 80, false, false)
	p.text("location.city", j.Location.City, 80, false, false)
	p.text("location.region", j.Location.Region, 80, false, false)
	p.text("location.postal_code", j.Location.PostalCode, 20, false, false)
	if j.Location.Country != "" && !countryRE.MatchString(j.Location.Country) {
		p.add("location.country", "must be an ISO 3166-1 alpha-2 code such as US")
	}
	if !slices.Contains(RemoteKinds, j.Remote) {
		p.add("remote", "must be one of %s", strings.Join(RemoteKinds, ", "))
	}
	if len(j.ApplicantCountries) > 50 {
		p.add("applicant_countries", "at most 50 countries")
	}
	for _, c := range j.ApplicantCountries {
		if !countryRE.MatchString(c) {
			p.add("applicant_countries", "%q is not an ISO 3166-1 alpha-2 code", c)
		}
	}
	if j.Remote == "onsite" && j.Location.City == "" && j.Location.Country == "" {
		p.add("location", "an on-site job needs a city or a country")
	}
	if j.Remote == "remote" && len(j.ApplicantCountries) == 0 && j.Location.Country == "" {
		p.add("applicant_countries", "a remote job needs the countries applicants may live in (or a location country)")
	}
	if _, ok := EmploymentTypes[j.EmploymentType]; !ok {
		p.add("employment_type", "must be one of full_time, part_time, contract, temporary, intern, volunteer, other")
	}
	if s := j.Salary; s != nil {
		if s.Min < 0 || s.Max < s.Min || s.Max > 1e9 {
			p.add("salary", "min and max must satisfy 0 <= min <= max <= 1000000000")
		}
		if !currRE.MatchString(s.Currency) {
			p.add("salary.currency", "must be an ISO 4217 code such as USD")
		}
		if _, ok := SalaryPeriods[s.Period]; !ok {
			p.add("salary.period", "must be one of hour, day, week, month, year")
		}
	} else if j.PublishSalary {
		p.add("publish_salary", "cannot publish a salary that is not set")
	}
	if len(j.Sections) == 0 || len(j.Sections) > 20 {
		p.add("sections", "between 1 and 20 sections are required")
	}
	total := 0
	for i, s := range j.Sections {
		p.text("sections.heading", s.Heading, 100, true, false)
		p.text("sections.body", s.Body, 10000, true, true)
		total += len(s.Body)
		_ = i
	}
	if total > 50000 {
		p.add("sections", "the description may be at most 50000 characters in total")
	}
	if j.HiringManager != "" && !validSubject(j.HiringManager) {
		p.add("hiring_manager", "must be the subject (login) of the hiring manager")
	}
	if j.Headcount < 1 || j.Headcount > 1000 {
		p.add("headcount", "between 1 and 1000")
	}
	return p.err()
}

// Stage kinds. The first stage is initial, the last is hired; everything between is active.
const (
	KindInitial = "initial"
	KindActive  = "active"
	KindOffer   = "offer"
	KindHired   = "hired"
)

// Stage is one step of the pipeline.
type Stage struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Position int    `json:"position"`
}

// DefaultStages is the pipeline a new deployment starts with.
func DefaultStages() []Stage {
	return []Stage{
		{Key: "applied", Name: "Applied", Kind: KindInitial},
		{Key: "screen", Name: "Screen", Kind: KindActive},
		{Key: "interview", Name: "Interview", Kind: KindActive},
		{Key: "offer", Name: "Offer", Kind: KindOffer},
		{Key: "hired", Name: "Hired", Kind: KindHired},
	}
}

var stageKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

// validateStages checks a configured pipeline: 3 to 12 stages, first initial, last hired,
// unique keys, nothing in between initial or hired.
func validateStages(ss []Stage) error {
	var p problems
	if len(ss) < 3 || len(ss) > 12 {
		p.add("stages", "between 3 and 12 stages")
		return p.err()
	}
	seen := map[string]bool{}
	for i, s := range ss {
		if !stageKeyRE.MatchString(s.Key) {
			p.add("stages.key", "%q must match %s", s.Key, stageKeyRE)
		}
		if seen[s.Key] {
			p.add("stages.key", "duplicate key %q", s.Key)
		}
		seen[s.Key] = true
		p.text("stages.name", s.Name, 40, true, false)
		want := KindActive
		switch {
		case i == 0:
			want = KindInitial
		case i == len(ss)-1:
			want = KindHired
		case s.Kind == KindOffer:
			want = KindOffer
		}
		if s.Kind != want {
			p.add("stages.kind", "stage %d (%s) must be %q", i+1, s.Key, want)
		}
	}
	return p.err()
}

// Rejection reasons are a fixed vocabulary so reports stay comparable; the free-text note
// is sealed.
var RejectReasons = []string{"not_a_fit", "unqualified", "position_filled", "no_response", "compensation", "withdrew", "other"}

// Recommendations on a scorecard, weakest to strongest.
var Recommendations = []string{"strong_no", "no", "yes", "strong_yes"}
