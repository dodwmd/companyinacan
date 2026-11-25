//go:build integration

package integration

// Epic E03c: an in-house module installed through can-core on a real cluster. The dev-cluster
// workflow builds the can-work image, imports it into k3d and sets CAN_IT_WORK=1; locally the test
// is skipped. It covers: enable `work` from company.yaml, wait for the pod (the chart's probes and
// securityContext are admitted by the restricted namespace), issue a personal access token through
// the admin API, create a project and an issue with it against the module's ClusterIP Service,
// revoke the token, disable the module and check the data volume is kept.
//
// What it does NOT cover: forms, people, jobs and finance on a cluster; event delivery between
// modules; approvals; the admin UI page for tokens.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/auth"
)

func workCompany(enabled bool) string {
	return fmt.Sprintf("schema: 1\nname: Work Co\ndomain: work.example\nprovider:\n  type: local\nmodules:\n  work:\n    enabled: %v\n", enabled)
}

// canApplyModules is canApply against the repository's real modules directory.
func canApplyModules(t *testing.T, companyYAML string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/company.yaml", []byte(companyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./cmd/can", "apply", dir, "--modules-dir", "modules", "--context", kubeContext()) //nolint:gosec // test helper
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("can apply: %v\n%s", err, out)
	}
	t.Logf("can apply: %s", strings.TrimSpace(string(out)))
}

// portForward forwards svc to a free local port and returns the base URL.
func portForward(t *testing.T, ns, svc string, port int) string {
	t.Helper()
	local := freePort(t)
	pf := exec.Command("kubectl", "--context", kubeContext(), "-n", ns, "port-forward", "svc/"+svc, fmt.Sprintf("%d:%d", local, port)) //nolint:gosec // test helper
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pf.Process.Kill(); _ = pf.Wait() })
	base := fmt.Sprintf("http://127.0.0.1:%d", local)
	eventually(t, 30*time.Second, "port-forward to "+svc, func() (bool, string) {
		resp, err := http.Get(base + "/healthz") //nolint:gosec,noctx // loopback test helper
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return true, ""
	})
	return base
}

func doJSON(t *testing.T, method, url string, headers map[string]string, body any) (int, map[string]any, []*http.Cookie) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req) //nolint:gosec // loopback
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Cookies()
}

