package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/forms"
)

// publicHandler serves the careers pages, the apply form, the sitemap and the feeds. Nothing
// here needs credentials; the apply endpoint is protected like the forms gateway: a signed
// timing token, a honeypot, a same-origin check, a per-address rate limit and an optional
// CAPTCHA.
type publicHandler struct {
	store     *Store
	cfg       *Config
	site      SiteInfo
	signer    *forms.TokenSigner
	captcha   forms.CaptchaVerifier
	limiter   *rateLimiter
	log       *slog.Logger
	now       func() time.Time
	dbTimeout time.Duration
}

func (p *publicHandler) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /careers", p.list)
	mux.HandleFunc("GET /careers/applied", p.applied)
	mux.HandleFunc("GET /careers/sitemap.xml", p.sitemap)
	mux.HandleFunc("GET /careers/jobs.json", p.jobsJSON)
	mux.HandleFunc("GET /careers/assets/careers.css", p.css)
	mux.HandleFunc("GET /careers/{slug}", p.detail)
	mux.HandleFunc("POST /careers/{slug}/apply", p.apply)
	mux.HandleFunc("GET /robots.txt", p.robots)
	mux.HandleFunc("GET /feeds/generic.xml", p.genericFeed)
	mux.HandleFunc("GET /feeds/indeed.xml", p.indeedFeed)
}

func setPublicHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' https: data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

func (p *publicHandler) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), p.dbTimeout)
}

// ---- templates -----------------------------------------------------------------------------

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"paras":  splitParas,
	"money":  money,
	"remote": remoteLabel,
	"emp":    empLabel,
	"loc":    locLabel,
}).Parse(`{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
{{if .NoIndex}}<meta name="robots" content="noindex, nofollow">{{end}}
<link rel="stylesheet" href="/careers/assets/careers.css">
{{if .LD}}<script type="application/ld+json">{{.LD}}</script>{{end}}
</head><body><header><a href="/careers">{{.Company}} careers</a></header><main>{{end}}
{{define "foot"}}</main><footer>Applications are processed as described in {{.Company}}'s privacy notice.</footer></body></html>{{end}}

{{define "list"}}{{template "head" .}}<h1>Open roles at {{.Company}}</h1>
<form method="get" action="/careers" class="filters">
<label>Search <input type="search" name="q" value="{{.Q}}" maxlength="80"></label>
<label>Department <select name="dept"><option value="">Any</option>{{range .Depts}}<option{{if eq . $.Dept}} selected{{end}}>{{.}}</option>{{end}}</select></label>
<label>Work <select name="remote"><option value="">Any</option><option value="onsite"{{if eq .Remote "onsite"}} selected{{end}}>On-site</option><option value="hybrid"{{if eq .Remote "hybrid"}} selected{{end}}>Hybrid</option><option value="remote"{{if eq .Remote "remote"}} selected{{end}}>Remote</option></select></label>
<label>Type <select name="type"><option value="">Any</option>{{range .Types}}<option value="{{.Key}}"{{if eq .Key $.Type}} selected{{end}}>{{.Label}}</option>{{end}}</select></label>
<button type="submit">Filter</button></form>
{{if .Jobs}}<ul class="jobs">{{range .Jobs}}<li><a href="/careers/{{.Slug}}">{{.Title}}</a><span>{{.Department}}{{if .Department}} · {{end}}{{loc .}} · {{remote .Remote}} · {{emp .EmploymentType}}</span></li>{{end}}</ul>
{{else}}<p>There are no open roles that match right now.</p>{{end}}{{template "foot" .}}{{end}}

{{define "detail"}}{{template "head" .}}<article><h1>{{.Job.Title}}</h1>
<p class="meta">{{.Job.Department}}{{if .Job.Department}} · {{end}}{{loc .Job}} · {{remote .Job.Remote}} · {{emp .Job.EmploymentType}}</p>
{{if .Notice}}<p class="notice">{{.Notice}}</p>{{end}}
{{if and .Job.PublishSalary .Job.Salary}}<p class="pay">Pay: {{money .Job.Salary}}</p>{{end}}
{{range .Job.Sections}}<section><h2>{{.Heading}}</h2>{{range paras .Body}}<p>{{.}}</p>{{end}}</section>{{end}}</article>
{{if .Accepting}}<section id="apply"><h2>Apply</h2>
<form method="post" action="/careers/{{.Job.Slug}}/apply" enctype="multipart/form-data">
<input type="hidden" name="_t" value="{{.Token}}">
<p class="hp" aria-hidden="true"><label>Leave this empty <input type="text" name="website" tabindex="-1" autocomplete="off"></label></p>
<label>Full name <input type="text" name="name" required maxlength="100" autocomplete="name"></label>
<label>Email <input type="email" name="email" required maxlength="254" autocomplete="email"></label>
<label>Phone (optional) <input type="tel" name="phone" maxlength="30" autocomplete="tel"></label>
<label>Cover letter (optional) <textarea name="cover_letter" rows="6" maxlength="5000"></textarea></label>
{{if .ResumeOK}}<label>Resume (optional; PDF, DOCX, ODT, RTF or text, up to {{.MaxMB}} MB) <input type="file" name="resume" accept=".pdf,.docx,.odt,.rtf,.txt"></label>{{end}}
{{if .EEO}}<fieldset><legend>Voluntary diversity questions (not shown to interviewers, kept apart from your application)</legend>
{{range .EEO}}<label>{{.Label}} <select name="{{.Name}}"><option value="">Prefer not to answer</option>{{range .Options}}<option value="{{.}}">{{.}}</option>{{end}}</select></label>{{end}}</fieldset>{{end}}
<label class="check"><input type="checkbox" name="consent" required> {{.ConsentText}}</label>
<label class="check"><input type="checkbox" name="talent_pool"> Keep my details so I can be considered for future roles.</label>
<button type="submit">Send application</button></form></section>{{end}}{{template "foot" .}}{{end}}

{{define "message"}}{{template "head" .}}<h1>{{.Heading}}</h1><p>{{.Message}}</p><p><a href="/careers">Back to open roles</a></p>{{template "foot" .}}{{end}}
`))

