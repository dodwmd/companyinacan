package installer

import (
	"context"
	"fmt"

	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// Configuration a module needs that no chart can hold: facts of company.yaml, and credentials of
// another module. reconcile.companyRevision makes a change of either an upgrade.

// companyValues are the chart value overrides built from company.yaml. jobs does not start without
// company.name and company.base_url; files puts share.base_url in front of the share links it
// returns. The origin is the module's own host (runtime.subdomain), http only when the cluster's
// routing facts say there is no TLS.
func (in *Installer) companyValues(ctx context.Context, s reconcile.Spec) (map[string]any, error) {
	domain, sub := in.domain(s), s.Manifest.Runtime.Subdomain
	if domain == "" || sub == "" || (s.ID != "jobs" && s.ID != "files") {
		return nil, nil
	}
	data, _, err := in.routingData(ctx)
	if err != nil {
		return nil, err
	}
	scheme := "https"
	if data["tls"] == "false" {
		scheme = "http"
	}
	origin := scheme + "://" + sub + "." + domain
	var conf map[string]any
	switch {
	case s.ID == "jobs":
		company := map[string]any{"base_url": origin}
		if s.CompanyName != "" {
			company["name"] = s.CompanyName
		}
		conf = map[string]any{"company": company}
	case s.Expose.PublicOn(): // files, and only while its links are reachable from outside
		conf = map[string]any{"share": map[string]any{"base_url": origin}}
	default:
		return nil, nil
	}
	return map[string]any{"config": conf}, nil
}

// wireSecrets copies into a module's Secret the credentials it needs of another module: the
// provisioning module reads the people directory with a token the people module accepts. A value
// that is already there is kept when the people module's Secret cannot supply one (a restore).
func (in *Installer) wireSecrets(ctx context.Context, s reconcile.Spec, data map[string]string) error {
	if s.ID != "provisioning" {
		return nil
	}
	tok, err := in.secretKey(ctx, "mod-people", secretModule, modsecrets.PeopleDirectoryKey)
	if err != nil {
		return err
	}
	if tok == "" {
		if tok, err = in.secretKey(ctx, s.Namespace, secretModule, modsecrets.ProvisioningDirectoryKey); err != nil {
			return err
		}
	}
	if tok == "" {
		return fmt.Errorf("the provisioning module reads the people directory: enable the people module (it is installed first, and creates %s)", modsecrets.PeopleDirectoryKey)
	}
	data[modsecrets.ProvisioningDirectoryKey] = tok
	return nil
}

// WiringFingerprint implements reconcile.WiringAware: a digest of the people directory token as the
// provisioning module was or would be given it, "" while there is none.
func (in *Installer) WiringFingerprint(ctx context.Context, s reconcile.Spec) (string, error) {
	if s.ID != "provisioning" {
		return "", nil
	}
	tok, err := in.secretKey(ctx, "mod-people", secretModule, modsecrets.PeopleDirectoryKey)
	if err != nil || tok == "" {
		return "", err
	}
	return secretVersion(s.HMACSecret, map[string]string{modsecrets.PeopleDirectoryKey: tok}), nil
}
