package runtime

import (
	"context"
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Only what the installed module was given is handed over: a form added to company.yaml behind a
// held upgrade is not, and one taken out stops before any upgrade (specs/067-lead-adapters).
func TestInstalledIntake(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	asked := config.LeadIntake{SupportForms: []string{"contact", "quote"}, NewsletterForms: []string{"contact"}, NewsletterList: "other"}

	got, err := installedIntake(ctx, db, asked)
	if err != nil || len(got.SupportForms)+len(got.NewsletterForms) != 0 {
		t.Fatalf("nothing installed: %+v %v", got, err)
	}
	for id, applied := range map[string]string{
		"support":    `{"mapped":true,"values":{"app.ticketsFromForms":["contact","gone"]}}`,
		"newsletter": `{"mapped":true,"values":{"app.defaultList":"News"}}`,
	} {
		if err := db.UpsertModule(ctx, store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, Applied: applied}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err = installedIntake(ctx, db, asked)
	if err != nil || !slices.Equal(got.SupportForms, []string{"contact"}) || len(got.NewsletterForms) != 0 || got.NewsletterList != "News" {
		t.Fatalf("%+v %v", got, err)
	}
}
