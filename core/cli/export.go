package cli

// `can export` (specs/071-company-export): ask the running can-core for an export, over kubectl
// exec (ssh to the server, or --context), and write it to this machine. Nothing is stored on the
// server. The stream is checked while it is written: only an archive that ends with a complete
// manifest is kept.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/exportcrypt"
	"github.com/dodwmd/companyinacan/core/exportfmt"
	"github.com/dodwmd/companyinacan/core/provider"
)

// exportPassphraseEnv may hold the passphrase, so that it is never an argument.
const exportPassphraseEnv = "CAN_EXPORT_PASSPHRASE"

// exportKubectl runs kubectl on this machine (--context), streaming. A variable so that tests
// substitute it.
var exportKubectl = func(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...) //nolint:gosec // checked arguments and the operator's context name
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

var exportPartRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type exportFlags struct {
	context, modulesDir, out, passFile string
	includeSealed, encrypt             bool
	only, skip                         []string
}

// exportReport is what the archive's manifest says that the operator must hear.
type exportReport struct {
	Complete bool     `json:"complete"`
	Warnings []string `json:"warnings"`
	Modules  []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"modules"`
}

func newExportCmd(g *globalOpts) *cobra.Command {
	f := &exportFlags{}
	cmd := &cobra.Command{
		Use:   "export [company-dir]",
		Short: "Export all company data in open formats to a file on this machine",
		Long: "Writes a tar.gz of everything the company holds: company.yaml, the published website, each module's\n" +
			"records as CSV and JSON Lines with a schema, files, audit logs, and the SQL dumps of wrapped\n" +
			"applications (docs/export.md). Passwords, tokens and keys are never included; erased data is not\n" +
			"there. The data is streamed from the server and nothing is stored on it.\n\n" +
			"Encrypted fields show as [sealed] unless --include-sealed is given. A passphrase encrypts the file:\n" +
			"--passphrase-file, $" + exportPassphraseEnv + " or --encrypt (asks on the terminal). There is no flag for the\n" +
			"passphrase itself. Without --context the server recorded for the company directory is asked over ssh.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runExport(cmd, g, f, dir)
		},
	}
	cmd.Flags().StringVar(&f.out, "out", "", "output file (default ./company-export-<time>.tar.gz[.canenc]); an existing file is never overwritten")
	cmd.Flags().StringVar(&f.context, "context", "", "use local kubectl with this kube context instead of ssh to the server")
	cmd.Flags().StringVar(&f.modulesDir, "modules-dir", "", "modules directory (default: ./modules, else the manifests built into can)")
	cmd.Flags().BoolVar(&f.includeSealed, "include-sealed", false, "DECRYPT the sealed fields (salaries, identity and bank numbers, private notes, board materials) into the export")
	cmd.Flags().BoolVar(&f.encrypt, "encrypt", false, "encrypt the file with a passphrase asked on the terminal")
	cmd.Flags().StringVar(&f.passFile, "passphrase-file", "", "encrypt the file with the passphrase on the first line of this file")
	cmd.Flags().StringArrayVar(&f.only, "module", nil, "export only this module (repeatable; `core` is the control plane's own data)")
	cmd.Flags().StringArrayVar(&f.skip, "skip", nil, "leave this module out (repeatable)")
	cmd.AddCommand(newExportDecryptCmd())
	return cmd
}

// passphrase returns the passphrase to encrypt with, nil for none.
func (f *exportFlags) passphrase() ([]byte, error) {
	var p string
	switch {
	case f.passFile != "":
		line, err := newPassphraseSource(f.passFile)("")
		if err != nil {
			return nil, failure("--passphrase-file: %v", err)
		}
		p, _, _ = strings.Cut(line, "\n")
		p = strings.TrimRight(p, "\r")
	case os.Getenv(exportPassphraseEnv) != "":
		p = os.Getenv(exportPassphraseEnv)
	case f.encrypt:
		ask := newPassphraseSource("")
		first, err := ask("Passphrase for the export: ")
		if err != nil {
			return nil, failure("%v", err)
		}
		again, err := ask("The passphrase again: ")
		if err != nil {
			return nil, failure("%v", err)
		}
		if first != again {
			return nil, failure("the two passphrases differ")
		}
		p = first
	default:
		return nil, nil
	}
	if err := exportcrypt.CheckPassphrase([]byte(p)); err != nil {
		return nil, usage("%v", err)
	}
	return []byte(p), nil
}

// remoteArgs are the words of `kubectl ... exec` that start the export in can-core's pod.
func (f *exportFlags) remoteArgs() ([]string, error) {
	args := []string{"-n", "can-system", "exec", "deploy/can-core", "-c", "can-core", "--", "/can-core", "export"}
	if f.includeSealed {
		args = append(args, "--include-sealed")
	}
	for _, l := range []struct {
		flag string
		ids  []string
	}{{"--module", f.only}, {"--skip", f.skip}} {
		for _, id := range l.ids {
			if !exportPartRe.MatchString(id) {
				return nil, usage("%s %q is not a module id", l.flag, id)
			}
			args = append(args, l.flag, id)
		}
	}
	return args, nil
}

// stream returns the function that writes the export to a writer.
func (f *exportFlags) stream(cmd *cobra.Command, g *globalOpts, dir string, args []string) (func(context.Context, io.Writer, io.Writer) error, error) {
	if f.context != "" {
		full := append([]string{"--context", f.context}, args...)
		return func(ctx context.Context, out, errs io.Writer) error { return exportKubectl(ctx, out, errs, full...) }, nil
	}
	t, err := load(cmd, g, dir, f.modulesDir)
	if err != nil {
		return nil, err
	}
	tgt, err := provider.TargetFor(t.spec, t.store, t.deps)
	if err != nil {
		return nil, failure("%s (for a cluster of your own, pass --context)", t.redact.Redact(err.Error()))
	}
	runner := t.deps.Runner
	if runner == nil {
		runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
	}
	s, ok := runner(tgt).(provider.StreamRunner)
	if !ok {
		return nil, failure("this connection cannot stream")
	}
	// Every word is a constant of this file or matched exportPartRe.
	command := "k3s kubectl " + strings.Join(args, " ")
	return func(ctx context.Context, out, errs io.Writer) error { return s.Stream(ctx, command, out, errs) }, nil
}

// checkExport reads a whole archive from r and returns its manifest. It fails unless the archive
// is complete.
func checkExport(r io.Reader) (exportReport, error) {
	var rep exportReport
	zr, err := gzip.NewReader(r)
	if err != nil {
		return rep, fmt.Errorf("this is not an export archive: %w", err)
	}
	tr := tar.NewReader(zr)
	last := ""
	var manifest []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return rep, fmt.Errorf("the archive is incomplete or damaged: %w", err)
		}
		last = h.Name
		if h.Name == exportfmt.ManifestName {
			if manifest, err = io.ReadAll(io.LimitReader(tr, 16<<20)); err != nil {
				return rep, fmt.Errorf("the archive is incomplete or damaged: %w", err)
			}
		}
	}
	// Reading to the end of the gzip stream checks its checksum.
	if _, err := io.Copy(io.Discard, zr); err != nil { //nolint:gosec // discarded
		return rep, fmt.Errorf("the archive is incomplete or damaged: %w", err)
	}
	if last != exportfmt.ManifestName || json.Unmarshal(manifest, &rep) != nil || !rep.Complete {
		return rep, errors.New("the archive does not end with a complete manifest: it is incomplete")
	}
	return rep, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// tail keeps the last bytes written to it: the error messages of the remote command.
type tail struct{ b bytes.Buffer }

func (t *tail) Write(p []byte) (int, error) {
	t.b.Write(p)
	if over := t.b.Len() - 4096; over > 0 {
		t.b.Next(over)
	}
	return len(p), nil
}

func runExport(cmd *cobra.Command, g *globalOpts, f *exportFlags, dir string) error {
	args, err := f.remoteArgs()
	if err != nil {
		return err
	}
	pass, err := f.passphrase()
	if err != nil {
		return err
	}
	out := f.out
	if out == "" {
		out = "company-export-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
		if pass != nil {
			out += exportcrypt.Extension
		}
	}
	if _, err := os.Lstat(out); err == nil {
		return failure("%s exists: it is not overwritten", out)
	}
	stream, err := f.stream(cmd, g, dir, args)
	if err != nil {
		return err
	}
	warn := cmd.ErrOrStderr()
	if f.includeSealed {
		fmt.Fprintln(warn, "WARNING: --include-sealed decrypts salaries, identity and bank account numbers, private notes and board materials into the export.")
		if pass == nil {
			fmt.Fprintln(warn, "WARNING: the file is NOT encrypted. Anyone who can read it can read all of that. Consider --encrypt.")
		}
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A new private file beside the target; it gets the name only when it is complete.
	partial := out + ".partial"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-chosen output path
	if err != nil {
		return failure("%v", err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(partial)
		}
	}()
	size := &countingWriter{w: file}
	var dst io.Writer = size
	var enc io.WriteCloser
	if pass != nil {
		if enc, err = exportcrypt.NewWriter(size, pass); err != nil {
			return failure("%v", err)
		}
		dst = enc
	}

	pr, pw := io.Pipe()
	var remoteErrs tail
	ran := make(chan error, 1)
	go func() {
		err := stream(ctx, pw, &remoteErrs)
		_ = pw.CloseWithError(err)
		ran <- err
	}()
	plain := io.TeeReader(pr, dst)
	rep, checkErr := checkExport(plain)
	if checkErr == nil {
		_, checkErr = io.Copy(io.Discard, plain)
	}
	if checkErr != nil {
		stop() // stop the remote command
		_ = pr.CloseWithError(checkErr)
	}
	runErr := <-ran
	if runErr != nil || checkErr != nil {
		why := strings.TrimSpace(remoteErrs.b.String())
		if why == "" {
			why = errors.Join(runErr, checkErr).Error()
		}
		return failure("no export was written: %s", why)
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			return failure("writing the export: %v", err)
		}
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return failure("writing the export: %v", err)
	}
	// Link, not rename: a file that appeared meanwhile is not replaced.
	if err := os.Link(partial, out); err != nil {
		if errors.Is(err, os.ErrExist) {
			return failure("%s exists: it is not overwritten", out)
		}
		if err := os.Rename(partial, out); err != nil { // a file system without links
			return failure("%v", err)
		}
		keep = true
	}
	w := cmd.OutOrStdout()
	if g.json() {
		return printJSON(w, map[string]any{"file": out, "bytes": size.n, "encrypted": pass != nil, "sealed_fields_decrypted": f.includeSealed, "warnings": rep.Warnings, "modules": rep.Modules})
	}
	fmt.Fprintf(w, "wrote %s (%d bytes, %s)\n", out, size.n, map[bool]string{true: "encrypted: open it with `can export decrypt`", false: "not encrypted"}[pass != nil])
	for _, m := range rep.Modules {
		fmt.Fprintf(w, "  %-14s %s\n", m.ID, m.Status)
	}
	for _, x := range rep.Warnings {
		fmt.Fprintln(warn, "WARNING:", x)
	}
	return nil
}

func newExportDecryptCmd() *cobra.Command {
	var out, passFile string
	cmd := &cobra.Command{
		Use:   "decrypt <file.canenc>",
		Short: "Decrypt an encrypted export into a tar.gz",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				name, ok := strings.CutSuffix(args[0], exportcrypt.Extension)
				if !ok {
					return usage("pass --out: the input does not end in %s", exportcrypt.Extension)
				}
				out = name
			}
			pass := os.Getenv(exportPassphraseEnv)
			if passFile != "" || pass == "" {
				line, err := newPassphraseSource(passFile)("Passphrase of the export: ")
				if err != nil {
					return failure("%v", err)
				}
				pass, _, _ = strings.Cut(line, "\n")
				pass = strings.TrimRight(pass, "\r")
			}
			in, err := os.Open(args[0]) //nolint:gosec // operator-chosen path
			if err != nil {
				return failure("%v", err)
			}
			defer func() { _ = in.Close() }()
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-chosen path
			if err != nil {
				return failure("%v", err)
			}
			err = exportcrypt.Decrypt(f, in, []byte(pass))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(out) // what was written is incomplete
				return failure("%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (not encrypted)\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "output file (default: the input without .canenc)")
	cmd.Flags().StringVar(&passFile, "passphrase-file", "", "read the passphrase from the first line of this file")
	return cmd
}
