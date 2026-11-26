package finance

// The public signing pages (specs/049-contract-esign). There is no session: the single-use token in
// the path is the only credential. The access log records the route pattern, never the path; no
// page loads anything from elsewhere and the referrer policy keeps the address from leaking.

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxSignForm = 16 << 10

type signSite struct {
	a    *api
	mux  *http.ServeMux
	once sync.Once
	tpl  *template.Template
}

func newSignSite(a *api) http.Handler {
	s := &signSite{a: a, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /public/sign/assets/sign.css", s.css)
	s.mux.HandleFunc("GET /public/sign/{token}", s.show)
	s.mux.HandleFunc("POST /public/sign/{token}", s.submit)
	s.mux.HandleFunc("GET /public/sign/{token}/document", s.document)
	s.mux.HandleFunc(signPathPrefix, s.invalidLink)
	return s
}

type signPage struct {
	Kind     string // form, signed, declined, invalid, busy, error
	Title    string
	View     *SignerView
	Receipt  *SignReceipt
	Token    string // only for the form's own action and the download link
	Consent  string
	Name     string
	Reason   string
	Problems []ProblemField
}

func (s *signSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Cache-Control", "no-store")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	s.mux.ServeHTTP(w, r)
}

func (s *signSite) render(w http.ResponseWriter, status int, p signPage) {
	s.once.Do(func() {
		s.tpl = template.Must(template.New("sign.html").Funcs(template.FuncMap{
			"when": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		}).ParseFS(uiAssets, "ui/public/sign.html"))
	})
	var buf bytes.Buffer
	if err := s.tpl.Execute(&buf, p); err != nil {
		s.a.log.Error("signing page render", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// invalidLink is the one answer for every link that does not work. Bad links count against the
// client address; a working link is never throttled.
func (s *signSite) invalidLink(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.a.failures.allow(clientIP(r, s.a.trustProxy)); !ok {
		select {
		case <-time.After(s.a.penalty):
		case <-r.Context().Done():
		}
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		s.render(w, http.StatusTooManyRequests, signPage{Kind: "busy", Title: "Too many requests"})
		return
	}
	s.render(w, http.StatusNotFound, signPage{Kind: "invalid", Title: "This link does not work"})
}

func (s *signSite) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		s.invalidLink(w, r)
		return
	}
	s.a.log.Error("signing page", "route", r.Pattern, "error", err)
	s.render(w, http.StatusInternalServerError, signPage{Kind: "error", Title: "Something went wrong"})
}

func (s *signSite) show(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	v, err := s.a.store.ViewSigning(r.Context(), token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, http.StatusOK, signPage{Kind: "form", Title: v.Title, View: v, Token: token, Consent: ConsentText})
}

// document downloads the exact bytes whose SHA-256 the page shows.
func (s *signSite) document(w http.ResponseWriter, r *http.Request) {
	v, err := s.a.store.ViewSigning(r.Context(), r.PathValue("token"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="contract-%d-v%d.txt"`, v.ContractID, v.Version))
	_, _ = io.WriteString(w, v.Body)
}

func (s *signSite) submit(w http.ResponseWriter, r *http.Request) {
	// A forged cross-site post would already need the link; this is a second line. (Origin is "null"
	// under the no-referrer policy, so it is not looked at.)
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		s.render(w, http.StatusForbidden, signPage{Kind: "error", Title: "Open the link again and retry"})
		return
	}
	token := r.PathValue("token")
	v, err := s.a.store.ViewSigning(r.Context(), token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSignForm)
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		s.render(w, http.StatusBadRequest, signPage{Kind: "error", Title: "The form could not be read"})
		return
	}
	page := signPage{Kind: "form", Title: v.Title, View: v, Token: token, Consent: ConsentText, Name: r.PostForm.Get("typed_name"), Reason: r.PostForm.Get("reason")}
	ip := clientIP(r, s.a.trustProxy)
	var receipt *SignReceipt
	switch r.PostForm.Get("action") {
	case "sign":
		receipt, err = s.a.store.Sign(r.Context(), token, SignInput{TypedName: page.Name, Consent: r.PostForm.Get("consent") == "yes", SHA256: r.PostForm.Get("sha256"), IP: ip})
	case "decline":
		err = s.a.store.Decline(r.Context(), token, strings.ToValidUTF8(page.Reason, ""), ip)
	default:
		err = invalid("action", "choose sign or decline")
	}
	var bad *InvalidError
	switch {
	case errors.As(err, &bad):
		page.Problems = bad.Problems
		s.render(w, http.StatusUnprocessableEntity, page)
	case err != nil:
		s.fail(w, r, err)
	case receipt != nil:
		s.render(w, http.StatusOK, signPage{Kind: "signed", Title: "Signed", Receipt: receipt})
	default:
		s.render(w, http.StatusOK, signPage{Kind: "declined", Title: "Declined"})
	}
}

func (s *signSite) css(w http.ResponseWriter, _ *http.Request) {
	b, err := uiAssets.ReadFile("ui/static/finance.css")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}
