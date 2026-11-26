package backup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cmd is one external command. Env entries are added to the process environment.
type Cmd struct {
	Name  string
	Args  []string
	Env   []string
	Stdin io.Reader
	// CleanEnv runs the command with Env, PATH and the temporary directory only, nothing else
	// this process inherited.
	CleanEnv bool
}

// Runner executes external commands. restic only ever runs behind it, so tests
// use FakeRunner and never need restic, a bucket or network.
type Runner interface {
	Run(ctx context.Context, c Cmd) ([]byte, error)
}

// CmdError is a command that ran and failed. Stderr is kept for diagnosis;
// Env is deliberately not part of the message.
type CmdError struct {
	Cmd    string
	Code   int
	Stderr string
}

func (e *CmdError) Error() string {
	return fmt.Sprintf("%s: exit %d: %s", e.Cmd, e.Code, strings.TrimSpace(e.Stderr))
}

// ExecRunner is the real Runner (os/exec).
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, c Cmd) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // argv is built by this package from validated values
	if c.CleanEnv {
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir()}, c.Env...)
	} else if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	cmd.Stdin = c.Stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), &CmdError{Cmd: c.Name, Code: ee.ExitCode(), Stderr: errb.String()}
		}
		return out.Bytes(), fmt.Errorf("%s: %w", c.Name, err)
	}
	return out.Bytes(), nil
}

// FakeRunner records commands and answers through Handle (nil Handle succeeds
// with no output).
type FakeRunner struct {
	mu     sync.Mutex
	Calls  []Cmd
	Handle func(Cmd) ([]byte, error)
}

// Run implements Runner.
func (f *FakeRunner) Run(_ context.Context, c Cmd) ([]byte, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	h := f.Handle
	f.mu.Unlock()
	if h == nil {
		return nil, nil
	}
	return h(c)
}

// LoadEnvFile parses a KEY=VALUE credentials file (restic.env): comments,
// `export` prefixes and single or double quotes are accepted.
func LoadEnvFile(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-chosen path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var env []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, "export "))
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s: malformed line", path)
		}
		if v, err = unquoteShell(v); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, k, err)
		}
		env = append(env, k+"="+v)
	}
	return env, sc.Err()
}

// Repo identifies a restic repository. The password is only ever given as a
// file; bucket credentials as environment entries of the restic process.
type Repo struct {
	Repository   string
	PasswordFile string
	Env          []string
}

// Snapshot is the part of `restic snapshots --json` this package uses.
type Snapshot struct {
	ID      string    `json:"id"`
	ShortID string    `json:"short_id"`
	Time    time.Time `json:"time"`
	Paths   []string  `json:"paths"`
	Tags    []string  `json:"tags"`
}

// ResticAPI is what verify, status and restore need from restic.
type ResticAPI interface {
	// Snapshots lists snapshots, newest first.
	Snapshots(ctx context.Context) ([]Snapshot, error)
	// Restore restores the includes (all paths when empty) of a snapshot
	// ("latest" allowed) under target.
	Restore(ctx context.Context, snapshot, target string, includes []string) error
	// Check verifies repository integrity; readData also reads every pack.
	Check(ctx context.Context, readData bool) error
}

// Restic wraps the restic CLI behind a Runner.
type Restic struct {
	R    Runner
	Repo Repo
	Bin  string // default "restic"
	// KitOnly lists with what Repo says and nothing from the environment, without a lock or a
	// cache (`can backup snapshots`).
	KitOnly bool
}

var _ ResticAPI = (*Restic)(nil)

func (r *Restic) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "restic"
	}
	env := append([]string{"RESTIC_REPOSITORY=" + r.Repo.Repository, "RESTIC_PASSWORD_FILE=" + r.Repo.PasswordFile}, r.Repo.Env...)
	if r.KitOnly {
		args = append(args, "--no-lock", "--no-cache")
	}
	return r.R.Run(ctx, Cmd{Name: bin, Args: args, Env: env, CleanEnv: r.KitOnly})
}

// EnsureInit creates the repository when (and only when) restic reports that
// none exists; any other failure is returned unchanged.
func (r *Restic) EnsureInit(ctx context.Context) (created bool, err error) {
	_, err = r.run(ctx, "cat", "config")
	if err == nil {
		return false, nil
	}
	var ce *CmdError
	if !errors.As(err, &ce) || ce.Code != 10 {
		return false, err
	}
	if _, err := r.run(ctx, "init"); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureNoRepo returns nil when restic reports that no repository exists
// (exit 10). If a repository answers, or anything else fails, it returns the
// error so callers refuse rather than guess.
func (r *Restic) EnsureNoRepo(ctx context.Context) (bool, error) {
	_, err := r.run(ctx, "cat", "config")
	var ce *CmdError
	if errors.As(err, &ce) && ce.Code == 10 {
		return true, nil
	}
	if err == nil {
		return false, &CmdError{Cmd: "restic cat config", Code: 0, Stderr: "a repository already exists"}
	}
	return false, err
}

// Check implements ResticAPI.
func (r *Restic) Check(ctx context.Context, readData bool) error {
	args := []string{"check"}
	if readData {
		args = append(args, "--read-data")
	}
	_, err := r.run(ctx, args...)
	return err
}

// Snapshots implements ResticAPI.
func (r *Restic) Snapshots(ctx context.Context) ([]Snapshot, error) {
	out, err := r.run(ctx, "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	snaps, err := parseSnapshots(out)
	if err != nil {
		return nil, err
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	return snaps, nil
}

// Restore implements ResticAPI.
func (r *Restic) Restore(ctx context.Context, snapshot, target string, includes []string) error {
	args := []string{"restore", snapshot, "--target", target}
	for _, inc := range includes {
		args = append(args, "--include", inc)
	}
	_, err := r.run(ctx, args...)
	return err
}

// Forget applies the retention policy and prunes.
func (r *Restic) Forget(ctx context.Context, days int) error {
	_, err := r.run(ctx, "forget", "--group-by", "", "--keep-within", fmt.Sprintf("%dd", days), "--prune", "--tag", "can")
	return err
}

// shellQuote single-quotes v for sourcing by a POSIX shell.
func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// FormatEnvFile renders KEY=VALUE entries as a file the backup script can source
// and LoadEnvFile reads back identically (values are single-quoted).
func FormatEnvFile(env []string) string {
	var b strings.Builder
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		b.WriteString(k + "=" + shellQuote(v) + "\n")
	}
	return b.String()
}

// unquoteShell evaluates one shell word made of '...', "..." and \x parts.
// No expansion is performed: $ and backticks are literal, as the script's
// credentials are never meant to expand.
func unquoteShell(v string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\'':
			j := strings.IndexByte(v[i+1:], '\'')
			if j < 0 {
				return "", errors.New("unterminated single quote")
			}
			out.WriteString(v[i+1 : i+1+j])
			i += j + 1
		case '"':
			i++
			for ; i < len(v) && v[i] != '"'; i++ {
				if v[i] == '\\' && i+1 < len(v) && strings.ContainsRune("\\\"$`", rune(v[i+1])) {
					i++
				}
				out.WriteByte(v[i])
			}
			if i >= len(v) {
				return "", errors.New("unterminated double quote")
			}
		case '\\':
			i++
			if i < len(v) {
				out.WriteByte(v[i])
			}
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), nil
}
