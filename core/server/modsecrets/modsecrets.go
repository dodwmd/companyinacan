// Package modsecrets lists the secrets can-core generates for the in-house modules and how they are
// generated and rotated (specs/016-module-wiring). Values are created once, kept in the module's
// `can-module` Secret and never written anywhere else; nothing in this package logs a value.
package modsecrets

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind is how a value is generated.
type Kind int

// Kinds.
const (
	// Token is 32 random bytes as 64 hex characters: at least 32 characters with 16 distinct, which
	// meets every module's own minimum.
	Token Kind = iota
	// Keyring is a field-encryption keyring: lines of `<key-id> <64 hex>`, newest first.
	Keyring
	// Password is 24 random bytes as 48 hex characters, for a login in a wrapped app: GoatCounter
	// refuses passwords over 50 bytes.
	Password
)

// KeyringKey is the key name of a sealed-field keyring in the module Secret.
const KeyringKey = "keyring"

// Keys of the credentials can-core copies from the people module's Secret into a consumer's.
const (
	// PeopleDirectoryKey (people.read and people.write) goes to provisioning as ProvisioningDirectoryKey.
	PeopleDirectoryKey       = "CAN_PEOPLE_DIRECTORY_TOKEN"
	ProvisioningDirectoryKey = "CAN_PROVISIONING_DIRECTORY_TOKEN"
	// PeopleLookupKey (read-only: who has this account, does this person exist) goes to equity as
	// EquityDirectoryKey.
	PeopleLookupKey    = "CAN_PEOPLE_LOOKUP_TOKEN"
	EquityDirectoryKey = "CAN_EQUITY_DIRECTORY_TOKEN"
)

// Grant is a module-to-module connection can-core is willing to make
// (specs/056-module-capability-wiring). The list is here, not in the manifests: handing a module the
// credential of another one is can-core's decision. A manifest only asks, by naming the capability
// under requires.capabilities or consumes.
type Grant struct {
	Capability, Consumer, Provider string
	// ProviderKey in the provider's Secret is copied to ConsumerKey in the consumer's.
	ProviderKey, ConsumerKey string
	// ConfigKey is the block of the consumer's chart `config:` that takes `url` and
	// `allow_insecure_http`.
	ConfigKey string
	// Gains and Loses say what the connection and its removal change, for the plan an operator reads.
	Gains, Loses string
}

var grants = []Grant{
	{Capability: "directory", Consumer: "equity", Provider: "people", ProviderKey: PeopleLookupKey, ConsumerKey: EquityDirectoryKey, ConfigKey: "directory",
		Gains: "equity verifies holders (directory mode): acting needs a linked directory record, holders need verify-person",
		Loses: "equity returns to attestation mode, which checks less"},
	{Capability: "directory", Consumer: "provisioning", Provider: "people", ProviderKey: PeopleDirectoryKey, ConsumerKey: ProvisioningDirectoryKey, ConfigKey: "directory",
		Gains: "reads and updates the directory",
		Loses: "it cannot work without it"},
}

// Grants returns the connections can-core makes.
func Grants() []Grant { return append([]Grant(nil), grants...) }

// GrantFor returns the grant of consumer to provider.
func GrantFor(consumer, provider string) (Grant, bool) {
	for _, g := range grants {
		if g.Consumer == consumer && g.Provider == provider {
			return g, true
		}
	}
	return Grant{}, false
}

// WebhookKey is the module Secret key holding the HMAC secret that signs can-core's webhooks.
const WebhookKey = "CAN_WEBHOOK_SECRET"

// Spec is one generated secret.
type Spec struct {
	Key  string
	Kind Kind
	// NoRotate marks a value whose replacement would silently damage data (see Rotatable).
	NoRotate bool
}

