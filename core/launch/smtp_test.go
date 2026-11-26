package launch

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

type recordingKube struct {
	args  [][]string
	stdin []string
}

func (k *recordingKube) Run(_ context.Context, stdin []byte, args ...string) (string, error) {
	k.args, k.stdin = append(k.args, args), append(k.stdin, string(stdin))
	return "", nil
}

func TestRelaySecretIsBoundToTheHostAndNeverInArguments(t *testing.T) {
	co := &config.Company{Email: &config.Email{Relay: &config.EmailRelay{Host: "smtp.relay.example", Port: 465, From: "hello@acme.example"}}}
	s, err := SMTPFrom(co, "apikey", "p4ss-relay-secret")
	if err != nil {
		t.Fatal(err)
	}
	k := &recordingKube{}
	r := &runner{o: Options{SMTP: s}, d: Deps{Kube: k}}
	if err := r.relaySecret(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(k.args) != 1 || strings.Contains(strings.Join(k.args[0], " "), "p4ss") || !strings.Contains(strings.Join(k.args[0], " "), "--server-side") {
		t.Fatalf("%v", k.args)
	}
	for _, want := range []string{"name: can-smtp-relay", "namespace: can-system", "host: smtp.relay.example", "password: p4ss-relay-secret", `port: "465"`} {
		if !strings.Contains(k.stdin[0], want) {
			t.Errorf("manifest lacks %q:\n%s", want, k.stdin[0])
		}
	}

	none := &runner{d: Deps{Kube: k}}
	if err := none.relaySecret(context.Background()); err != nil || len(k.args) != 1 {
		t.Fatal("without credentials the Secret must be left alone")
	}
	for name, c := range map[string]*config.Company{
		"no relay host": {},
		"no tls":        {Email: &config.Email{Relay: &config.EmailRelay{Host: "h.example", TLS: "none"}}},
	} {
		if _, err := SMTPFrom(c, "u", "p"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := SMTPFrom(co, "u", ""); err == nil {
		t.Error("a missing password was accepted")
	}
}