func TestWorkModuleThroughCanCore(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" || os.Getenv("CAN_IT_WORK") != "1" {
		t.Skip("CAN_IT_CANCORE=1 and CAN_IT_WORK=1 are not both set: can-core and the can-work image are not in this cluster")
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=120")
			t.Logf("can-core log:\n%s", out)
			out, _ = kubectl("-n", "mod-work", "get", "all,pvc,cm,secret,networkpolicy", "-o", "wide")
			t.Logf("mod-work:\n%s", out)
			out, _ = kubectl("-n", "mod-work", "logs", "deploy/work", "--tail=80")
			t.Logf("work log:\n%s", out)
		}
	})
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")

	// An admin account, created inside the can-core pod like an operator would (password on stdin).
	const password = "ci-Passw0rd-not-a-secret-9f31"
	user := fmt.Sprintf("ci%d", time.Now().Unix())
	out, err := kubectlStdin(password+"\n", "-n", "can-system", "exec", "-i", "deploy/can-core", "--", "/can-core", "admin", "bootstrap", "--username", user)
	if err != nil {
		t.Fatalf("admin bootstrap: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`TOTP secret \(shown once\): ([A-Z2-7]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no TOTP secret in the bootstrap output")
	}
	key, err := auth.DecodeTOTPSecret(m[1])
	if err != nil {
		t.Fatal(err)
	}

	// 1. enable work through company.yaml; the image was built and imported by the workflow.
	canApplyModules(t, workCompany(true))
	eventually(t, 240*time.Second, "work to be rolled out", func() (bool, string) {
		o, err := kubectl("-n", "mod-work", "rollout", "status", "deploy/work", "--timeout=5s")
		return err == nil, o
	})
	// the chart's objects exist
	mustKubectl(t, "-n", "mod-work", "get", "svc/work", "pvc/work-data", "cm/work-config")

	// 2. a personal access token for work from the admin API (session cookie + CSRF, like the UI).
	admin := portForward(t, "can-system", "can-core", 8080)
	code := auth.Code(key, auth.SHA1, 6, 30, time.Now())
	st, body, cookies := doJSON(t, "POST", admin+"/api/v1/login", nil, map[string]string{"username": user, "password": password, "totp": code})
	if st != 200 {
		t.Fatalf("login: %d %v", st, body)
	}
	var session string
	for _, c := range cookies {
		if c.Name == auth.CookieName { // not via a cookie jar: the cookie is Secure and this is plain loopback HTTP
			session = c.Value
		}
	}
	csrf, _ := body["csrf_token"].(string)
	hdr := map[string]string{"Cookie": auth.CookieName + "=" + session, "X-CSRF-Token": csrf}
	st, body, _ = doJSON(t, "POST", admin+"/api/v1/tokens", hdr, map[string]any{
		"module": "work", "permissions": []string{"work.read", "work.write", "work.admin"}, "ttl_hours": 1, "name": "ci",
	})
	if st != 201 && st != 200 {
		t.Fatalf("issue token: %d %v", st, body)
	}
	pat, _ := body["token"].(string)
	id := fmt.Sprint(body["id"])
	if !strings.HasPrefix(pat, "canpat_") {
		t.Fatalf("unexpected token format (the value is not logged)")
	}

	// 3. use it against the module's ClusterIP Service: can-work asks can-core to introspect it.
	w := portForward(t, "mod-work", "work", 8080)
	bearer := map[string]string{"Authorization": "Bearer " + pat}
	if st, _, _ := doJSON(t, "GET", w+"/v1/projects", map[string]string{"Authorization": "Bearer canpat_not-a-real-token"}, nil); st != 401 {
		t.Errorf("a made-up token must be 401, got %d", st)
	}
	eventually(t, 60*time.Second, "work to accept the personal access token", func() (bool, string) {
		st, b, _ := doJSON(t, "POST", w+"/v1/projects", bearer, map[string]string{"key": "CI", "name": "CI project"})
		return st == 201 || st == 409, fmt.Sprint(st, b)
	})
	st, b, _ := doJSON(t, "POST", w+"/v1/projects/CI/issues", map[string]string{"Authorization": "Bearer " + pat, "Idempotency-Key": "ci-issue-0001"},
		map[string]string{"type": "task", "title": "created through can-core with a personal access token"})
	if st != 201 && st != 200 {
		t.Fatalf("create issue: %d %v", st, b)
	}
	if k, _ := b["key"].(string); !strings.HasPrefix(k, "CI-") {
		t.Errorf("issue key %v", b["key"])
	}

	// 4. revoke: the module stops accepting it once its short positive cache expires.
	if st, b, _ := doJSON(t, "DELETE", admin+"/api/v1/tokens/"+id, hdr, nil); st != 204 && st != 200 {
		t.Fatalf("revoke: %d %v", st, b)
	}
	eventually(t, 120*time.Second, "work to refuse the revoked token", func() (bool, string) {
		st, _, _ := doJSON(t, "GET", w+"/v1/projects", bearer, nil)
		return st == 401, fmt.Sprint(st)
	})

	// 5. disable: workload to zero, data volume and Secret stay.
	canApplyModules(t, workCompany(false))
	eventually(t, 120*time.Second, "work to scale to zero", func() (bool, string) {
		o, _ := kubectl("-n", "mod-work", "get", "deploy", "work", "-o", "jsonpath={.spec.replicas}/{.status.replicas}")
		return o == "0/" || o == "0/0", o
	})
	mustKubectl(t, "-n", "mod-work", "get", "pvc", "work-data")
	mustKubectl(t, "-n", "mod-work", "get", "secret", "can-module")
}
