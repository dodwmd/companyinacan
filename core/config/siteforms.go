package config

// The link between site.contact and the forms module (docs/site-contact.md): which forms the module
// serves, whether it serves them to the public, and what is wrong with a contact section. The site
// renderer and `can validate` apply the same rules.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultContactForm is the form the shipped forms module serves.
const DefaultContactForm = "contact"

// HostedForm is a form of the forms module, as far as a site link to it cares.
type HostedForm struct {
	ID      string `yaml:"id"`
	Captcha bool   `yaml:"captcha"` // a CAPTCHA form has no hosted page
}

// ShippedForms are the forms of the forms module as shipped (a contract test keeps the two in step).
func ShippedForms() []HostedForm { return []HostedForm{{ID: DefaultContactForm}} }

// FormsOfModules returns the forms the forms module in modulesDir is configured with (its chart
// values), or the shipped ones when that directory does not hold the module.
func FormsOfModules(modulesDir string) ([]HostedForm, error) {
	p := filepath.Join(modulesDir, "forms", "chart", "values.yaml")
	b, err := os.ReadFile(p) //nolint:gosec // the operator's modules directory
	if errors.Is(err, fs.ErrNotExist) {
		return ShippedForms(), nil
	}
	if err != nil {
		return nil, err
	}
	var v struct {
		Config struct {
			Forms []HostedForm `yaml:"forms"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return append([]HostedForm{}, v.Config.Forms...), nil
}

// AbsentForms says what it means that company.yaml does not mention the forms module.
type AbsentForms int

const (
	// AbsentUnknown: nothing is known about the install; the link is kept and a warning given.
	AbsentUnknown AbsentForms = iota
	// AbsentAdded: `can up` adds the module (the default bundle).
	AbsentAdded
	// AbsentOff: nothing adds it (`can up --no-default-bundle`).
	AbsentOff
)

// ContactIssue is one finding about site.contact. A warning does not stop a build.
type ContactIssue struct {
	Path, Rule, Message string
	Warning             bool
}

// ContactCheck is the outcome of CheckContact.
type ContactCheck struct {
	// Show is false when there is no section, or it must be left out because its link would lead nowhere.
	Show   bool
	Issues []ContactIssue
}

// CheckContact checks site.contact against the forms module: forms are the ones it serves.
func CheckContact(c *Company, forms []HostedForm, absent AbsentForms) ContactCheck {
	s := c.Site.Contact
	if s == nil {
		return ContactCheck{}
	}
	res := ContactCheck{Show: true}
	warn := func(msg string) {
		res.Issues = append(res.Issues, ContactIssue{Path: "/site/contact", Rule: "semantic.contact-forms-off", Warning: true, Message: msg})
	}
	leaveOut := func(why string) {
		res.Show = false
		warn("site.contact is left out of the site: " + why)
	}
	entry, listed := c.Modules["forms"]
	switch {
	case listed && !entry.IsEnabled():
		leaveOut("the forms module is disabled (modules.forms.enabled: false), so the form would not answer")
	case listed && !entry.Exposure().PublicOn():
		leaveOut("modules.forms.expose.public is false, so /_forms is not routed")
	case !listed && absent == AbsentOff:
		leaveOut("the forms module is not enabled; add `forms: {}` under modules:")
	case !listed && absent == AbsentUnknown:
		warn("site.contact needs the forms module, which company.yaml does not enable: `can up` adds it by default, `can apply` does not; add `forms: {}` under modules:")
	}
	if !res.Show {
		return res
	}
	id := s.FormID()
	bad := func(msg string) {
		res.Issues = append(res.Issues, ContactIssue{Path: "/site/contact/form", Rule: "semantic.contact-form", Message: msg})
	}
	i := slices.IndexFunc(forms, func(f HostedForm) bool { return f.ID == id })
	switch {
	case i < 0:
		ids := make([]string, 0, len(forms))
		for _, f := range forms {
			ids = append(ids, f.ID)
		}
		bad(fmt.Sprintf("site.contact.form: the forms module has no form %q (it has: %s)", id, strings.Join(ids, ", ")))
	case forms[i].Captcha:
		bad(fmt.Sprintf("site.contact.form: form %q has a CAPTCHA, so the forms module serves no page for it", id))
	}
	return res
}
