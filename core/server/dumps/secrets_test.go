package dumps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

type forbidding struct {
	kube.API
	ns string
}

func (f forbidding) List(ctx context.Context, kind, ns, sel string) ([]kube.Object, error) {
	if ns == f.ns {
		return nil, errors.New("forbidden")
	}
	return f.API.List(ctx, kind, ns, sel)
}

func TestExportSecrets(t *testing.T) {
	ctx := context.Background()
	api := kube.NewFake()
	put := func(kind, ns, name, typ string) {
		o := kube.Object{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": name}}
		if ns != "" {
			o["metadata"].(map[string]any)["namespace"] = ns
		}
		if kind == "Secret" {
			o["type"] = typ
			o["stringData"] = map[string]any{"k": "v"}
		}
		if err := api.Apply(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, ns := range []string{"mod-people", "mod-old"} {
		put("Namespace", "", ns, "")
		put("Secret", ns, "can-module", "Opaque")
	}
	put("Secret", "mod-people", "can-release", "Opaque")
	put("Secret", "mod-people", "can-smtp", "Opaque")
	put("Secret", "mod-people", "tok", serviceAccountType)
	put("Secret", "can-system", "can-smtp-relay", "Opaque")
	put("Secret", "can-system", "something-else", "Opaque")
	mods := []ModuleRef{{"people", true}, {"old", false}, {"never", true}}
	e, warn, err := ExportSecrets(ctx, api, mods)
	if err != nil || len(warn) != 0 {
		t.Fatal(warn, err)
	}
	var got []string
	for _, s := range e.Secrets {
		got = append(got, s.Namespace+"/"+s.Name)
	}
	if strings.Join(got, " ") != "can-system/can-smtp-relay mod-old/can-module mod-people/can-module" {
		t.Fatal(got)
	}
	if e.Secrets[2].Data["k"] != "dg==" {
		t.Error(e.Secrets[2].Data)
	}
	// an enabled module whose Secrets cannot be read fails the run; a disabled one is announced
	if _, _, err := ExportSecrets(ctx, forbidding{api, "mod-people"}, mods); err == nil {
		t.Error("a backup without an enabled module's keys must not look complete")
	}
	_, warn, err = ExportSecrets(ctx, forbidding{api, "mod-old"}, mods)
	if err != nil || len(warn) != 1 || !strings.Contains(warn[0], "NOT in the backup") {
		t.Error(warn, err)
	}
}