var table = map[string][]Spec{
	"work":  {{Key: "CAN_WORK_ADMIN_TOKEN"}},
	"forms": {{Key: "CAN_FORMS_TOKEN_KEY"}, {Key: "CAN_FORMS_API_TOKEN"}, {Key: "CAN_FORMS_API_READ_TOKEN"}},
	// The seal key encrypts bank account numbers (E15c); a new one could not open what the old one sealed, so it is
	// generated once and never rotated.
	"finance": {{Key: "CAN_FINANCE_ADMIN_TOKEN"}, {Key: "CAN_FINANCE_LINK_SECRET"}, {Key: "CAN_FINANCE_SEAL_KEY", NoRotate: true}},
	// The hash key makes candidate e-mail dedupe work; a new one would split every existing
	// candidate from their duplicates without any error, so it is generated once and never rotated.
	"jobs": {{Key: "CAN_JOBS_ADMIN_TOKEN"}, {Key: "CAN_JOBS_HASH_KEY", NoRotate: true}, {Key: "CAN_JOBS_FORM_SECRET"}, {Key: KeyringKey, Kind: Keyring}},
	// The directory and lookup tokens are smaller credentials that can-core copies to the consumers of the
	// directory (Grants); they are not rotatable through `secrets rotate`.
	"people": {{Key: "CAN_PEOPLE_API_TOKEN"}, {Key: PeopleDirectoryKey, NoRotate: true}, {Key: PeopleLookupKey, NoRotate: true}, {Key: KeyringKey, Kind: Keyring}},
	// Governance and equity. The board admin token carries erasure, legal hold and the erasure feed. `board` and
	// `entity` are one binary installed as two instances, each with its own values.
	"board":  {{Key: "CAN_BOARD_API_TOKEN"}, {Key: "CAN_BOARD_ADMIN_TOKEN"}, {Key: KeyringKey, Kind: Keyring}},
	"entity": {{Key: "CAN_BOARD_API_TOKEN"}, {Key: "CAN_BOARD_ADMIN_TOKEN"}, {Key: KeyringKey, Kind: Keyring}},
	"equity": {{Key: "CAN_EQUITY_API_TOKEN"}, {Key: KeyringKey, Kind: Keyring}},
	// The service token for the provisioning API (can-core and operators); the SCIM tokens are made through that API.
	"provisioning": {{Key: "CAN_PROVISIONING_API_TOKEN"}},
	// The service token, the restore-only credential of the purge hook, the HMAC secret that signs share links (rotating it ends every live link, which
	// is its own decision, so it is rotatable) and the sealed-field keyring for names and descriptions.
	"files": {{Key: "CAN_FILES_API_TOKEN"}, {Key: "CAN_FILES_RESTORE_TOKEN"}, {Key: "CAN_FILES_LINK_SECRET"}, {Key: KeyringKey, Kind: Keyring}},
	// Wrapped apps (specs/041): the first admin's password, so none of them starts with a default one. The app
	// reads it only when it creates that user and keeps its own hash afterwards, so a new value would change
	// nothing but what the operator reads: never rotated.
	"analytics":  {{Key: "CAN_ANALYTICS_ADMIN_PASSWORD", Kind: Password, NoRotate: true}},
	"newsletter": {{Key: "CAN_NEWSLETTER_ADMIN_PASSWORD", Kind: Password, NoRotate: true}},
	"support":    {{Key: "CAN_SUPPORT_ADMIN_PASSWORD", Kind: Password, NoRotate: true}},
}

// smtpApps lists the wrapped apps that speak SMTP themselves and therefore get the relay credentials
// (docs/email-policy.md). The list is here, not in the manifest: handing out the relay password is
// can-core's decision, and in-house modules send through email.send instead.
var smtpApps = map[string]bool{"support": true}

// WantsRelay reports whether the module is given the SMTP relay in its `can-smtp` Secret.
func WantsRelay(module string) bool { return smtpApps[module] }

// For returns the secrets generated for a module (none for modules that need none).
func For(module string) []Spec { return append([]Spec(nil), table[module]...) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Generate returns a new value of the kind.
func Generate(k Kind) string {
	switch k {
	case Keyring:
		return "k1 " + randHex(32) + "\n"
	case Password:
		return randHex(24)
	}
	return randHex(32)
}

var keyLineRe = regexp.MustCompile(`^k([0-9]+) ([0-9a-f]{64})$`)

// RotateKeyring prepends a fresh key, keeping the old ones so data sealed with them still opens;
// the module's own `rotate-keys` command then re-seals data under the new key. A value that does
// not parse is an error and is never replaced: an unreadable keyring must be looked at, not lost.
func RotateKeyring(current string) (string, error) {
	max := 0
	var lines []string
	for _, l := range strings.Split(current, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		m := keyLineRe.FindStringSubmatch(l)
		if m == nil {
			return "", errors.New("the existing keyring is not in the generated format (k<N> <64 hex>); refusing to replace it")
		}
		n, _ := strconv.Atoi(m[1])
		if n > max {
			max = n
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return "", errors.New("there is no existing keyring to rotate")
	}
	return fmt.Sprintf("k%d %s\n%s\n", max+1, randHex(32), strings.Join(lines, "\n")), nil
}

// Rotatable reports whether key of the module may be rotated: the webhook signing secret and the
// module's generated secrets except those marked NoRotate.
func Rotatable(module, key string) bool {
	if key == WebhookKey {
		return module != ""
	}
	for _, s := range table[module] {
		if s.Key == key {
			return !s.NoRotate
		}
	}
	return false
}
