//go:build integration

package integration

// specs/059-support-mailbox: the support inbox configured from company.yaml, on a real cluster with
// the pinned FreeScout image, against the in-memory mail server of dev/mailsink (IMAP over TLS, SMTP
// with STARTTLS, self-signed). It proves that
//
//   - enabling the module creates the mailbox with outgoing and incoming settings, with no step by hand;
//   - a message put into the IMAP mailbox becomes a conversation by FreeScout's own scheduled fetch;
//   - the relay's self-signed certificate is refused by default, and accepted only when the
//     operator's relay Secret says tls_verify: "false"; a reply then reaches the SMTP server,
//     from the support address, in the thread;
//   - `can support status` and `credentials` report; a wrong IMAP password is applied and reported
//     without being printed; and opting out removes what was written.
//
// What it does NOT cover: a real mail provider, a certificate that passes verification (the server's
// is self-signed; IMAP's check is turned off by its Secret throughout), IMAP with STARTTLS, deliverability or DNS records. The
// network rule that lets the module reach a server inside the cluster is added by the test: a real
// mail server is on the internet, which the module's own policy allows.
//
// Runs only with CAN_IT_CANCORE=1 and CAN_IT_SUPPORT_MAIL=1 (.github/workflows/support-mail.yml),
// with the image mailsink:test imported. It deletes the namespaces mod-support and can-test-mail:
// use a throw-away cluster.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/routing"
)

const (
	sinkNS       = "can-test-mail"
	sinkHost     = "mailsink." + sinkNS + ".svc.cluster.local"
	sinkPassword = `ci mail $ecret "not" real`
	supportAddr  = "support@" + wrappedDomain
	customerAddr = "casey@customer.example"
)

const sinkManifest = `apiVersion: v1
kind: Namespace
metadata: {name: ` + sinkNS + `}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: mailsink, namespace: ` + sinkNS + `}
spec:
  selector: {matchLabels: {app: mailsink}}
  template:
    metadata: {labels: {app: mailsink}}
    spec:
      containers:
        - name: mailsink
          image: mailsink:test
          imagePullPolicy: Never
          env:
            - {name: MAILSINK_USER, value: ` + supportAddr + `}
            - {name: MAILSINK_PASSWORD, value: '` + sinkPassword + `'}
          readinessProbe: {httpGet: {path: /healthz, port: 8025}}
---
apiVersion: v1
kind: Service
metadata: {name: mailsink, namespace: ` + sinkNS + `}
spec:
  selector: {app: mailsink}
  ports:
    - {name: smtp, port: 2525}
    - {name: imap, port: 9993}
    - {name: http, port: 8025}
`

const sinkEgress = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: ci-allow-mailsink, namespace: mod-support}
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ` + sinkNS + `}}}]
      ports: [{protocol: TCP, port: 2525}, {protocol: TCP, port: 9993}]
