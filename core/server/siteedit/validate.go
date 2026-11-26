package siteedit

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/schema"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/site"
)

// FieldError is one reason a change cannot be published. Field is the path inside `site:` with dots
// ("hero.cta.href", "features.0.title"), "" when it is not about one field.
type FieldError struct{ Field, Message string }

// logoFS serves the published logo under the name company.yaml gives it.
type logoFS struct {
	name string
	data []byte
}

func (f logoFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func (f logoFS) ReadFile(name string) ([]byte, error) {
	if f.data == nil || name != f.name {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errNoLogo}
	}
	return f.data, nil
}

var (
	errNoLogo = errors.New("the logo file is not on the server: publish the site once with `can up`")
)

// assets is the filesystem the renderer reads company.yaml's logo from: the logo of the published
// site, as long as it is the same kind of file.
func assets(ref string, p Published) fs.FS {
	clean := path.Clean(ref)
	f := logoFS{name: clean}
	if p.Logo != nil && strings.EqualFold(path.Ext(clean), path.Ext(p.LogoName)) {
		f.data = p.Logo
	}
	return f
}

var schemaMessages = map[string]string{
	"schema.required": "This field is required.",
	"schema.length":   "Too short or too long.",
	"schema.pattern":  "Not in the expected format.",
	"schema.maxItems": "Too many entries.",
	"schema.minItems": "At least one entry is required.",
}

func schemaErrors(ps []problem.Problem) []FieldError {
	var out []FieldError
	for _, p := range ps {
		if p.Severity != problem.Error {
			continue
		}
		field, inSite := strings.CutPrefix(p.Path, "/site/")
		msg := schemaMessages[p.Rule]
		if msg == "" {
			msg = p.Message
		}
		if !inSite {
			out = append(out, FieldError{Message: p.Path + ": " + p.Message})
			continue
		}
		field = strings.ReplaceAll(field, "/", ".")
		switch {
		case field == "brand.colours.0" || field == "brand.colours.1":
			msg = "Use a colour such as #2457c5."
		case strings.HasSuffix(field, ".href"):
			msg = "Use an https:// or mailto: address, a path starting with / or a #section."
		case strings.HasSuffix(field, ".slug"):
			msg = "Lower-case letters, digits and hyphens, starting with a letter."
		}
		out = append(out, FieldError{Field: field, Message: msg})
	}
	return out
}

var (
	reProblem = regexp.MustCompile(`^site\.([A-Za-z0-9_.\[\]]+): (.*)$`)
	reIndex   = regexp.MustCompile(`\[(\d+)\]`)
)

func renderErrors(err error) []FieldError {
	var se *site.Error
	if !errors.As(err, &se) {
		return []FieldError{{Message: err.Error()}}
	}
	var out []FieldError
	for _, p := range se.Problems {
		m := reProblem.FindStringSubmatch(p)
		if m == nil {
			out = append(out, FieldError{Message: p})
			continue
		}
		field := reIndex.ReplaceAllStringFunc(m[1], func(s string) string {
			n, _ := strconv.Atoi(s[1 : len(s)-1])
			return fmt.Sprintf(".%d", n)
		})
		out = append(out, FieldError{Field: field, Message: m[2]})
	}
	return out
}

// check validates the whole document as can-core would apply it and renders its site.
func (s *Service) check(doc []byte, p Published) (*config.Company, *site.Output, []FieldError) {
	d, _ := config.LoadBytes("company.yaml", doc)
	if d == nil {
		return nil, nil, []FieldError{{Message: "company.yaml cannot be read"}}
	}
	if sp := schema.Validate(schema.Company, d); problem.HasErrors(sp) {
		return nil, nil, schemaErrors(sp)
	}
	c, err := config.DecodeCompany(d)
	if err != nil {
		return nil, nil, []FieldError{{Message: "company.yaml cannot be read"}}
	}
	forms, err := config.FormsOfModules(s.ModulesDir)
	if err != nil {
		return nil, nil, []FieldError{{Message: err.Error()}}
	}
	base := p.BaseURL
	if base == "" {
		base = s.BaseURL
	}
	out, err := site.Render(c, site.Options{BaseURL: base, Assets: assets(c.Site.Brand.Logo, p), Forms: forms, AbsentForms: config.AbsentOff})
	if err != nil {
		return c, nil, renderErrors(err)
	}
	// The document must still be one can-core can apply.
	if _, lp := reconcile.LoadDesiredBytes("company.yaml", doc, s.ModulesDir); problem.HasErrors(lp) {
		var fe []FieldError
		for _, x := range lp {
			if x.Severity == problem.Error {
				fe = append(fe, FieldError{Message: x.Message})
			}
		}
		return c, nil, fe
	}
	return c, out, nil
}