type pageData struct {
	Title, Company string
	NoIndex        bool
	LD             template.JS

	// list
	Jobs                  []*Job
	Q, Dept, Remote, Type string
	Depts                 []string
	Types                 []typeOpt

	// detail
	Job         *Job
	Notice      string
	Accepting   bool
	ResumeOK    bool
	Token       string
	MaxMB       int64
	EEO         []eeoQ
	ConsentText string

	Heading, Message string
}

type typeOpt struct{ Key, Label string }

type eeoQ struct {
	Name, Label string
	Options     []string
}

func money(s *Salary) string {
	return fmt.Sprintf("%s %s to %s per %s", s.Currency, commas(s.Min), commas(s.Max), s.Period)
}

func commas(f float64) string {
	s := num(f)
	whole, frac, _ := strings.Cut(s, ".")
	var out []byte
	for i, c := range []byte(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if frac != "" {
		return string(out) + "." + frac
	}
	return string(out)
}

func remoteLabel(r string) string {
	return map[string]string{"onsite": "On-site", "hybrid": "Hybrid", "remote": "Remote"}[r]
}

func empLabel(e string) string {
	return map[string]string{"full_time": "Full-time", "part_time": "Part-time", "contract": "Contract", "temporary": "Temporary", "intern": "Internship", "volunteer": "Volunteer", "other": "Other"}[e]
}

func locLabel(j *Job) string {
	parts := []string{}
	for _, s := range []string{j.Location.City, j.Location.Region, j.Location.Country} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "Anywhere"
	}
	return strings.Join(parts, ", ")
}

var typeOptions = func() []typeOpt {
	keys := []string{"full_time", "part_time", "contract", "temporary", "intern", "volunteer", "other"}
	out := make([]typeOpt, len(keys))
	for i, k := range keys {
		out[i] = typeOpt{k, empLabel(k)}
	}
	return out
}()

