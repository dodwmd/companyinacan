package config

// Site is the `site:` section of company.yaml: the content the renderer turns into a static site.
type Site struct {
	Tagline     string        `yaml:"tagline"`
	Description string        `yaml:"description"`
	Template    string        `yaml:"template"`
	Lang        string        `yaml:"lang"`
	OGImage     string        `yaml:"og_image"`
	NoIndex     bool          `yaml:"noindex"`
	SameAs      []string      `yaml:"same_as"`
	Brand       SiteBrand     `yaml:"brand"`
	Hero        *SiteHero     `yaml:"hero"`
	Features    []SiteFeature `yaml:"features"`
	Pricing     *SitePricing  `yaml:"pricing"`
	FAQ         []SiteFAQ     `yaml:"faq"`
	CTA         *SiteCTA      `yaml:"cta"`
	Contact     *SiteContact  `yaml:"contact"`
	Footer      SiteFooter    `yaml:"footer"`
}

// SiteBrand holds colours and the logo path.
type SiteBrand struct {
	Colours []string `yaml:"colours"`
	Logo    string   `yaml:"logo"`
}

// SiteLink is a labelled link or button.
type SiteLink struct {
	Label string `yaml:"label"`
	Href  string `yaml:"href"`
}

// SiteHero is the top section.
type SiteHero struct {
	Headline     string    `yaml:"headline"`
	Subheadline  string    `yaml:"subheadline"`
	CTA          *SiteLink `yaml:"cta"`
	SecondaryCTA *SiteLink `yaml:"secondary_cta"`
}

// SiteFeature is one feature card.
type SiteFeature struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
}

// SitePricing is the pricing section.
type SitePricing struct {
	Title    string     `yaml:"title"`
	Subtitle string     `yaml:"subtitle"`
	Plans    []SitePlan `yaml:"plans"`
}

// SitePlan is one pricing plan.
type SitePlan struct {
	Name        string    `yaml:"name"`
	Price       string    `yaml:"price"`
	Period      string    `yaml:"period"`
	Description string    `yaml:"description"`
	Features    []string  `yaml:"features"`
	CTA         *SiteLink `yaml:"cta"`
	Highlighted bool      `yaml:"highlighted"`
}

// SiteFAQ is one question and answer.
type SiteFAQ struct {
	Question string `yaml:"question"`
	Answer   string `yaml:"answer"`
}

// SiteCTA is the closing call to action.
type SiteCTA struct {
	Headline string   `yaml:"headline"`
	Text     string   `yaml:"text"`
	Button   SiteLink `yaml:"button"`
}

// SiteContact is the contact section: a button to the form the forms module serves on the site's own
// origin (/_forms/<form>/form). Every field is optional.
type SiteContact struct {
	Title  string `yaml:"title"`
	Text   string `yaml:"text"`
	Form   string `yaml:"form"` // default DefaultContactForm
	Button string `yaml:"button"`
}

// FormID is the form the section links to.
func (c *SiteContact) FormID() string {
	if c.Form == "" {
		return DefaultContactForm
	}
	return c.Form
}

// SiteFooter is the footer content.
type SiteFooter struct {
	Text  string     `yaml:"text"`
	Links []SiteLink `yaml:"links"`
}
