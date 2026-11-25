package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/diagnose"
	"github.com/dodwmd/companyinacan/core/doctor"
	"github.com/dodwmd/companyinacan/core/provider"
)

// reachRunner runs the cheap reachability probes of `can doctor` and the log collection of
// `can diagnose`; tests replace it.
var reachRunner doctor.Runner = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed tool names (docker, kubectl) with constant arguments
	return string(out), err
}

const (
	maxMetricsBody = 1 << 20
	maxCompanyFile = 256 << 10
)

func newDiagnoseCmd(g *globalOpts) *cobra.Command {
	var out, companyDir, metricsURL, composeProject, modulesDir string
	var server bool
	var logFiles []string
	var lines int
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Write a support bundle (versions, redacted config, checks, log tail, metrics) to a tar.gz",
		Long: "Collects what a maintainer needs to help you and nothing else: versions, company.yaml with secrets, URLs,\n" +
			"email addresses and contact details removed, prerequisite and reachability checks, the last log lines of the files or compose\n" +
			"project you name, and a metrics snapshot from --metrics-url (the bearer token is read from CAN_METRICS_TOKEN,\n" +
			"never a flag). Only an allowlist of file names can be in the bundle, sizes are capped, every text is scrubbed\n" +
			"for secrets and email addresses (names and phone numbers in free text can survive), and an existing file is\n" +
			"never overwritten. Review it before sharing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				out = "can-diagnose-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
			}
			if metricsURL != "" {
				u, err := url.Parse(metricsURL)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
					return usage("--metrics-url must be an http(s) URL without credentials (the token comes from CAN_METRICS_TOKEN)")
				}
			}
			if composeProject != "" && !diagnose.ValidComposeProject(composeProject) {
				return usage("--compose-project must be a docker compose project name: lower-case letters, digits, - and _, starting with a letter or digit")
			}
			if lines < 1 || lines > 5000 {
				return usage("--lines must be between 1 and 5000")
			}
			b := diagnose.New()
			// Everything that looks like a credential in this process's environment is also removed by
			// value, wherever it shows up (the environment itself is never collected).
			red := provider.NewRedactor()
			for _, kv := range os.Environ() {
				k, v, _ := strings.Cut(kv, "=")
				if envLooksSecret(k) {
					red.Add(v)
				}
			}
			b.AddRedactor(red.Redact)
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			var problems []string
			note := func(what string, err error) {
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", what, diagnose.Scrub(err.Error())))
				}
			}
			note("versions", b.AddJSON("versions.json", map[string]string{
				"can": version, "commit": commit, "date": date, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
			}))
			if raw, err := readCapped(filepath.Join(companyDir, "company.yaml"), maxCompanyFile); err == nil {
				note("config", b.AddText("config.yaml", raw))
			}
			res := append(doctor.Check(lookPath), doctor.Reach(ctx, lookPath, reachRunner)...)
			if server {
				res = append(res, serverCheck(ctx, cmd, g, companyDir, modulesDir))
			}
			note("doctor", b.AddJSON("doctor.json", res))
			if logs := collectLogs(ctx, logFiles, composeProject, lines); logs != "" {
				note("logs", b.AddText("logs.txt", logs))
			}
			if metricsURL != "" {
				m, err := scrapeMetrics(ctx, metricsURL, os.Getenv("CAN_METRICS_TOKEN"))
				if err != nil {
					problems = append(problems, "metrics: "+diagnose.Scrub(err.Error()))
				} else {
					note("metrics", b.AddText("metrics.txt", m))
				}
			}
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-chosen output path
			if err != nil {
				return failure("%v", err)
			}
			if err := b.Encode(f); err != nil {
				_ = f.Close()
				_ = os.Remove(out)
				return failure("writing the bundle: %v", err)
			}
			if err := f.Close(); err != nil {
				return failure("%v", err)
			}
			w := cmd.OutOrStdout()
			if g.json() {
				fmt.Fprintf(w, "{\"bundle\":%q,\"files\":%d}\n", out, len(b.Names())+1)
			} else {
				fmt.Fprintf(w, "wrote %s (%d files). Review it before sharing; secrets and email addresses are removed, names and phone numbers in free text may remain.\n", out, len(b.Names())+1)
				for _, p := range problems {
					fmt.Fprintln(w, "note:", p)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "bundle path (default ./can-diagnose-<time>.tar.gz); an existing file is never overwritten")
	cmd.Flags().StringVar(&companyDir, "company-dir", ".", "directory holding company.yaml")
	cmd.Flags().StringArrayVar(&logFiles, "log-file", nil, "a log file whose last lines to include (repeatable)")
	cmd.Flags().StringVar(&composeProject, "compose-project", "", "include `docker compose -p <name> logs` (for example companyinacan)")
	cmd.Flags().StringVar(&metricsURL, "metrics-url", "", "scrape this /metrics URL (token from $CAN_METRICS_TOKEN)")
	cmd.Flags().IntVar(&lines, "lines", 200, "log lines to keep per source")
	cmd.Flags().BoolVar(&server, "server", false, "also check, over ssh, the server's pod-to-node isolation (into doctor.json)")
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "with --server: modules directory (default: ./modules)")
	return cmd
}

func envLooksSecret(name string) bool {
	n := strings.ToUpper(name)
	for _, p := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "PRIVATE_KEY", "CREDENTIAL"} {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

func readCapped(path string, max int64) (string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-chosen path
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max))
	return string(b), err
}

func collectLogs(ctx context.Context, files []string, composeProject string, lines int) string {
	var sb strings.Builder
	for _, p := range files {
		f, err := os.Open(p) //nolint:gosec // operator-chosen path
		if err != nil {
			fmt.Fprintf(&sb, "== %s: unreadable ==\n", filepath.Base(p))
			continue
		}
		fmt.Fprintf(&sb, "== %s ==\n", filepath.Base(p))
		sb.WriteString(diagnose.TailLines(f, lines, 1024))
		_ = f.Close()
	}
	if composeProject != "" {
		c, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := reachRunner(c, "docker", "compose", "-p", composeProject, "logs", "--no-color", "--tail", fmt.Sprint(lines))
		if err != nil {
			fmt.Fprintf(&sb, "== compose %s: logs unavailable ==\n", composeProject)
		} else {
			fmt.Fprintf(&sb, "== compose %s ==\n", composeProject)
			sb.WriteString(diagnose.TailLines(strings.NewReader(out), lines*8, 1024))
		}
	}
	return sb.String()
}

// scrapeMetrics fetches a /metrics page and keeps the comment lines and the can_, go_ and process_
// series only, so a misconfigured target cannot smuggle arbitrary text into the bundle.
func scrapeMetrics(ctx context.Context, rawURL, token string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("scrape failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("scrape answered HTTP %d", resp.StatusCode)
	}
	var kept []string
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetricsBody))
	for _, l := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(l, "# HELP can_") || strings.HasPrefix(l, "# TYPE can_") || strings.HasPrefix(l, "can_") ||
			strings.HasPrefix(l, "go_") || strings.HasPrefix(l, "process_") {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n") + "\n", nil
}
