package config

import (
	"net"
	"testing"
)

func TestPublicIPDeniesPrivateRanges(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fc00::1", "::ffff:10.0.0.1", "224.0.0.1"} {
		if PublicIP(net.ParseIP(s)) {
			t.Errorf("%s must not be public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !PublicIP(net.ParseIP(s)) {
			t.Errorf("%s must be public", s)
		}
	}
}

func TestCheckHTTPSURL(t *testing.T) {
	bad := []string{"", "http://idp.example.org", "https://user:pw@idp.example.org", "https://localhost", "https://10.0.0.1/", "https://[::1]/", "/relative", "https://x.internal"}
	for _, u := range bad {
		if err := CheckHTTPSURL(u, false, false); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if err := CheckHTTPSURL("http://127.0.0.1:9000", true, false); err != nil {
		t.Errorf("insecure dev refused: %v", err)
	}
	if err := CheckHTTPSURL("https://admin.acme.example/path", false, true); err == nil {
		t.Error("path accepted for origin-only")
	}
}

func TestValidateSSORules(t *testing.T) {
	good := &SSO{Enabled: true, Issuer: "https://idp.example.org", ClientID: "c", ClientSecretRef: "acme-sso", GroupRoles: map[string][]string{"a": {"operator", "finance:approver"}}}
	if is := ValidateSSO(good, "acme.example"); len(is) != 0 {
		t.Fatalf("%v", is)
	}
	if is := ValidateSSO(good, ""); len(is) != 1 {
		t.Fatalf("no domain and no redirect base must be refused: %v", is)
	}
	cases := map[string]func(*SSO){
		"owner needs confirm": func(s *SSO) { s.GroupRoles["x"] = []string{"owner"} },
		"unqualified role":    func(s *SSO) { s.GroupRoles["x"] = []string{"viewer"} },
		"uppercase role":      func(s *SSO) { s.GroupRoles["x"] = []string{"Operator"} },
		"empty group":         func(s *SSO) { s.GroupRoles[""] = []string{"auditor"} },
		"revalidate too low":  func(s *SSO) { s.RevalidateInterval = "1m" },
		"revalidate junk":     func(s *SSO) { s.RevalidateInterval = "soon" },
		"bad local login":     func(s *SSO) { s.LocalLogin = "maybe" },
		"secret named can-":   func(s *SSO) { s.ClientSecretRef = "can-secret" },
		"redirect with path":  func(s *SSO) { s.RedirectBaseURL = "https://admin.acme.example/x" },
		"bad email domain":    func(s *SSO) { s.AllowedEmailDomains = []string{"*.acme.example"} },
		"bad claim":           func(s *SSO) { s.Claims.Groups = "a b" },
	}
	for name, mut := range cases {
		s := *good
		s.GroupRoles = map[string][]string{"a": {"operator"}}
		mut(&s)
		if len(ValidateSSO(&s, "acme.example")) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if ValidateSSO(&SSO{Enabled: false}, "") != nil {
		t.Error("a disabled block is not validated")
	}
}

func TestNormalisedDefaults(t *testing.T) {
	n := SSO{}.Normalised("acme.example")
	if n.RedirectBaseURL != "https://admin.acme.example" || n.Claims.Groups != "groups" || !n.EmailVerifiedRequired() || n.LocalLogin != LocalLoginAll || n.Scopes[0] != "openid" {
		t.Fatalf("%+v", n)
	}
	if got := (SSO{Scopes: []string{"email"}}).Normalised("a.example").Scopes; got[0] != "openid" {
		t.Fatalf("openid not forced: %v", got)
	}
}

func TestOpenAccessNeedsConfirmation(t *testing.T) {
	s := &SSO{Enabled: true, Issuer: "https://idp.example.org", ClientID: "c", ClientSecretRef: "acme-sso"}
	if len(ValidateSSO(s, "acme.example")) != 1 {
		t.Fatalf("%v", ValidateSSO(s, "acme.example"))
	}
	s.AllowAnyAccount = true
	if len(ValidateSSO(s, "acme.example")) != 0 {
		t.Fatal("confirmed")
	}
	for _, f := range []func(*SSO){func(s *SSO) { s.AllowedEmailDomains = []string{"acme.example"} }, func(s *SSO) { s.RequireMappedRole = true }, func(s *SSO) { s.GroupRoles = map[string][]string{"g": {"auditor"}} }} {
		s := &SSO{Enabled: true, Issuer: "https://idp.example.org", ClientID: "c", ClientSecretRef: "acme-sso"}
		f(s)
		if is := ValidateSSO(s, "acme.example"); len(is) != 0 {
			t.Fatalf("%v", is)
		}
	}
}

func TestPublicIPDeniesTransitionForms(t *testing.T) {
	for _, s := range []string{"2002:c0a8:0101::1", "2002:0a00:0001::1", "::10.0.0.1", "::127.0.0.1", "2001:0:4136:e378:8000:63bf:3fff:fdd2"} {
		if PublicIP(net.ParseIP(s)) {
			t.Errorf("%s must not be public", s)
		}
	}
}
