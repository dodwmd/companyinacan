package files

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type slowScanner struct{ d time.Duration }

func (s slowScanner) Scan(_ context.Context, r io.Reader) (ScanVerdict, error) {
	_, _ = io.Copy(io.Discard, r)
	time.Sleep(s.d)
	return ScanClean, nil
}

// QA major 4: the server's WriteTimeout (30 s) ran while the upload was being received, scanned and
// stored: the file was kept but the client saw a failure and uploaded it again.
func TestASlowUploadStillGetsItsAnswer(t *testing.T) {
	e := newEnv(t, func(_ *Config, o *StoreOptions) { o.Scanner = slowScanner{700 * time.Millisecond} })
	api := &API{Store: e.s, Auth: StaticToken{Token: apiToken}}
	srv := httptest.NewUnstartedServer(api.Handler())
	srv.Config.WriteTimeout = 200 * time.Millisecond // what the real server sets, only shorter
	srv.Start()
	defer srv.Close()
	svc := &Principal{Subject: "svc:static", Perms: []string{PermAdmin}}
	sp := e.space(svc, "S")
	f := e.upload(svc, sp.ID, "a.pdf", pdf("x"))
	for _, path := range []string{"/v1/spaces/" + sp.ID + "/files?name=b.pdf", "/v1/files/" + f.ID + "/versions"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(pdf("y")))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: the client got no answer: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}
