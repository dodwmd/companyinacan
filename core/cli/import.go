package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

// `can import` sends a CSV to the module that owns the records (specs/047-csv-import). Like
// `can work` it takes the address and token from the environment.
func newImportCmd(g *globalOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "import",
		Short: "Import people, customers and suppliers, or work issues from a CSV file",
		Long: "Without --apply nothing is written: the module answers what the file would do. With --apply the\n" +
			"whole file is written or none of it. Rows are matched on their external_id column, so running\n" +
			"the same file again changes nothing. FILE is - for standard input.\n\n" +
			"Set CAN_PEOPLE_URL / CAN_PEOPLE_TOKEN, CAN_FINANCE_URL / CAN_FINANCE_TOKEN or CAN_WORK_URL /\n" +
			"CAN_WORK_TOKEN (URLs default to http://127.0.0.1:8080).",
	}
	for _, t := range []struct{ use, short, module string }{
		{"people", "Directory records (needs HR write rights)", "PEOPLE"},
		{"parties", "Customers and suppliers of finance (needs finance.write)", "FINANCE"},
		{"issues", "Issues of one project (needs work.admin)", "WORK"},
	} {
		var apply bool
		var mapping []string
		var project string
		cmd := &cobra.Command{
			Use:   t.use + " FILE",
			Short: t.short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				o, err := csvimport.ParseQuery(url.Values{"map": mapping})
				if err != nil {
					return usage("%v", err)
				}
				o.Apply = apply
				path := "/v1/import/" + t.use
				if t.use == "issues" {
					if project == "" {
						return usage("--project is required")
					}
					path = "/v1/projects/" + url.PathEscape(project) + "/issues/import"
				}
				if q := o.Query(); q != "" {
					path += "?" + q
				}
				body, err := readCSVArg(cmd, args[0])
				if err != nil {
					return failure("%v", err)
				}
				rep, err := postCSV(cmd.Context(), t.module, path, body)
				if err != nil {
					return err
				}
				if err := printReport(cmd.OutOrStdout(), g, rep); err != nil {
					return failure("%v", err)
				}
				if rep.ErrorCount > 0 {
					return failure("the file has %d error(s)", rep.ErrorCount)
				}
				return nil
			},
		}
		cmd.Flags().BoolVar(&apply, "apply", false, "write (default: preview only)")
		cmd.Flags().StringArrayVar(&mapping, "map", nil, "read a field from a column: field=Header (repeatable)")
		if t.use == "issues" {
			cmd.Flags().StringVar(&project, "project", "", "project key (required)")
		}
		root.AddCommand(cmd)
	}
	return root
}

func readCSVArg(cmd *cobra.Command, path string) ([]byte, error) {
	r := cmd.InOrStdin()
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the operator names the file to import
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, csvimport.MaxBytes+1))
	if err == nil && len(b) > csvimport.MaxBytes {
		err = fmt.Errorf("the file is larger than %d bytes; split it", csvimport.MaxBytes)
	}
	return b, err
}

// postCSV sends the file. Redirects are not followed: they would carry the token elsewhere.
func postCSV(ctx context.Context, module, path string, body []byte) (*csvimport.Report, error) {
	base := os.Getenv("CAN_" + module + "_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, usage("CAN_%s_URL must be an http(s) URL", module)
	}
	token := os.Getenv("CAN_" + module + "_TOKEN")
	if token == "" {
		return nil, usage("no API token (set CAN_%s_TOKEN)", module)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, failure("%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/csv")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, failure("%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, failure("%v", err)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnprocessableEntity {
		var rep csvimport.Report
		// A 422 may also be a module's ordinary error body, which has no rows.
		if err := json.Unmarshal(raw, &rep); err == nil && rep.Rows > 0 {
			return &rep, nil
		}
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Code == "" {
		e.Code, e.Message = "error", "unexpected answer"
	}
	return nil, failure("%v", errors.New(resp.Status+" "+e.Code+": "+e.Message))
}

func printReport(w io.Writer, g *globalOpts, r *csvimport.Report) error {
	if g.json() {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	switch {
	case r.Applied:
		fmt.Fprintf(w, "applied: %d rows: %d created, %d updated, %d unchanged\n", r.Rows, r.Create, r.Update, r.Unchanged)
	default:
		label := "preview"
		if !r.DryRun {
			label = "refused"
		}
		hint := "nothing was written; add --apply to write"
		if !r.DryRun || r.ErrorCount > 0 {
			hint = "nothing was written"
		}
		fmt.Fprintf(w, "%s: %d rows: %d to create, %d to update, %d unchanged, %d failed (%s)\n", label, r.Rows, r.Create, r.Update, r.Unchanged, r.Failed, hint)
	}
	if len(r.Ignored) > 0 {
		fmt.Fprintf(w, "ignored columns: %q\n", r.Ignored)
	}
	line := func(kind string, m csvimport.Message) {
		column := ""
		if m.Column != "" {
			column = m.Column + ": "
		}
		fmt.Fprintf(w, "line %d: %s%s%s\n", m.Line, kind, column, m.Message)
	}
	for _, m := range r.Errors {
		line("", m)
	}
	if more := r.ErrorCount - len(r.Errors); more > 0 {
		fmt.Fprintf(w, "... and %d more errors\n", more)
	}
	for _, m := range r.Warnings {
		line("warning: ", m)
	}
	return nil
}
