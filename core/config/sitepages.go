package config

// Checks of site.pages and site.careers that the schema cannot express. The site renderer and
// `can validate` apply the same rules (docs/site-editor.md).

import "fmt"

// Page slugs the site itself uses.
var reservedSlugs = map[string]bool{"index": true, "404": true, "assets": true, "robots": true, "sitemap": true, "healthz": true}

// PageIssue is one finding about site.pages.
type PageIssue struct {
	Index   int // the page's position in site.pages
	Message string
}

// CheckPages reports slugs that collide with the site's own files or with each other.
func CheckPages(pages []SitePage) []PageIssue {
	var out []PageIssue
	seen := map[string]bool{}
	for i, p := range pages {
		switch {
		case reservedSlugs[p.Slug]:
			out = append(out, PageIssue{Index: i, Message: fmt.Sprintf("%q is used by the site itself", p.Slug)})
		case seen[p.Slug]:
			out = append(out, PageIssue{Index: i, Message: fmt.Sprintf("%q is already used by another page", p.Slug)})
		}
		seen[p.Slug] = true
	}
	return out
}

// CheckCareers reports whether the careers link of site.careers leads somewhere, and why not otherwise.
// A company.yaml without the section shows nothing and has no warning.
func CheckCareers(c *Company) (show bool, warning string) {
	if c.Site.Careers == nil {
		return false, ""
	}
	e, listed := c.Modules["jobs"]
	why := ""
	switch {
	case !listed || !e.IsEnabled():
		why = "the jobs module is not enabled"
	case !e.Exposure().PublicOn():
		why = "modules.jobs.expose.public is false, so the careers pages are not routed"
	default:
		return true, ""
	}
	return false, "site.careers is left out of the site: " + why
}
