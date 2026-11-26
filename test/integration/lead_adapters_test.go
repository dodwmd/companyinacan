//go:build integration

package integration

// specs/069-lead-adapters: website leads on a real cluster, through can-core, to the pinned FreeScout
// and Listmonk images, with the in-memory mail server of dev/mailsink. It proves that
//
//   - a lead from the hosted contact form becomes a support conversation, with no automatic reply;
//   - only a sender who ticked the consent checkbox is subscribed, unconfirmed, with a confirmation
//     mail whose link confirms; the other is unknown to Listmonk;
//   - while the API user is missing the delivery is retried and says why, and succeeds afterwards;
//   - erasing the lead deletes the subscriber and notes it on the conversation;
//   - the audit log and can-core's log hold nothing of the people, and the listener refuses an
//     unsigned request.
//
// It does not cover a real mail provider, domain or server, or an upgrade from an earlier release
// (the approval that upgrade waits for is a unit test). Dead letters and requeueing are unit tests
// too: reaching one takes the dispatcher about ten minutes of backoff.
//
// Runs only with CAN_IT_CANCORE=1 and CAN_IT_LEADS=1 (.github/workflows/lead-adapters.yml). It
// deletes the namespaces of forms, support, newsletter and the mail server: use a throw-away cluster.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/server/auth"
)

const (
	yesAddr = "casey+ci@customer.example"
	noAddr  = "robin@customer.example"
)

func leadCompany() string {
	return fmt.Sprintf(`schema: 1
name: Wrapped Co
domain: %s
provider:
  type: local
modules:
  forms:
    settings:
      newsletter_consent: {text: "Send me the CI newsletter"}
  support:
    settings:
      support_address: %s
      tickets_from_forms: [contact]
  newsletter:
    settings:
      subscribe_from_forms: [contact]
      default_list: newsletter
`, wrappedDomain, supportAddr)
}

func submitLead(t *testing.T, forms, name, email string, consent bool) {
	t.Helper()
	st, b := call(t, "GET", forms+"/_forms/contact/token", "", "", nil)
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &tok); st != 200 || err != nil {
		t.Fatalf("form token: %d %s", st, b)
	}
	time.Sleep(4 * time.Second) // a faster submit is treated as spam
	v := url.Values{"name": {name}, "email": {email}, "message": {"Please call me about widgets"}, "_t": {tok.Token}}
	if consent {
		v.Set("newsletter", "yes")
	}
	if st, b = call(t, "POST", forms+"/_forms/contact", "", "application/x-www-form-urlencoded", []byte(v.Encode())); st != 200 && st != 201 {
		t.Fatalf("form post: %d %s", st, b)
	}
}

// listmonkAdmin signs in to Listmonk's admin with the generated password and returns a JSON caller.
func listmonkAdmin(t *testing.T) (*browser, func(method, path string, in any) json.RawMessage) {
	t.Helper()
	b := newBrowser(portForward(t, "mod-newsletter", "newsletter", 9000), "newsletter."+wrappedDomain)
	login := b.do(t, "GET", "/admin/login", "", "", nil)
	m := regexp.MustCompile(`name="nonce" value="([^"]*)"`).FindStringSubmatch(login.body)
	if m == nil {
		t.Fatalf("no nonce on Listmonk's sign-in page: %d", login.status)
	}
	p := b.form(t, "/admin/login", url.Values{"nonce": {m[1]}, "next": {"/admin"}, "username": {"admin"},
		"password": {adminPassword(t, "newsletter", "CAN_NEWSLETTER_ADMIN_PASSWORD")}})
	if p.status != 302 {
		t.Fatalf("signing in to Listmonk: %d", p.status)
	}
	return b, func(method, path string, in any) json.RawMessage {
		t.Helper()
		var body []byte
		if in != nil {
			body, _ = json.Marshal(in)
		}
		r := b.do(t, method, path, "application/json", string(body), nil)
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if r.status != 200 || json.Unmarshal([]byte(r.body), &env) != nil {
			t.Fatalf("Listmonk %s %s: %d %.300s", method, path, r.status, r.body)
		}
		return env.Data
	}
}

