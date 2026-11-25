package jobs

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

// SiteInfo is what the public renderers and feeds need to know about the company and where
// the careers pages live.
type SiteInfo struct {
	Company     string
	BaseURL     string // https://example.com, no trailing slash
	CareersPath string // /careers, no trailing slash
	LogoURL     string
}

// JobURL is the public address of one posting.
func (s SiteInfo) JobURL(j *Job) string { return s.BaseURL + s.CareersPath + "/" + j.Slug }

// CareersURL is the public address of the list page.
func (s SiteInfo) CareersURL() string { return s.BaseURL + s.CareersPath }

func (j *Job) listed() bool { return j.Status == StatusOpen }

// DescriptionHTML renders the sections as a small, fixed subset of HTML (h3 and p). Every
// character of user text is escaped, so the result is safe to embed in JSON-LD, feeds and
// pages.
func DescriptionHTML(secs []Section) string {
	var b strings.Builder
	for _, s := range secs {
		b.WriteString("<h3>" + html.EscapeString(s.Heading) + "</h3>")
		for _, para := range splitParas(s.Body) {
			b.WriteString("<p>" + html.EscapeString(para) + "</p>")
		}
	}
	return b.String()
}

func splitParas(body string) []string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	var out []string
	for _, p := range strings.Split(body, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func postedDate(j *Job) time.Time {
	if j.OpenedAt != nil {
		return *j.OpenedAt
	}
	return j.UpdatedAt
}

func (s *Salary) text() string {
	return fmt.Sprintf("%s - %s %s a %s", num(s.Min), num(s.Max), s.Currency, strings.ToLower(s.Period))
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// ---- schema.org JobPosting ---------------------------------------------------------------

type ldOrg struct {
	Type   string `json:"@type"`
	Name   string `json:"name"`
	SameAs string `json:"sameAs,omitempty"`
	Logo   string `json:"logo,omitempty"`
}

type ldAddress struct {
	Type     string `json:"@type"`
	Locality string `json:"addressLocality,omitempty"`
	Region   string `json:"addressRegion,omitempty"`
	Postal   string `json:"postalCode,omitempty"`
	Country  string `json:"addressCountry,omitempty"`
}

type ldPlace struct {
	Type    string    `json:"@type"`
	Address ldAddress `json:"address"`
}

type ldCountry struct {
	Type string `json:"@type"`
	Name string `json:"name"`
}

type ldQuantity struct {
	Type string  `json:"@type"`
	Min  float64 `json:"minValue"`
	Max  float64 `json:"maxValue"`
	Unit string  `json:"unitText"`
}

type ldMoney struct {
	Type     string     `json:"@type"`
	Currency string     `json:"currency"`
	Value    ldQuantity `json:"value"`
}

type ldID struct {
	Type  string `json:"@type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ldJob struct {
	Context      string      `json:"@context"`
	Type         string      `json:"@type"`
	Title        string      `json:"title"`
	Description  string      `json:"description"`
	Identifier   ldID        `json:"identifier"`
	DatePosted   string      `json:"datePosted"`
	ValidThrough string      `json:"validThrough,omitempty"`
	Employment   string      `json:"employmentType"`
	Org          ldOrg       `json:"hiringOrganization"`
	Location     *ldPlace    `json:"jobLocation,omitempty"`
	LocationType string      `json:"jobLocationType,omitempty"`
	Applicant    []ldCountry `json:"applicantLocationRequirements,omitempty"`
	BaseSalary   *ldMoney    `json:"baseSalary,omitempty"`
	DirectApply  bool        `json:"directApply"`
	URL          string      `json:"url"`
}

// JobPostingJSONLD returns the schema.org JobPosting for a job as indented JSON. Output is
// deterministic. json.Marshal escapes <, > and &, so the bytes are safe inside a
// <script type="application/ld+json"> element. The salary appears only when the job
// publishes it.
func JobPostingJSONLD(site SiteInfo, j *Job) ([]byte, error) {
	ld := ldJob{
		Context: "https://schema.org", Type: "JobPosting", Title: j.Title, Description: DescriptionHTML(j.Sections),
		Identifier: ldID{Type: "PropertyValue", Name: site.Company, Value: j.ID},
		DatePosted: day(postedDate(j)), Employment: EmploymentTypes[j.EmploymentType],
		Org:         ldOrg{Type: "Organization", Name: site.Company, SameAs: site.BaseURL, Logo: site.LogoURL},
		DirectApply: true, URL: site.JobURL(j),
	}
	if j.ValidThrough != nil {
		ld.ValidThrough = day(*j.ValidThrough)
	}
	if l := j.Location; l != (Location{}) {
		ld.Location = &ldPlace{Type: "Place", Address: ldAddress{Type: "PostalAddress", Locality: l.City, Region: l.Region, Postal: l.PostalCode, Country: l.Country}}
	}
	if j.Remote == "remote" {
		ld.LocationType = "TELECOMMUTE"
		countries := j.ApplicantCountries
		if len(countries) == 0 && j.Location.Country != "" {
			countries = []string{j.Location.Country} // Google requires applicantLocationRequirements for TELECOMMUTE
		}
		for _, c := range countries {
			ld.Applicant = append(ld.Applicant, ldCountry{Type: "Country", Name: c})
		}
	}
	if j.PublishSalary && j.Salary != nil {
		s := j.Salary
		ld.BaseSalary = &ldMoney{Type: "MonetaryAmount", Currency: s.Currency, Value: ldQuantity{Type: "QuantitativeValue", Min: s.Min, Max: s.Max, Unit: SalaryPeriods[s.Period]}}
	}
	return json.MarshalIndent(ld, "", "  ")
}

// JobsJSON is the generic JSON feed: an array of JobPosting objects for every listed job.
func JobsJSON(site SiteInfo, js []*Job) ([]byte, error) {
	out := []json.RawMessage{}
	for _, j := range js {
		if !j.listed() {
			continue
		}
		b, err := JobPostingJSONLD(site, j)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return json.MarshalIndent(out, "", "  ")
}

// ---- sitemap -------------------------------------------------------------------------------

type smURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type smSet struct {
	XMLName xml.Name `xml:"urlset"`
	NS      string   `xml:"xmlns,attr"`
	URLs    []smURL  `xml:"url"`
}

// Sitemap lists the careers page and every open job, with lastmod, for Google and other
// crawlers. Only open jobs are listed: closed ones carry noindex and leave the sitemap.
func Sitemap(site SiteInfo, js []*Job) ([]byte, error) {
	set := smSet{NS: "http://www.sitemaps.org/schemas/sitemap/0.9", URLs: []smURL{{Loc: site.CareersURL()}}}
	for _, j := range js {
		if j.listed() {
			set.URLs = append(set.URLs, smURL{Loc: site.JobURL(j), LastMod: day(j.UpdatedAt)})
		}
	}
	return marshalXML(set)
}

func marshalXML(v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// ---- generic XML feed ------------------------------------------------------------------------

type gfLocation struct {
	City       string `xml:"city,omitempty"`
	Region     string `xml:"region,omitempty"`
	Country    string `xml:"country,omitempty"`
	PostalCode string `xml:"postal_code,omitempty"`
}

type gfSalary struct {
	Min      string `xml:"min"`
	Max      string `xml:"max"`
	Currency string `xml:"currency"`
	Period   string `xml:"period"`
}

type gfJob struct {
	ID          string     `xml:"id"`
	Title       string     `xml:"title"`
	URL         string     `xml:"url"`
	ApplyURL    string     `xml:"apply_url"`
	Department  string     `xml:"department,omitempty"`
	Employment  string     `xml:"employment_type"`
	Remote      string     `xml:"remote"`
	Location    gfLocation `xml:"location"`
	Applicants  []string   `xml:"applicant_countries>country,omitempty"`
	Salary      *gfSalary  `xml:"salary"`
	DatePosted  string     `xml:"date_posted"`
	ValidThru   string     `xml:"valid_through,omitempty"`
	Description string     `xml:"description_html"`
}

type gfFeed struct {
	XMLName   xml.Name `xml:"jobs"`
	Generated string   `xml:"generated,attr"`
	Company   string   `xml:"company,attr"`
	Jobs      []gfJob  `xml:"job"`
}

// GenericFeed is the project's own XML job feed: one element per listed job with plain
// fields, for aggregators that accept a custom format or for your own tooling.
func GenericFeed(site SiteInfo, js []*Job, now time.Time) ([]byte, error) {
	f := gfFeed{Generated: now.UTC().Format(time.RFC3339), Company: site.Company}
	for _, j := range js {
		if !j.listed() {
			continue
		}
		g := gfJob{
			ID: j.ID, Title: j.Title, URL: site.JobURL(j), ApplyURL: site.JobURL(j) + "#apply", Department: j.Department,
			Employment: j.EmploymentType, Remote: j.Remote, Location: gfLocation(j.Location), Applicants: j.ApplicantCountries,
			DatePosted: day(postedDate(j)), Description: DescriptionHTML(j.Sections),
		}
		if j.ValidThrough != nil {
			g.ValidThru = day(*j.ValidThrough)
		}
		if j.PublishSalary && j.Salary != nil {
			g.Salary = &gfSalary{Min: num(j.Salary.Min), Max: num(j.Salary.Max), Currency: j.Salary.Currency, Period: j.Salary.Period}
		}
		f.Jobs = append(f.Jobs, g)
	}
	return marshalXML(f)
}

// ---- Indeed-style XML feed -------------------------------------------------------------------

// The description element needs its own tag with CDATA, so it is wrapped.
type inDesc struct {
	Text string `xml:",cdata"`
}

type inJobX struct {
	Title       string `xml:"title"`
	Date        string `xml:"date"`
	Reference   string `xml:"referencenumber"`
	URL         string `xml:"url"`
	Company     string `xml:"company"`
	SourceName  string `xml:"sourcename"`
	City        string `xml:"city,omitempty"`
	State       string `xml:"state,omitempty"`
	Country     string `xml:"country,omitempty"`
	PostalCode  string `xml:"postalcode,omitempty"`
	Description inDesc `xml:"description"`
	Salary      string `xml:"salary,omitempty"`
	JobType     string `xml:"jobtype,omitempty"`
	Category    string `xml:"category,omitempty"`
	RemoteType  string `xml:"remotetype,omitempty"`
}

type inSource struct {
	XMLName       xml.Name `xml:"source"`
	Publisher     string   `xml:"publisher"`
	PublisherURL  string   `xml:"publisherurl"`
	LastBuildDate string   `xml:"lastBuildDate"`
	Jobs          []inJobX `xml:"job"`
}

var indeedJobTypes = map[string]string{"full_time": "fulltime", "part_time": "parttime", "contract": "contract", "temporary": "temporary", "intern": "internship", "volunteer": "volunteer", "other": "other"}

// IndeedFeed renders the XML format Indeed documents for job feeds. Indeed (and Glassdoor,
// which is operated through Indeed) accept such feeds only from approved partners and ATS
// vendors, so this output is a correct file, not proof that Indeed will ingest it. Other
// aggregators accept the same shape. See docs/recruiting.md.
func IndeedFeed(site SiteInfo, js []*Job, now time.Time) ([]byte, error) {
	src := inSource{Publisher: site.Company, PublisherURL: site.BaseURL, LastBuildDate: now.UTC().Format(time.RFC1123)}
	for _, j := range js {
		if !j.listed() {
			continue
		}
		x := inJobX{
			Title: j.Title, Date: postedDate(j).UTC().Format(time.RFC1123), Reference: j.ID, URL: site.JobURL(j),
			Company: site.Company, SourceName: site.Company, City: j.Location.City, State: j.Location.Region,
			Country: j.Location.Country, PostalCode: j.Location.PostalCode, Description: inDesc{Text: DescriptionHTML(j.Sections)},
			JobType: indeedJobTypes[j.EmploymentType], Category: j.Department,
		}
		switch j.Remote {
		case "remote":
			x.RemoteType = "Fully remote"
		case "hybrid":
			x.RemoteType = "Hybrid"
		}
		if j.PublishSalary && j.Salary != nil {
			x.Salary = j.Salary.text()
		}
		src.Jobs = append(src.Jobs, x)
	}
	return marshalXML(src)
}