`

func supportMailCompany(configured, enabled bool) string {
	y := fmt.Sprintf("schema: 1\nname: Wrapped Co\ndomain: %s\nprovider:\n  type: local\nemail:\n  relay: {host: %s, port: 2525}\n  modules:\n    support: {enabled: %v}\n", wrappedDomain, sinkHost, configured)
	y += fmt.Sprintf("modules:\n  support:\n    enabled: %v\n    settings:\n      support_address: %s\n      mailbox_name: Wrapped Support\n", enabled, supportAddr)
	if configured {
		y += fmt.Sprintf("      inbound:\n        imap: {host: %s}\n", sinkHost)
	}
	return y
}

// mailSecrets writes the operator's two Secrets. relayUnchecked adds tls_verify: "false" to the
// relay's, the only way to turn the certificate check off.
func mailSecrets(t *testing.T, imapPassword string, relayUnchecked bool) {
	t.Helper()
	optOut := ""
	if relayUnchecked {
		optOut = "  tls_verify: \"false\"\n"
	}
	y := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata: {name: can-smtp-relay, namespace: can-system}\nstringData:\n  host: %s\n  username: %s\n  password: '%s'\n  from: no-reply@%s\n%s"+
		"---\napiVersion: v1\nkind: Secret\nmetadata: {name: can-support-imap, namespace: can-system}\nstringData:\n  host: %s\n  username: %s\n  password: '%s'\n  port: \"9993\"\n  tls_verify: \"false\"\n",
		sinkHost, supportAddr, sinkPassword, wrappedDomain, optOut, sinkHost, supportAddr, imapPassword)
	if out, err := kubectlStdin(y, "apply", "-f", "-"); err != nil {
		t.Fatalf("creating the mail Secrets: %v\n%s", err, out)
	}
	// The kubelet takes a minute or two to show a changed Secret in can-core's volume: a restart
	// mounts it at once.
	mustKubectl(t, "-n", "can-system", "rollout", "restart", "deploy/can-core")
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
}

// canSupport runs `can support <args>` for a company directory with this company.yaml.
func canSupport(t *testing.T, company string, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/company.yaml", []byte(company), 0o600); err != nil {
		t.Fatal(err)
	}
	all := append(append([]string{"run", "./cmd/can", "support"}, args...), dir, "--context", kubeContext())
	cmd := exec.Command("go", all...) //nolint:gosec // test helper
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type sentMail struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Data string   `json:"data"`
}

func TestSupportMailThroughCanCore(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" || os.Getenv("CAN_IT_SUPPORT_MAIL") != "1" {
		t.Skip("CAN_IT_CANCORE=1 and CAN_IT_SUPPORT_MAIL=1 are not both set")
	}
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	t.Cleanup(func() {
		if t.Failed() {
			for _, args := range [][]string{
				{"-n", "can-system", "logs", "deploy/can-core", "--tail=120"},
				{"-n", "mod-support", "get", "pod,secret,networkpolicy", "-o", "wide"},
				{"-n", "mod-support", "logs", "deploy/support", "--all-containers", "--tail=150"},
				{"-n", "mod-support", "exec", "deploy/support", "-c", "app", "--", "sh", "-c", "tail -n 60 /data/storage/logs/fetch-emails.log /data/storage/logs/queue-jobs.log"},
				{"-n", sinkNS, "logs", "deploy/mailsink", "--tail=60"},
			} {
				out, _ := kubectl(args...)
				t.Logf("kubectl %v:\n%s", args, out)
			}
		}
		_, _ = kubectl("-n", "can-system", "delete", "secret", "can-smtp-relay", "can-support-imap", "--ignore-not-found")
		_, _ = kubectl("delete", "namespace", sinkNS, "--ignore-not-found", "--wait=false")
	})
	applyRoutingFacts(t, routing.Default(wrappedDomain))
	if out, err := kubectlStdin(sinkManifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("deploying the mail server: %v\n%s", err, out)
	}
	mustKubectl(t, "-n", sinkNS, "rollout", "status", "deploy/mailsink", "--timeout=180s")
	mailSecrets(t, sinkPassword, false)

	company := supportMailCompany(true, true)
	canApplyModules(t, company)
	eventually(t, 5*time.Minute, "can-core to create the module namespace", func() (bool, string) {
		o, err := kubectlStdin(sinkEgress, "apply", "-f", "-")
		return err == nil, o
	})
	rolledOut := func(what string) {
		eventually(t, 12*time.Minute, what, func() (bool, string) {
			o, err := kubectl("-n", "mod-support", "rollout", "status", "deploy/support", "--timeout=5s")
			return err == nil, o
		})
	}
	rolledOut("support to be rolled out")

	// 1. The mailbox, with nothing done by hand.
	mailbox := "select email||' '||name||' '||coalesce(in_server,'')||':'||coalesce(in_port,0)||' '||coalesce(out_server,'')||':'||coalesce(out_port,'0') from mailboxes"
	want := fmt.Sprintf("%s Wrapped Support %s:9993 %s:2525", supportAddr, sinkHost, sinkHost)
	eventually(t, 6*time.Minute, "the mailbox to be configured", func() (bool, string) {
		o, err := kubectl("-n", "can-data", "exec", "deploy/postgres", "--", "psql", "-U", "postgres", "-d", "mod_support", "-tAc", mailbox)
		return err == nil && o == want, o
	})

	// 2. A message into the IMAP mailbox becomes a conversation, by the scheduled fetch.
	sink := newBrowser(portForward(t, sinkNS, "mailsink", 8025), "mailsink")
	inbound := "From: Casey Customer <" + customerAddr + ">\r\nTo: " + supportAddr + "\r\nSubject: CI widget is broken\r\n" +
		"Message-ID: <ci-widget-1@customer.example>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nIt stopped working this morning.\r\n"
	if p := sink.do(t, "POST", "/inbound", "message/rfc822", inbound, nil); p.status != 200 {
		t.Fatalf("injecting the message: %d %s", p.status, p.body)
	}
	var conversation string
	eventually(t, 5*time.Minute, "the message to become a conversation", func() (bool, string) {
		conversation = moduleSQL(t, "mod_support", "select id||' '||mailbox_id||' '||customer_email from conversations where subject = 'CI widget is broken'")
		return strings.HasSuffix(conversation, " "+customerAddr), conversation
	})
	ids := strings.Fields(conversation)
	if len(ids) != 3 {
		t.Fatalf("expected one conversation, got %q", conversation)
	}

	// 3. A reply written in FreeScout leaves through the relay, from the support address.
	b := newBrowser(portForward(t, "mod-support", "support", 8080), "support."+wrappedDomain)
	password := adminPassword(t, "support", "CAN_SUPPORT_ADMIN_PASSWORD")
	token := func(path string) string {
		p := b.do(t, "GET", path, "", "", nil)
		if p.status == 302 { // a conversation redirects to itself with its folder
			u, err := url.Parse(p.location)
			if err != nil {
				t.Fatal(err)
			}
			p = b.do(t, "GET", u.RequestURI(), "", "", nil)
		}
		m := csrfRE.FindStringSubmatch(p.body)
		if p.status != 200 || m == nil {
			t.Fatalf("GET %s: %d, CSRF token found: %v", path, p.status, m != nil)
		}
		return m[1]
	}
	if p := b.form(t, "/login", url.Values{"_token": {token("/login")}, "email": {"admin@" + wrappedDomain}, "password": {password}}); p.status != 302 || strings.HasSuffix(p.location, "/login") {
		t.Fatalf("signing in: %d to %q", p.status, p.location)
	}
	csrf := token("/conversation/" + ids[0])
	reply := func(text string) {
		t.Helper()
		v := url.Values{"_token": {csrf}, "action": {"send_reply"}, "conversation_id": {ids[0]}, "mailbox_id": {ids[1]},
			"body": {"<div>" + text + "</div>"}, "status": {"1"}}
		p := b.do(t, "POST", "/conversation/ajax", "application/x-www-form-urlencoded", v.Encode(),
			map[string]string{"X-Requested-With": "XMLHttpRequest", "X-CSRF-TOKEN": csrf})
		if p.status != 200 || !strings.Contains(p.body, `"status":"success"`) {
			t.Fatalf("replying: %d %.300s", p.status, p.body)
		}
	}
	allSent := func() []sentMail {
		var sent []sentMail
		pg := sink.do(t, "GET", "/sent", "", "", nil)
		if err := json.Unmarshal([]byte(pg.body), &sent); err != nil {
			t.Fatalf("listing sent mail: %v", err)
		}
		return sent
	}

	// 3a. The mail server's certificate is self-signed: by default FreeScout refuses it. This is
	// the proof, in the real image, that the relay's certificate is checked.
	eventually(t, 4*time.Minute, "the unchecked certificate to be reported", func() (bool, string) {
		o, err := canSupport(t, company, "status")
		return err != nil && strings.Contains(o, "send:   failing (SMTP: TLS failed"), o
	})
	reply("This-must-not-leave.")
	eventually(t, 3*time.Minute, "FreeScout to try to send, and fail", func() (bool, string) {
		n := moduleSQL(t, "mod_support", "select count(*) from send_logs where email = '"+customerAddr+"'")
		return n != "0", n
	})
	if sent := allSent(); len(sent) != 0 {
		t.Fatalf("%d messages were sent to a server whose certificate cannot be checked", len(sent))
	}
	if logs := mustKubectl(t, "-n", sinkNS, "logs", "deploy/mailsink"); !strings.Contains(logs, "smtp: TLS handshake") {
		t.Errorf("the mail server saw no refused handshake:\n%s", logs)
	}

	// 3b. The operator's Secret turns the check off, and the reply leaves.
	mailSecrets(t, sinkPassword, true)
	eventually(t, 6*time.Minute, "the opt-out to reach the module", func() (bool, string) {
		o, err := kubectl("-n", "mod-support", "get", "secret", "can-smtp", "-o", "jsonpath={.data.verify}")
		return err == nil && o == base64.StdEncoding.EncodeToString([]byte("false")), o
	})
	rolledOut("support to be back with the opt-out")
	eventually(t, 6*time.Minute, "mail send to be reported healthy", func() (bool, string) {
		o, err := canSupport(t, company, "status")
		return err == nil && strings.Contains(o, "send:   ok"), o
	})
	b = newBrowser(portForward(t, "mod-support", "support", 8080), "support."+wrappedDomain) // the pod is a new one
	if p := b.form(t, "/login", url.Values{"_token": {token("/login")}, "email": {"admin@" + wrappedDomain}, "password": {password}}); p.status != 302 || strings.HasSuffix(p.location, "/login") {
		t.Fatalf("signing in again: %d to %q", p.status, p.location)
	}
	csrf = token("/conversation/" + ids[0])
	reply("Please-restart-the-widget.")
	var got sentMail
	eventually(t, 5*time.Minute, "the reply to reach the mail server", func() (bool, string) {
		sent := allSent()
		for _, m := range sent {
			if len(m.To) == 1 && m.To[0] == customerAddr && strings.Contains(m.Data, "Please-restart-the-widget") {
				got = m
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d messages", len(sent))
	})
	if got.From != supportAddr {
		t.Errorf("envelope sender %q", got.From)
	}
	for _, header := range []string{"From: Wrapped Support <" + supportAddr + ">", "Subject: Re: CI widget is broken", "In-Reply-To: <ci-widget-1@customer.example>"} {
		if !strings.Contains(got.Data, header) {
			t.Errorf("the reply lacks %q", header)
		}
	}

	// 4. Health and the administrator sign-in, as the operator reads them.
	eventually(t, 4*time.Minute, "mail fetch and send to be reported healthy", func() (bool, string) {
		o, err := canSupport(t, company, "status")
		return err == nil && strings.Contains(o, "config: ok") && strings.Contains(o, "fetch:  ok") && strings.Contains(o, "send:   ok"), o
	})
	if o, err := canSupport(t, company, "credentials", "--print-secrets"); err != nil || !strings.Contains(o, "Password: "+password) || !strings.Contains(o, "admin@"+wrappedDomain) {
		t.Errorf("can support credentials: %v", err)
	}

	// 5. A new, wrong IMAP password is applied by itself and reported, without being repeated.
	const wrong = "ci-wrong-imap-password"
	mailSecrets(t, wrong, true)
	eventually(t, 8*time.Minute, "the failing fetch to be reported", func() (bool, string) {
		o, err := canSupport(t, company, "status")
		return err != nil && strings.Contains(o, "fetch:  failing (IMAP: the server refused the user name or password)"), o
	})
	rolledOut("support to be back")
	logs := mustKubectl(t, "-n", "mod-support", "logs", "deploy/support", "-c", "app")
	status, _ := canSupport(t, company, "status")
	for _, secret := range []string{wrong, sinkPassword, base64.StdEncoding.EncodeToString([]byte(wrong)), base64.StdEncoding.EncodeToString([]byte(sinkPassword))} {
		if strings.Contains(logs+status, secret) {
			t.Error("a mail password is in the logs or the status")
		}
	}
	if n := moduleSQL(t, "mod_support", "select count(*) from mailboxes"); n != "1" {
		t.Errorf("%s mailboxes after being applied twice", n)
	}

	// 6. Opting out removes what was written; the mailbox and its conversation stay.
	canApplyModules(t, supportMailCompany(false, true))
	eventually(t, 10*time.Minute, "the mail settings to be removed", func() (bool, string) {
		o := moduleSQL(t, "mod_support", "select coalesce(in_server,'')||'|'||coalesce(out_server,'')||'|'||(select count(*) from options where name like 'mail_%') from mailboxes")
		return o == "||0", o
	})
	if n := moduleSQL(t, "mod_support", "select count(*) from conversations"); n != "1" {
		t.Errorf("%s conversations", n)
	}

	canApplyModules(t, supportMailCompany(false, false))
	eventually(t, 150*time.Second, "support to scale to zero", func() (bool, string) {
		o, _ := kubectl("-n", "mod-support", "get", "deploy", "support", "-o", "jsonpath={.spec.replicas}/{.status.replicas}")
		return o == "0/" || o == "0/0", o
	})
	mustKubectl(t, "delete", "namespace", "mod-support", "--wait=true", "--timeout=180s")
}