func (p *publicHandler) render(w http.ResponseWriter, status int, name string, d *pageData) {
	d.Company = p.site.Company
	var buf bytes.Buffer
	if err := pageTmpl.ExecuteTemplate(&buf, name, d); err != nil {
		p.log.Error("render", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setPublicHeaders(w)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	if d.NoIndex {
		h.Set("X-Robots-Tag", "noindex, nofollow")
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (p *publicHandler) message(w http.ResponseWriter, r *http.Request, status int, heading, msg string) {
	if wantsJSON(r) {
		setPublicHeaders(w)
		writeError(w, status, "rejected", msg)
		return
	}
	p.render(w, status, "message", &pageData{Title: heading, Heading: heading, Message: msg, NoIndex: true})
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// ---- pages ---------------------------------------------------------------------------------

func (p *publicHandler) list(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := p.ctx(r)
	defer cancel()
	js, err := p.store.PublicJobs(ctx)
	if err != nil {
		p.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	d := &pageData{Title: "Open roles at " + p.site.Company, Q: strings.TrimSpace(q.Get("q")), Dept: q.Get("dept"), Remote: q.Get("remote"), Type: q.Get("type"), Types: typeOptions}
	if len(d.Q) > 80 {
		d.Q = d.Q[:80]
	}
	seen := map[string]bool{}
	for _, j := range js {
		if j.Department != "" && !seen[j.Department] {
			seen[j.Department] = true
			d.Depts = append(d.Depts, j.Department)
		}
	}
	slices.Sort(d.Depts)
	for _, j := range js {
		if d.Q != "" && !strings.Contains(strings.ToLower(j.Title+" "+j.Department+" "+locLabel(j)), strings.ToLower(d.Q)) {
			continue
		}
		if (d.Dept != "" && j.Department != d.Dept) || (d.Remote != "" && j.Remote != d.Remote) || (d.Type != "" && j.EmploymentType != d.Type) {
			continue
		}
		d.Jobs = append(d.Jobs, j)
	}
	p.render(w, http.StatusOK, "list", d)
}

func (p *publicHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		w.Header().Set("Retry-After", "5")
		p.message(w, r, http.StatusServiceUnavailable, "Busy", "The page took too long to load. Please try again in a moment.")
		return
	}
	p.log.Error("public request failed", "error", err)
	p.message(w, r, http.StatusInternalServerError, "Something went wrong", "Please try again later.")
}

func (p *publicHandler) detail(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !slugRE.MatchString(slug) || len(slug) > 100 {
		p.notFound(w, r)
		return
	}
	ctx, cancel := p.ctx(r)
	defer cancel()
	j, err := p.store.PublicJob(ctx, slug)
	if errors.Is(err, ErrNotFound) {
		p.notFound(w, r)
		return
	}
	if err != nil {
		p.fail(w, r, err)
		return
	}
	d := &pageData{Title: j.Title + " - " + p.site.Company, Job: j, Accepting: j.accepting(p.now())}
	switch {
	case d.Accepting:
		ld, err := JobPostingJSONLD(p.site, j)
		if err != nil {
			p.fail(w, r, err)
			return
		}
		d.LD = template.JS(ld) //nolint:gosec // json.Marshal output: <, > and & are escaped, so it cannot close the script element
		d.Token = p.signer.Issue(j.Slug)
		d.MaxMB = p.cfg.Apply.MaxResumeBytes >> 20
		d.ResumeOK = p.store.resumesOpen()
		d.ConsentText = p.cfg.Apply.ConsentText
		if p.cfg.Apply.CollectEEO {
			for _, f := range EEOFields {
				d.EEO = append(d.EEO, eeoQ{Name: "eeo_" + f, Label: strings.ToUpper(f[:1]) + f[1:], Options: eeoOptions[f]})
			}
		}
	case j.Status == StatusPaused:
		d.NoIndex, d.Notice = true, "Applications for this role are paused right now."
	default:
		d.NoIndex, d.Notice = true, "This role is no longer open."
	}
	p.render(w, http.StatusOK, "detail", d)
}

func (p *publicHandler) notFound(w http.ResponseWriter, r *http.Request) {
	p.message(w, r, http.StatusNotFound, "Not found", "That role does not exist or is no longer listed.")
}

func (p *publicHandler) applied(w http.ResponseWriter, r *http.Request) {
	p.render(w, http.StatusOK, "message", &pageData{Title: "Application received", Heading: "Thank you", Message: "Your application has been received.", NoIndex: true})
}

const careersCSS = `body{font:16px/1.5 system-ui,sans-serif;max-width:48rem;margin:0 auto;padding:1rem;color:#1b1b1b}
header a{font-weight:600;text-decoration:none;color:inherit}
.jobs{list-style:none;padding:0}.jobs li{padding:.75rem 0;border-bottom:1px solid #ddd}.jobs span{display:block;color:#555;font-size:.9rem}
.meta,.pay{color:#555}.notice{background:#fff4d6;padding:.5rem .75rem;border-radius:4px}
form label{display:block;margin:.75rem 0}input[type=text],input[type=email],input[type=tel],input[type=search],textarea,select{display:block;width:100%;max-width:30rem;padding:.4rem;box-sizing:border-box}
.check{display:flex;gap:.5rem}.hp{position:absolute;left:-9999px}button{padding:.5rem 1rem;font:inherit}
.filters label{display:inline-block;margin-right:.75rem}footer{margin-top:2rem;color:#555;font-size:.85rem}`

func (p *publicHandler) css(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, careersCSS)
}

// ---- sitemap, robots, feeds --------------------------------------------------------------------

func (p *publicHandler) serveFeed(w http.ResponseWriter, r *http.Request, ctype string, build func([]*Job) ([]byte, error)) {
	ctx, cancel := p.ctx(r)
	defer cancel()
	js, err := p.store.PublicJobs(ctx)
	if err == nil {
		var b []byte
		if b, err = build(js); err == nil {
			setPublicHeaders(w)
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(b)
			return
		}
	}
	p.fail(w, r, err)
}

func (p *publicHandler) sitemap(w http.ResponseWriter, r *http.Request) {
	p.serveFeed(w, r, "application/xml; charset=utf-8", func(js []*Job) ([]byte, error) { return Sitemap(p.site, js) })
}

func (p *publicHandler) jobsJSON(w http.ResponseWriter, r *http.Request) {
	p.serveFeed(w, r, "application/json", func(js []*Job) ([]byte, error) { return JobsJSON(p.site, js) })
}

func (p *publicHandler) genericFeed(w http.ResponseWriter, r *http.Request) {
	p.serveFeed(w, r, "application/xml; charset=utf-8", func(js []*Job) ([]byte, error) { return GenericFeed(p.site, js, p.now()) })
}

func (p *publicHandler) indeedFeed(w http.ResponseWriter, r *http.Request) {
	p.serveFeed(w, r, "application/xml; charset=utf-8", func(js []*Job) ([]byte, error) { return IndeedFeed(p.site, js, p.now()) })
}

// robots serves a robots.txt for deployments where this service owns the host (for example
// jobs.example.com). On a shared host the company site's own robots.txt should list the
// sitemap URL instead.
func (p *publicHandler) robots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprintf(w, "User-agent: *\nAllow: /careers\nDisallow: /v1/\nDisallow: /careers/*/apply\n\nSitemap: %s/careers/sitemap.xml\n", p.site.BaseURL)
}

// ---- apply ---------------------------------------------------------------------------------------

const (
	maxFieldBytes = 20000
	formSlack     = 256 << 10
)

type applyForm struct {
	vals   map[string]string
	resume *ResumeMeta
}

func (p *publicHandler) apply(w http.ResponseWriter, r *http.Request) {
	setPublicHeaders(w)
	if ok, wait := p.limiter.allow(clientIP(r, p.cfg.TrustProxy)); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		p.message(w, r, http.StatusTooManyRequests, "Too many applications", "Please wait a while before sending another application.")
		return
	}
	if !p.checkOrigin(r) {
		p.message(w, r, http.StatusForbidden, "Not allowed", "Cross-origin applications are not accepted.")
		return
	}
	slug := r.PathValue("slug")
	ctx, cancel := p.ctx(r)
	defer cancel()
	if !slugRE.MatchString(slug) || len(slug) > 100 {
		p.notFound(w, r)
		return
	}
	j, err := p.store.PublicJob(ctx, slug)
	if errors.Is(err, ErrNotFound) {
		p.notFound(w, r)
		return
	}
	if err != nil {
		p.fail(w, r, err)
		return
	}
	if !j.accepting(p.now()) {
		p.message(w, r, http.StatusConflict, "Closed", "This role is no longer accepting applications.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, p.cfg.Apply.MaxResumeBytes+formSlack)
	f, status, msg := p.readForm(ctx, r)
	if f.resume != nil && f.resume.key != "" {
		defer func() {
			if f.resume != nil && f.resume.key != "" {
				_ = p.store.resumes.Delete(context.WithoutCancel(ctx), f.resume.key) // every path that does not store the application removes the file
			}
		}()
	}
	if msg != "" {
		p.message(w, r, status, "Could not send your application", msg)
		return
	}
	if err := p.signer.Check(j.Slug, f.vals["_t"], time.Duration(p.cfg.Apply.MinFillSeconds)*time.Second); err != nil {
		p.message(w, r, http.StatusUnprocessableEntity, "Could not send your application", "The form expired or was sent too quickly. Reload the page and try again.")
		return
	}
	if f.vals["website"] != "" { // honeypot: look like success so a bot learns nothing
		http.Redirect(w, r, "/careers/applied", http.StatusSeeOther)
		return
	}
	if p.captcha != nil {
		ok, err := p.captcha.Verify(ctx, f.vals[forms.CaptchaResponseField(p.cfg.Apply.CaptchaProvider)], clientIP(r, p.cfg.TrustProxy))
		if err != nil || !ok {
			p.message(w, r, http.StatusUnprocessableEntity, "Could not send your application", "The CAPTCHA check failed. Please try again.")
			return
		}
	}
	in := PublicApplication{JobSlug: slug, Name: f.vals["name"], Email: f.vals["email"], Phone: f.vals["phone"], CoverLetter: f.vals["cover_letter"],
		Source: f.vals["src"], Consent: f.vals["consent"] != "", TalentPool: f.vals["talent_pool"] != "", Resume: f.resume}
	if p.cfg.Apply.CollectEEO {
		in.EEO = &EEOInput{Gender: f.vals["eeo_gender"], Ethnicity: f.vals["eeo_ethnicity"], Veteran: f.vals["eeo_veteran"], Disability: f.vals["eeo_disability"]}
	}
	res, err := p.store.Apply(ctx, in)
	var ie *InvalidError
	switch {
	case errors.As(err, &ie):
		p.message(w, r, http.StatusUnprocessableEntity, "Could not send your application", ie.Error())
		return
	case errors.Is(err, ErrClosed):
		p.message(w, r, http.StatusConflict, "Closed", "This role is no longer accepting applications.")
		return
	case errors.Is(err, ErrNotFound):
		p.notFound(w, r)
		return
	case err != nil:
		p.fail(w, r, err)
		return
	}
	if res.Created {
		f.resume = nil // stored with the application: the deferred cleanup must not delete it
	}
	http.Redirect(w, r, "/careers/applied", http.StatusSeeOther)
}

// readForm parses the multipart (or urlencoded) body without writing temp files: fields are
// read with hard caps, the one resume part is streamed into the ResumeStore after a content
// sniff and a virus scan. On a problem it returns the status and a message; a stored file is
// reported in form.resume so the caller removes it.
func (p *publicHandler) readForm(ctx context.Context, r *http.Request) (*applyForm, int, string) {
	f := &applyForm{vals: map[string]string{}}
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	tooBig := func(err error) bool {
		var mb *http.MaxBytesError
		return errors.As(err, &mb)
	}
	if mt == "application/x-www-form-urlencoded" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			return f, http.StatusBadRequest, "The form could not be read."
		}
		vs, err := url.ParseQuery(string(body))
		if err != nil {
			return f, http.StatusBadRequest, "The form could not be read."
		}
		for k, v := range vs {
			if len(v) > 0 && len(v[0]) <= maxFieldBytes {
				f.vals[k] = v[0]
			}
		}
		return f, 0, ""
	}
	if mt != "multipart/form-data" || params["boundary"] == "" {
		return f, http.StatusUnsupportedMediaType, "Send the form as multipart/form-data."
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for parts := 0; ; parts++ {
		if parts > 40 {
			return f, http.StatusUnprocessableEntity, "Too many form fields."
		}
		part, err := mr.NextPart()
		if err == io.EOF {
			return f, 0, ""
		}
		if err != nil {
			if tooBig(err) {
				return f, http.StatusRequestEntityTooLarge, "The upload is too large."
			}
			return f, http.StatusBadRequest, "The form could not be read."
		}
		name := part.FormName()
		if part.FileName() == "" {
			b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if err != nil {
				if tooBig(err) {
					return f, http.StatusRequestEntityTooLarge, "The upload is too large."
				}
				return f, http.StatusBadRequest, "The form could not be read."
			}
			if len(b) > maxFieldBytes {
				return f, http.StatusUnprocessableEntity, "A field is too long."
			}
			if _, dup := f.vals[name]; !dup {
				f.vals[name] = string(b)
			}
			continue
		}
		if name != "resume" || f.resume != nil {
			return f, http.StatusUnprocessableEntity, "Only one file, named resume, is accepted."
		}
		meta, status, msg := p.storeResume(ctx, part)
		f.resume = meta
		if msg != "" {
			return f, status, msg
		}
	}
}

func (p *publicHandler) storeResume(ctx context.Context, part *multipart.Part) (*ResumeMeta, int, string) {
	if !p.store.resumesOpen() {
		return nil, http.StatusUnprocessableEntity, "Resume uploads are not available right now. You can apply without one."
	}
	name := SafeFilename(part.FileName())
	head := make([]byte, 512)
	n, err := io.ReadFull(part, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return nil, http.StatusRequestEntityTooLarge, "The resume is too large."
		}
		return nil, http.StatusBadRequest, "The resume could not be read."
	}
	head = head[:n]
	if n == 0 {
		return nil, http.StatusUnprocessableEntity, "The resume file is empty."
	}
	ct, err := SniffResume(name, head)
	if err != nil {
		return nil, http.StatusUnprocessableEntity, err.Error()
	}
	max := p.cfg.Apply.MaxResumeBytes
	key, size, sum, err := p.store.resumes.Put(ctx, io.MultiReader(bytes.NewReader(head), part), max)
	if err != nil {
		var mb *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &mb) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("The resume is larger than %d MB.", max>>20)
		}
		p.log.Error("resume store", "error", err)
		return nil, http.StatusServiceUnavailable, "The resume could not be saved. Please try again later."
	}
	meta := &ResumeMeta{Name: name, Type: ct, Size: size, SHA256: sum, key: key}
	rc, err := p.store.resumes.Open(ctx, key)
	if err != nil {
		return meta, http.StatusServiceUnavailable, "The resume could not be checked. Please try again later."
	}
	verdict, err := p.store.scanner.Scan(ctx, name, rc)
	_ = rc.Close()
	switch {
	case err != nil:
		p.log.Error("resume scan", "error", err)
		return meta, http.StatusServiceUnavailable, "The resume could not be checked. Please try again later."
	case verdict == ScanInfected:
		return meta, http.StatusUnprocessableEntity, "The file was rejected by the virus scan."
	case verdict == ScanSkipped && p.cfg.Apply.ScannerRequired:
		return meta, http.StatusServiceUnavailable, "Resume uploads are unavailable right now."
	}
	meta.Scan = string(verdict)
	return meta, 0, ""
}

// checkOrigin enforces same-origin submissions: an Origin header must match the host the
// request was sent to or be on the allow-list. Requests without Origin pass.
func (p *publicHandler) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := r.Host
	if p.cfg.TrustProxy {
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			host = xfh
		}
	}
	if u, err := url.Parse(origin); err == nil && u.Host != "" && u.Host == host {
		return true
	}
	return slices.Contains(p.cfg.Apply.AllowedOrigins, origin)
}
