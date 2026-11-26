package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/adminui"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/registry"
)

type leadsProvider string

func (p leadsProvider) Resolve(context.Context, string) (registry.Resolution, error) {
	return registry.Resolution{Providers: []registry.Provider{{ModuleID: string(p)}}}, nil
}

type secretAPI struct {
	kube.API
	token string
}

func (a secretAPI) Get(_ context.Context, kind, ns, name string) (kube.Object, error) {
	if kind != "Secret" || ns != "mod-forms" || name != "can-module" {
		return nil, errors.New("not found")
	}
	return kube.Object{"data": map[string]any{"CAN_FORMS_API_TOKEN": base64.StdEncoding.EncodeToString([]byte(a.token))}}, nil
}

func TestLeadsTokenComesFromTheFormsSecret(t *testing.T) {
	api := secretAPI{token: "a-generated-token"}
	if tok, err := formsToken(leadsProvider("forms"), api)(); err != nil || tok != "a-generated-token" {
		t.Fatalf("%q %v", tok, err)
	}
	// Another provider of `leads` is never given the forms module's token.
	if tok, err := formsToken(leadsProvider("other"), api)(); !errors.Is(err, adminui.ErrLeadsUnavailable) || tok != "" {
		t.Fatalf("%q %v", tok, err)
	}
}
