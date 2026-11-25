package jobs

import (
	"context"
	"errors"
	"testing"
)

func TestFakeSyndicatorRecordsLifecycle(t *testing.T) {
	f := NewFakeSyndicator("fake-board")
	ctx := context.Background()
	j := fixtureJobs()[0]
	ref, err := f.Publish(ctx, testSite(), j)
	if err != nil || ref == "" {
		t.Fatalf("publish: %q %v", ref, err)
	}
	if err := f.Update(ctx, testSite(), ref, j); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls(); len(got) != 3 || got[0] != "publish:"+j.ID || got[2] != "close:"+string(ref) {
		t.Errorf("calls = %v", got)
	}
	if f.Caps().Status != BoardConnected {
		t.Errorf("fake status = %v", f.Caps().Status)
	}
}

func TestPartnerStubNeedsAccess(t *testing.T) {
	for _, name := range []string{"linkedin", "indeed", "glassdoor"} {
		s := NewPartnerStub(name)
		if s.Caps().Status != BoardNeedsPartner {
			t.Errorf("%s status = %v", name, s.Caps().Status)
		}
		if _, err := s.Publish(context.Background(), testSite(), fixtureJobs()[0]); !errors.Is(err, ErrPartnerAccess) {
			t.Errorf("%s publish = %v, want ErrPartnerAccess (no network, no pretending)", name, err)
		}
	}
}
