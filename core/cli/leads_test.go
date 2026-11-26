package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/forms"
)

// The command against the real forms API.
func TestLeadsFindAndErase(t *testing.T) {
	cfg, err := forms.ParseConfig([]byte("forms:\n  - {id: contact, name: Contact, min_fill_seconds: 0, fields: [{name: email, type: email, required: true}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = t.TempDir() + "/forms.db"
	const token = "admin-token-0123456789"
	app, err := forms.NewApp(cfg, forms.Secrets{TokenKey: []byte("0123456789abcdef0123456789abcdef"), APIToken: token},
		forms.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Close(ctx)
	})
	api := httptest.NewServer(app.APIHandler())
	defer api.Close()
	pub := httptest.NewServer(app.PublicHandler())
	defer pub.Close()
	t.Setenv("CAN_FORMS_URL", api.URL)
	t.Setenv("CAN_FORMS_TOKEN", token)

	cli := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Execute(append([]string{"leads"}, args...), strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}
	tok, err := pub.Client().Get(pub.URL + "/_forms/contact/token")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(tok.Body)
	_ = tok.Body.Close()
	_, rest, _ := strings.Cut(string(body), `"token":"`)
	timing, _, _ := strings.Cut(rest, `"`)
	resp, err := pub.Client().Post(pub.URL+"/_forms/contact", "application/x-www-form-urlencoded", strings.NewReader("email=ann%40example.com&_t="+timing))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	code, out, errs := cli("ann@example.com\n", "find")
	id, _, _ := strings.Cut(out, " ")
	if code != ExitOK || !cliLeadID.MatchString(id) || !strings.Contains(out, "address") {
		t.Fatalf("find: %d %q %q (submit: %d)", code, out, errs, resp.StatusCode)
	}
	if code, _, errs := cli("", "erase", id); code != ExitUsage || !strings.Contains(errs, "--yes") {
		t.Errorf("without --yes: %d %q", code, errs)
	}
	code, out, errs = cli("", "erase", id, "--yes")
	if code != ExitOK || !strings.Contains(out, id+"  erased") || !strings.Contains(errs, "ledger-add --subject "+id+" --class lead-submission") {
		t.Fatalf("erase: %d %q %q", code, out, errs)
	}
	if code, out, _ := cli("", "erase", id, "--yes"); code != ExitOK || !strings.Contains(out, "already erased") {
		t.Errorf("repeat: %d %q", code, out)
	}
	if code, _, errs := cli("", "erase", "ld_000000000000000000000000", "--yes"); code != ExitFailure || !strings.Contains(errs, "404") || strings.Contains(errs, "ledger-add") {
		t.Errorf("unknown: %d %q", code, errs)
	}
	if code, _, _ := cli("", "erase", "../x", "--yes"); code != ExitUsage {
		t.Errorf("bad id: %d", code)
	}
	if code, out, _ := cli("ann@example.com\n", "find"); code != ExitOK || out != "" {
		t.Errorf("after: %d %q", code, out)
	}
	// No answer, or a failure after the delete: the lead may be gone, so the command is shown.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"code":"scrub_failed","message":"erased, not scrubbed"}`)
	}))
	defer broken.Close()
	t.Setenv("CAN_FORMS_URL", broken.URL)
	if code, _, errs := cli("", "erase", id, "--yes"); code != ExitFailure || !strings.Contains(errs, "ledger-add --subject "+id) {
		t.Errorf("scrub failed: %d %q", code, errs)
	}
	t.Setenv("CAN_FORMS_TOKEN", "")
	if code, _, _ := cli("a@b.example\n", "find"); code != ExitUsage {
		t.Errorf("no token: %d", code)
	}
}