func TestLeadAdaptersThroughCanCore(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" || os.Getenv("CAN_IT_LEADS") != "1" {
		t.Skip("CAN_IT_CANCORE=1 and CAN_IT_LEADS=1 are not both set")
	}
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	t.Cleanup(func() {
		if t.Failed() {
			for _, args := range [][]string{
				{"-n", "can-system", "logs", "deploy/can-core", "--tail=120"},
				{"-n", "mod-support", "get", "pod,svc,networkpolicy", "-o", "wide"},
				{"-n", "mod-support", "logs", "deploy/support", "--all-containers", "--tail=100"},
				{"-n", "mod-newsletter", "logs", "deploy/newsletter", "--all-containers", "--tail=100"},
				{"-n", "mod-forms", "logs", "deploy/forms", "--tail=60"},
				{"-n", sinkNS, "logs", "deploy/mailsink", "--tail=60"},
			} {
				out, _ := kubectl(args...)
				t.Logf("kubectl %v:\n%s", args, out)
			}
		}
		_, _ = kubectl("-n", "can-system", "delete", "secret", "can-newsletter-api", "--ignore-not-found")
		_, _ = kubectl("delete", "namespace", sinkNS, "mod-forms", "mod-support", "mod-newsletter", "--ignore-not-found", "--wait=false")
	})
	applyRoutingFacts(t, routing.Default(wrappedDomain))
	if out, err := kubectlStdin(sinkManifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("deploying the mail server: %v\n%s", err, out)
	}
	mustKubectl(t, "-n", sinkNS, "rollout", "status", "deploy/mailsink", "--timeout=180s")

	canApplyModules(t, leadCompany())
	for _, id := range []string{"forms", "newsletter", "support"} {
		eventually(t, 15*time.Minute, id+" to be rolled out", func() (bool, string) {
			o, err := kubectl("-n", "mod-"+id, "rollout", "status", "deploy/"+id, "--timeout=5s")
			return err == nil, o
		})
	}
	// The mail server is inside the cluster here; a real one is on the internet.
	egress := strings.NewReplacer("mod-support", "mod-newsletter").Replace(sinkEgress)
	if out, err := kubectlStdin(egress, "apply", "-f", "-"); err != nil {
		t.Fatalf("egress to the mail server: %v\n%s", err, out)
	}
	eventually(t, 6*time.Minute, "the support mailbox", func() (bool, string) {
		o := moduleSQL(t, "mod_support", "select count(*) from mailboxes")
		return o == "1", o
	})
	// An automatic reply that is switched on must still not be sent to a form's sender.
	moduleSQL(t, "mod_support", "update mailboxes set auto_reply_enabled = true, auto_reply_subject = 'auto', auto_reply_message = 'auto'")

	// --- Listmonk: SMTP, the double opt-in list and the least-privileged API user ----------------
	_, api := listmonkAdmin(t)
	var settings map[string]any
	if err := json.Unmarshal(api("GET", "/api/settings", nil), &settings); err != nil {
		t.Fatal(err)
	}
	smtp, ok := settings["smtp"].([]any)
	if !ok || len(smtp) == 0 {
		t.Fatal("Listmonk has no SMTP setting")
	}
	server, _ := smtp[0].(map[string]any)
	for k, v := range map[string]any{"enabled": true, "host": sinkHost, "port": 2525, "auth_protocol": "login",
		"username": supportAddr, "password": sinkPassword, "tls_type": "STARTTLS", "tls_skip_verify": true} {
		server[k] = v
	}
	settings["smtp"] = []any{server}
	settings["app.root_url"] = "http://newsletter." + wrappedDomain
	api("PUT", "/api/settings", settings)
	time.Sleep(10 * time.Second) // Listmonk restarts itself on new settings
	_, api = listmonkAdmin(t)
	id := func(raw json.RawMessage) int {
		var v struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(raw, &v); err != nil || v.ID == 0 {
			t.Fatalf("no id in %s", raw)
		}
		return v.ID
	}
	list := id(api("POST", "/api/lists", map[string]any{"name": "newsletter", "type": "public", "optin": "double", "status": "active"}))
	role := id(api("POST", "/api/roles/users", map[string]any{"name": "can-leads", "permissions": []string{"subscribers:get", "subscribers:manage"}}))
	listRole := id(api("POST", "/api/roles/lists", map[string]any{"name": "can-leads-list",
		"lists": []any{map[string]any{"id": list, "permissions": []string{"list:get", "list:manage"}}}}))
	var apiUser struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(api("POST", "/api/users", map[string]any{"username": "can_leads", "name": "can-leads", "type": "api",
		"status": "enabled", "user_role_id": role, "list_role_id": listRole}), &apiUser); err != nil || apiUser.Password == "" {
		t.Fatal("no API token")
	}

	// --- two leads: one ticks the box ------------------------------------------------------------
	forms := portForward(t, "mod-forms", "forms", 8080)
	if p := newBrowser(forms, wrappedDomain).do(t, "GET", "/_forms/contact/form", "", "", nil); !strings.Contains(p.body, `type="checkbox" value="yes"> Send me the CI newsletter`) {
		t.Fatalf("the hosted form has no consent checkbox: %d %s", p.status, p.body)
	}
	submitLead(t, forms, "Casey Yes", yesAddr, true)
	submitLead(t, forms, "Robin No", noAddr, false)

	// --- the operator's view: deliveries, retried while the API user is missing -------------------
	password := "ci admin passphrase 0123456789"
	out, err := kubectlStdin(password+"\n", "-n", "can-system", "exec", "-i", "deploy/can-core", "--", "/can-core", "admin", "bootstrap")
	m := regexp.MustCompile(`TOTP secret \(shown once\): (\S+)`).FindStringSubmatch(out)
	if err != nil || m == nil {
		t.Fatalf("admin bootstrap: %v", err)
	}
	key, err := auth.DecodeTOTPSecret(m[1])
	if err != nil {
		t.Fatal(err)
	}
	type delivery struct {
		ID       int64  `json:"id"`
		Type     string `json:"event_type"`
		Module   string `json:"module"`
		Status   string `json:"status"`
		Attempts int    `json:"attempts"`
	}
	var admin *browser
	signIn := func() {
		t.Helper()
		admin = newBrowser(portForward(t, "can-system", "can-core", 8080), "admin."+wrappedDomain)
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": password, "totp": auth.Code(key, auth.SHA1, 6, 30, time.Now())})
		if p := admin.do(t, "POST", "/api/v1/login", "application/json", string(body), nil); p.status != 200 {
			t.Fatalf("signing in to can-core: %d %s", p.status, p.body)
		}
	}
	deliveries := func() []delivery {
		t.Helper()
		var res struct {
			Deliveries []delivery `json:"deliveries"`
		}
		p := admin.do(t, "GET", "/api/v1/deliveries?limit=100", "", "", nil)
		if err := json.Unmarshal([]byte(p.body), &res); p.status != 200 || err != nil {
			t.Fatalf("deliveries: %d %s", p.status, p.body)
		}
		return res.Deliveries
	}
	signIn()
	eventually(t, 3*time.Minute, "the newsletter delivery to say what is missing", func() (bool, string) {
		for _, d := range deliveries() {
			if d.Module == "newsletter" && d.Status == "pending" && d.Attempts > 0 {
				p := admin.do(t, "GET", fmt.Sprintf("/api/v1/deliveries/%d/attempts", d.ID), "", "", nil)
				if strings.Contains(p.body, "can-newsletter-api") {
					return true, ""
				}
			}
		}
		return false, fmt.Sprintf("%+v", deliveries())
	})
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata: {name: can-newsletter-api, namespace: can-system}\nstringData:\n  username: can_leads\n  token: %q\n", apiUser.Password)
	if o, err := kubectlStdin(secret, "apply", "-f", "-"); err != nil {
		t.Fatalf("the API user's Secret: %v\n%s", err, o)
	}
	mustKubectl(t, "-n", "can-system", "rollout", "restart", "deploy/can-core") // mounted at once
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	time.Sleep(31 * time.Second) // a TOTP code is accepted once
	signIn()

	allDelivered := func(eventType string, want int) {
		t.Helper()
		eventually(t, 12*time.Minute, "every "+eventType+" delivery", func() (bool, string) {
			n := 0
			ds := deliveries()
			for _, d := range ds {
				if d.Type == eventType && (d.Module == "support" || d.Module == "newsletter") {
					if d.Status != "delivered" {
						return false, fmt.Sprintf("%+v", ds)
					}
					n++
				}
			}
			return n == want, fmt.Sprintf("%+v", ds)
		})
	}
	allDelivered("lead.created.v1", 4)

	// --- support ----------------------------------------------------------------------------------
	conversations := moduleSQL(t, "mod_support", "select customer_email from conversations where subject = 'Website enquiry (contact)' order by customer_email")
	if conversations != yesAddr+"\n"+noAddr {
		t.Fatalf("conversations for %q", conversations)
	}
	time.Sleep(75 * time.Second) // FreeScout's queue runs every minute
	if n := moduleSQL(t, "mod_support", "select count(*) from send_logs"); n != "0" {
		t.Errorf("%s messages were sent by FreeScout to form senders", n)
	}

	// --- newsletter: consent only, double opt-in --------------------------------------------------
	subscribers := func() string {
		return moduleSQL(t, "mod_newsletter", "select s.email||' '||sl.status from subscribers s join subscriber_lists sl on sl.subscriber_id = s.id join lists l on l.id = sl.list_id where l.name = 'newsletter' order by 1")
	}
	if got := subscribers(); got != yesAddr+" unconfirmed" {
		t.Fatalf("subscribers: %q", got)
	}
	if n := moduleSQL(t, "mod_newsletter", "select count(*) from subscribers where email = '"+noAddr+"'"); n != "0" {
		t.Fatal("a sender who did not tick the box is in Listmonk")
	}
	sink := newBrowser(portForward(t, sinkNS, "mailsink", 8025), "mailsink")
	var sent []sentMail
	if err := json.Unmarshal([]byte(sink.do(t, "GET", "/sent", "", "", nil).body), &sent); err != nil {
		t.Fatal(err)
	}
	var confirm []string
	for _, msg := range sent {
		for _, to := range msg.To {
			if to == noAddr {
				t.Error("mail was sent to the sender who did not tick the box")
			}
		}
		decoded, _ := io.ReadAll(quotedprintable.NewReader(bytes.NewReader([]byte(msg.Data))))
		if m := regexp.MustCompile(`(/subscription/optin/[0-9a-f-]{36})\?l=([0-9a-f-]{36})`).FindStringSubmatch(string(decoded)); m != nil && len(msg.To) == 1 && msg.To[0] == yesAddr {
			confirm = m
		}
	}
	if confirm == nil {
		t.Fatalf("no confirmation mail to the subscriber among %d messages", len(sent))
	}
	public := newBrowser(portForward(t, "mod-newsletter", "newsletter", 9000), "newsletter."+wrappedDomain)
	if p := public.form(t, confirm[1], url.Values{"l": {confirm[2]}, "confirm": {"true"}}); p.status != 200 {
		t.Fatalf("confirming: %d", p.status)
	}
	if got := subscribers(); got != yesAddr+" confirmed" {
		t.Fatalf("after confirming: %q", got)
	}

	// --- the listener refuses what can-core did not sign ------------------------------------------
	leadsPort := newBrowser(portForward(t, "mod-support", "support-leads", 8081), "support-leads")
	if p := leadsPort.do(t, "POST", "/hooks/lead", "application/json", `{"lead_id":"ld_000000000000000000000000","form_id":"contact"}`, nil); p.status != 401 {
		t.Errorf("unsigned request: %d", p.status)
	}

	// --- erasure ----------------------------------------------------------------------------------
	leadsAPI := portForward(t, "mod-forms", "forms-api", 8081)
	token := moduleToken(t, "mod-forms", "CAN_FORMS_API_TOKEN")
	st, b := call(t, "GET", leadsAPI+"/v1/leads?limit=10", token, "", nil)
	var leads struct {
		Items []struct{ ID, Email string } `json:"items"`
	}
	if err := json.Unmarshal(b, &leads); st != 200 || err != nil {
		t.Fatalf("listing leads: %d %v", st, err)
	}
	erased := ""
	for _, l := range leads.Items {
		if l.Email == yesAddr {
			erased = l.ID
		}
	}
	if st, b = call(t, "DELETE", leadsAPI+"/v1/leads/"+erased, token, "", nil); erased == "" || st != 200 {
		t.Fatalf("erasing the lead %q: %d %s", erased, st, b)
	}
	allDelivered("lead.erased.v1", 2)
	if n := moduleSQL(t, "mod_newsletter", "select count(*) from subscribers where email = '"+yesAddr+"'"); n != "0" {
		t.Error("the subscriber of the erased lead is still there")
	}
	notes := moduleSQL(t, "mod_support", "select c.customer_email from threads t join conversations c on c.id = t.conversation_id where t.type = 3 and t.body like '%"+erased+"%'")
	if notes != yesAddr {
		t.Errorf("the erasure note is on %q", notes)
	}

	// --- nothing personal outside the applications ------------------------------------------------
	auditLog := mustKubectl(t, "-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "audit", "export")
	for _, want := range []string{"lead.forward", "lead.erase_copy", "lead.copy_kept"} {
		if !strings.Contains(auditLog, want) {
			t.Errorf("the audit log lacks %s", want)
		}
	}
	coreLog := mustKubectl(t, "-n", "can-system", "logs", "deploy/can-core")
	for _, pii := range []string{"casey", "Casey", "robin", "Robin", "widgets"} {
		if strings.Contains(auditLog+coreLog, pii) {
			t.Errorf("%q is in the audit log or can-core's log", pii)
		}
	}
}
