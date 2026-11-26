package adminui

// Strings of the module page's public access section and the DNS note (specs/045-public-access).
func init() {
	for k, v := range map[string]string{
		"exp.title":        "Internet access",
		"exp.what":         "What",
		"exp.state":        "State",
		"exp.url":          "Address",
		"exp.ui":           "Web UI",
		"exp.api":          "API",
		"exp.pages":        "Public pages",
		"exp.invoice_link": "Invoice links",
		"exp.signing":      "Contract signing pages",
		"exp.forms":        "Form intake",
		"exp.on":           "On",
		"exp.off":          "Off",
		"exp.not_routed":   "On, not routed yet",
		"exp.none":         "This module has nothing that can be reached from the internet.",
		"exp.disabled":     "The module is not enabled in company.yaml, so nothing of it is public.",
		"exp.no_domain":    "company.yaml has no domain, so there are no addresses.",
		"exp.unavailable":  "company.yaml cannot be read right now.",
		"exp.how":          "Change this in company.yaml (modules, then the module, then expose) and apply it. Each address needs a DNS record: can up and can status list them.",
		"wz.dns_modules":   "Module addresses are listed once company.yaml puts them on the internet. With dns.provider: cloudflare, can up creates these records; otherwise add them yourself.",
	} {
		messagesEN[k] = v
	}
}
