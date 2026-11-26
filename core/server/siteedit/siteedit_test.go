package siteedit

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/site"
)

const doc = `# head comment
schema: 1
name: Acme
domain: acme.example
provider: {type: local}
site:
  # about the tagline
  tagline: Old
  noindex: false
  hero:
    headline: Hello # inline
    cta: { label: Go, href: "#features" }
  features:
    - title: One # first
    - title: Two # second
email:
  relay: {host: smtp.example, from: a@acme.example}
`

func parse(t *testing.T, b []byte) *config.Company {
	t.Helper()
	c, err := modmgr.ParseCompany(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMergeKeepsWhatDidNotChange(t *testing.T) {
	c := parse(t, []byte(doc))
	same, err := Merge([]byte(doc), c.Site)
	if err != nil {
		t.Fatal(err)
	}
	if string(same) != doc {
		t.Errorf("an unchanged section rewrote the document:\n%s", same)
	}
	s := c.Site
	s.Tagline = "New\nlines: yes"
	s.Features = []config.SiteFeature{s.Features[1], {Title: "yes"}}
	s.Hero = &config.SiteHero{Headline: "Hello", CTA: nil}
	s.Contact = &config.SiteContact{}
	s.NoIndex = false
	out, err := Merge([]byte(doc), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# head comment", "# about the tagline", "# inline", "# second", "noindex: false", "contact: {}", "relay: {host: smtp.example, from: a@acme.example}", `"yes"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "label: Go") || strings.Contains(string(out), "# first") {
		t.Errorf("what was removed is still there:\n%s", out)
	}
	if got := parse(t, out).Site; site.SectionHash(got) != site.SectionHash(s) {
		t.Errorf("round trip: %#v, want %#v", got, s)
	}

	// A document without the section gets one; an empty section adds nothing.
	bare := []byte("schema: 1\nname: A\nprovider: {type: local}\n")
	if out, err := Merge(bare, config.Site{}); err != nil || string(out) != string(bare) {
		t.Errorf("%v %s", err, out)
	}
	if out, err := Merge(bare, config.Site{Tagline: "t"}); err != nil || parse(t, out).Site.Tagline != "t" {
		t.Errorf("%v %s", err, out)
	}
	for _, bad := range []string{"- a\n", "a: &x 1\nb: *x\n", ":\n:"} {
		if _, err := Merge([]byte(bad), config.Site{Tagline: "t"}); !errors.Is(err, ErrDocument) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// cluster is a kube API holding what `can up` leaves behind.
func cluster(t *testing.T, annotated bool) *kube.Fake {
	t.Helper()
	c := parse(t, []byte(doc))
	out, err := site.Render(c, site.Options{BaseURL: "https://203-0-113-7.sslip.io"})
	if err != nil {
		t.Fatal(err)
	}
	tgz, _, err := out.Archive()
	if err != nil {
		t.Fatal(err)
	}
	k := kube.NewFake()
	md := map[string]any{"name": "site-content", "namespace": "can-system", "resourceVersion": "41"}
	if annotated {
		md["annotations"] = map[string]any{site.AnnotationBaseURL: "https://203-0-113-7.sslip.io", site.AnnotationSource: site.SectionHash(c.Site)}
	}
	k.Objects["ConfigMap/can-system/site-content"] = kube.Object{"metadata": md, "binaryData": map[string]any{"site.tar.gz": base64.StdEncoding.EncodeToString(tgz)}}
	k.Objects["Deployment/can-system/site"] = kube.Object{"metadata": map[string]any{"name": "site"}, "spec": map[string]any{"replicas": 1.0, "template": map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{site.AnnotationSHA: "none"}}, "spec": map[string]any{"containers": []any{}}}}}
	for _, name := range AdmissionPolicies {
		k.Objects["ValidatingAdmissionPolicy//"+name] = kube.Object{}
		k.Objects["ValidatingAdmissionPolicyBinding//"+name] = kube.Object{"spec": map[string]any{"validationActions": []any{"Deny"}}}
	}
	return k
}

func service(t *testing.T, p Publisher) (*Service, *modmgr.FileStore) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	path := t.TempDir() + "/company.yaml"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := &modmgr.FileStore{Path: path}
	return &Service{DB: db, Audit: audit.New(db), Company: fs, Publisher: p, ModulesDir: "../../../modules"}, fs
}

func TestKubePublisherWritesOnlyContentAndDigest(t *testing.T) {
	ctx := context.Background()
	k := cluster(t, true)
	s, _ := service(t, KubePublisher{API: k})
	st, err := s.State(ctx)
	if err != nil || st.Live != LiveCurrent {
		t.Fatalf("%v %q", err, st.Live)
	}
	next := st.Company.Site
	next.Tagline = "From the cluster"
	d, err := s.CreateDraft(ctx, "ops", st.Hash, next, KindPublish)
	if err != nil {
		t.Fatal(err)
	}
	k.Log = nil
	if _, err := s.Publish(ctx, "ops", d.ID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(k.Log, ","); got != "patch ConfigMap can-system/site-content,patch Deployment can-system/site" {
		t.Errorf("wrote %s", got)
	}
	p, err := KubePublisher{API: k}.Current(ctx)
	if err != nil || p.BaseURL != "https://203-0-113-7.sslip.io" || p.SourceSHA256 == st.Hash || p.SHA256 == "" {
		t.Fatalf("%v %+v", err, p)
	}
	dep := k.Objects["Deployment/can-system/site"]
	ann := asMap(asMap(asMap(asMap(dep["spec"])["template"])["metadata"])["annotations"])
	if ann[site.AnnotationSHA] != p.SHA256 || asMap(dep["spec"])["replicas"] != 1.0 {
		t.Errorf("deployment: %v", dep)
	}
	if st, _ := s.State(ctx); st.Live != LiveCurrent {
		t.Errorf("live %q", st.Live)
	}
}

// Without the admission policy that bounds it, can-core does not publish and changes nothing.
func TestKubePublisherNeedsItsAdmissionPolicy(t *testing.T) {
	ctx := context.Background()
	for _, missing := range []string{"ValidatingAdmissionPolicy//can-core-site", "ValidatingAdmissionPolicyBinding//can-core-site-content", "Deployment/can-system/site"} {
		k := cluster(t, false)
		delete(k.Objects, missing)
		s, fs := service(t, KubePublisher{API: k})
		st, err := s.State(ctx)
		if err != nil || st.Live != LiveUnknown {
			t.Fatalf("%v %q", err, st.Live)
		}
		next := st.Company.Site
		next.Tagline = "x"
		d, err := s.CreateDraft(ctx, "ops", st.Hash, next, KindPublish)
		if err != nil {
			t.Fatal(err)
		}
		k.Log = nil
		if _, err := s.Publish(ctx, "ops", d.ID); !errors.Is(err, ErrNotReady) {
			t.Errorf("%s: %v", missing, err)
		}
		if snap, _ := fs.Get(ctx); string(snap.Data) != doc || len(k.Log) != 0 {
			t.Errorf("%s: changed something: %v", missing, k.Log)
		}
	}
}

func TestPublishWithoutTheLogoFileSaysSo(t *testing.T) {
	ctx := context.Background()
	k := cluster(t, true)
	s, fs := service(t, KubePublisher{API: k})
	withLogo := strings.Replace(doc, "  tagline: Old", "  brand: {logo: logo.png}\n  tagline: Old", 1)
	snap, _ := fs.Get(ctx)
	if _, err := fs.Put(ctx, []byte(withLogo), snap.Version); err != nil {
		t.Fatal(err)
	}
	st, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateDraft(ctx, "ops", st.Hash, st.Company.Site, KindPublish)
	var inv *InvalidError
	if !errors.As(err, &inv) || !strings.Contains(err.Error(), "`can up`") {
		t.Fatalf("%v", err)
	}
}

func TestNoSiteYet(t *testing.T) {
	ctx := context.Background()
	s, _ := service(t, DirPublisher{Dir: t.TempDir()})
	st, err := s.State(ctx)
	if err != nil || st.Live != LiveNone {
		t.Fatalf("%v %q", err, st.Live)
	}
	d, err := s.CreateDraft(ctx, "ops", st.Hash, st.Company.Site, KindPublish)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "ops", d.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("%v", err)
	}
}

func TestDraftsExpire(t *testing.T) {
	ctx := context.Background()
	s, _ := service(t, KubePublisher{API: cluster(t, true)})
	st, _ := s.State(ctx)
	d, err := s.CreateDraft(ctx, "ops", st.Hash, st.Company.Site, KindPublish)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return d.CreatedAt.Add(DraftTTL + time.Minute) }
	if _, err := s.Publish(ctx, "ops", d.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("%v", err)
	}
}
