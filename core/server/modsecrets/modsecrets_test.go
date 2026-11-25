package modsecrets

import (
	"strings"
	"testing"
)

func TestEveryInHouseModuleHasItsRequiredSecrets(t *testing.T) {
	want := map[string][]string{
		"work":    {"CAN_WORK_ADMIN_TOKEN"},
		"forms":   {"CAN_FORMS_TOKEN_KEY", "CAN_FORMS_API_TOKEN", "CAN_FORMS_API_READ_TOKEN"},
		"finance": {"CAN_FINANCE_ADMIN_TOKEN", "CAN_FINANCE_LINK_SECRET"},
		"jobs":    {"CAN_JOBS_ADMIN_TOKEN", "CAN_JOBS_HASH_KEY", "CAN_JOBS_FORM_SECRET", KeyringKey},
		"people":  {"CAN_PEOPLE_API_TOKEN", KeyringKey},
	}
	for mod, keys := range want {
		got := map[string]bool{}
		for _, s := range For(mod) {
			got[s.Key] = true
		}
		for _, k := range keys {
			if !got[k] {
				t.Errorf("%s: no generated secret %s", mod, k)
			}
		}
	}
	if len(For("support")) != 0 || len(For("")) != 0 {
		t.Error("a module that needs no secrets must get none")
	}
}

func TestGeneratedValuesMeetTheModulesOwnMinimums(t *testing.T) {
	for _, mod := range []string{"work", "forms", "finance", "jobs", "people"} {
		for _, s := range For(mod) {
			v := Generate(s.Kind)
			if s.Kind == Keyring {
				continue
			}
			distinct := map[rune]bool{}
			for _, r := range v {
				distinct[r] = true
			}
			if len(v) < 32 || len(distinct) < 10 {
				t.Errorf("%s %s: %d chars %d distinct", mod, s.Key, len(v), len(distinct))
			}
			if v == Generate(s.Kind) {
				t.Errorf("%s %s: two generated values are equal", mod, s.Key)
			}
		}
	}
}

func TestKeyringFirstValueAndRotationKeepOldKeys(t *testing.T) {
	first := Generate(Keyring)
	lines := strings.Split(strings.TrimSpace(first), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "k1 ") || len(strings.TrimPrefix(lines[0], "k1 ")) != 64 {
		t.Fatalf("%q", first)
	}
	next, err := RotateKeyring(first)
	if err != nil {
		t.Fatal(err)
	}
	nl := strings.Split(strings.TrimSpace(next), "\n")
	if len(nl) != 2 || !strings.HasPrefix(nl[0], "k2 ") || nl[1] != lines[0] {
		t.Fatalf("the newest key must come first and the old one must stay readable:\n%s", next)
	}
	again, _ := RotateKeyring(next)
	if !strings.HasPrefix(again, "k3 ") {
		t.Fatalf("%s", again)
	}
	if _, err := RotateKeyring("garbage"); err == nil {
		t.Fatal("rotating a keyring that does not parse must fail, not replace it")
	}
	if _, err := RotateKeyring(""); err == nil {
		t.Fatal("rotating an absent keyring must fail")
	}
}

func TestRotationPolicy(t *testing.T) {
	if Rotatable("jobs", "CAN_JOBS_HASH_KEY") {
		t.Error("rotating the dedupe hash key would silently break duplicate detection; it must be refused")
	}
	for _, c := range [][2]string{{"jobs", "CAN_JOBS_ADMIN_TOKEN"}, {"people", KeyringKey}, {"work", "CAN_WORK_ADMIN_TOKEN"}, {"work", WebhookKey}} {
		if !Rotatable(c[0], c[1]) {
			t.Errorf("%v must be rotatable", c)
		}
	}
	if Rotatable("work", "CAN_NOT_A_SECRET") || Rotatable("nope", "CAN_WORK_ADMIN_TOKEN") {
		t.Error("an unknown key must not be rotatable")
	}
}
