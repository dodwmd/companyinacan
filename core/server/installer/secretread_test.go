package installer

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

// flaky fails Get of Secrets on demand and counts every write.
type flaky struct {
	kube.API
	failGet error
	writes  int
}

func (f *flaky) Get(ctx context.Context, kind, ns, name string) (kube.Object, error) {
	if f.failGet != nil && kind == "Secret" {
		return nil, f.failGet
	}
	return f.API.Get(ctx, kind, ns, name)
}

func (f *flaky) Apply(ctx context.Context, o kube.Object) error {
	if o["kind"] == "Secret" {
		f.writes++
	}
	return f.API.Apply(ctx, o)
}

// A failed read of the module Secret must never be taken for "no Secret": regenerating would make
// the sealed data undecryptable and invalidate every issued credential.
func TestAFailedSecretReadNeverRegeneratesSecrets(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.SecretsFor = func(string) []modsecrets.Spec {
		return []modsecrets.Spec{{Key: "CAN_STUB_TOKEN"}, {Key: modsecrets.KeyringKey, Kind: modsecrets.Keyring}}
	}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	before := secretData(get(t, api, "Secret", "mod-stub", "can-module"))
	for name, ferr := range map[string]error{
		"500":     &kube.StatusError{Code: 500, Reason: "InternalError", Message: "boom"},
		"timeout": context.DeadlineExceeded,
		"403":     &kube.StatusError{Code: 403, Reason: "Forbidden", Message: "no"},
	} {
		f := &flaky{API: api, failGet: ferr}
		in.cfg.API = f
		if err := in.Enable(context.Background(), spec); err == nil {
			t.Fatalf("%s: enable succeeded although the Secret could not be read", name)
		}
		if f.writes != 0 {
			t.Fatalf("%s: %d Secret writes happened after a failed read", name, f.writes)
		}
	}
	in.cfg.API = api
	after := secretData(get(t, api, "Secret", "mod-stub", "can-module"))
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed", k)
		}
	}
	// a Secret that is genuinely missing is created
	delete(api.Objects, "Secret/mod-stub/can-module")
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if secretData(get(t, api, "Secret", "mod-stub", "can-module"))["CAN_STUB_TOKEN"] == "" {
		t.Fatal("a missing Secret was not created")
	}
}
