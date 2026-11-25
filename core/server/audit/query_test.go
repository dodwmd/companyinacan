package audit

import (
	"context"
	"testing"
)

func TestQueryFiltersNewestFirstAndPages(t *testing.T) {
	l, _ := newLog(t)
	ctx := context.Background()
	for _, e := range []Entry{
		{Actor: "root", Action: "admin.login", Target: "admin"},
		{Actor: "root", Action: "delivery.requeue", Target: "7"},
		{Actor: "cli", Action: "admin.bootstrap", Target: "admin"},
		{Actor: "root", Action: "admin.login", Target: "admin"},
	} {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	all, err := l.Query(ctx, Filter{Limit: 10})
	if err != nil || len(all) != 4 || all[0].Seq != 4 || all[3].Seq != 1 {
		t.Fatalf("%v %+v", err, all)
	}
	got, _ := l.Query(ctx, Filter{Actor: "root", ActionPrefix: "admin.", Limit: 10})
	if len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("%+v", got)
	}
	got, _ = l.Query(ctx, Filter{Target: "7", Limit: 10})
	if len(got) != 1 || got[0].Action != "delivery.requeue" {
		t.Fatalf("%+v", got)
	}
	got, _ = l.Query(ctx, Filter{Before: 3, Limit: 1})
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("paging: %+v", got)
	}
	// Non-ASCII prefixes are counted in characters, not bytes.
	if _, err := l.Append(ctx, Entry{Actor: "root", Action: "módulo.enable", Target: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = l.Query(ctx, Filter{ActionPrefix: "módulo.", Limit: 10}); len(got) != 1 {
		t.Fatalf("multi-byte prefix: %+v", got)
	}
	// LIKE wildcards in the filter are literal.
	got, _ = l.Query(ctx, Filter{ActionPrefix: "%", Limit: 10})
	if len(got) != 0 {
		t.Fatalf("wildcard matched: %+v", got)
	}
}
