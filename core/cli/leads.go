package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `can leads` answers erasure requests about contact-form submissions through the forms module's
// API (specs/054-lead-erasure). Like `can work` it is configured from the environment, so that the
// token stays out of shell history; the address searched for is read from standard input for the
// same reason, never from an argument.

var cliLeadID = regexp.MustCompile(`^ld_[0-9a-f]{24}$`)

type leadsClient struct {
	base, token string
	http        *http.Client
}

func newLeadsClient() (*leadsClient, error) {
	base := os.Getenv("CAN_FORMS_URL")
	if base == "" {
		base = "http://127.0.0.1:8081"
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("CAN_FORMS_URL %q must be an http(s) URL", base)
	}
	token := os.Getenv("CAN_FORMS_TOKEN")
	if token == "" {
		return nil, errors.New("no API token (set CAN_FORMS_TOKEN)")
	}
	// No redirects: one would forward the token elsewhere.
	return &leadsClient{strings.TrimRight(base, "/"), token, &http.Client{Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *leadsClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the forms API did not answer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Code, Message string }
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = "is CAN_FORMS_URL the forms API, and is the forms module recent enough?"
		}
		return fmt.Errorf("%d %s: %s", resp.StatusCode, e.Code, e.Message)
	}
	return json.Unmarshal(b, out)
}

type cliLeadMatch struct {
	ID        string    `json:"id"`
	FormID    string    `json:"form_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Match     string    `json:"match"`
}

type cliErasure struct {
	LeadID        string   `json:"lead_id"`
	ErasedAt      string   `json:"erased_at"`
	AlreadyErased bool     `json:"already_erased,omitempty"`
	Notification  string   `json:"notification,omitempty"`
	UnderWay      bool     `json:"notification_was_under_way,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newLeadsCmd(g *globalOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "leads",
		Short: "Find and erase contact-form submissions (erasure requests)",
		Long: "Talks to the forms module's API. Set CAN_FORMS_URL (default http://127.0.0.1:8081) and\n" +
			"CAN_FORMS_TOKEN (a token with leads.erase). On a server, forward the port first:\n" +
			"  kubectl -n mod-forms port-forward svc/forms-api 8081\n" +
			"See modules/forms/README.md, \"Erasure requests\".",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return usage("leads needs a subcommand: find, erase") },
	}

	find := &cobra.Command{
		Use:   "find",
		Short: "List the submissions that name an email address (read from standard input)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newLeadsClient()
			if err != nil {
				return usage("%v", err)
			}
			fmt.Fprint(cmd.ErrOrStderr(), "Email address: ")
			line, err := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 1024)).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return failure("%v", err)
			}
			fmt.Fprintln(cmd.ErrOrStderr())
			email := strings.TrimSpace(line)
			if email == "" {
				return usage("give the address on standard input")
			}
			var res struct {
				Items     []cliLeadMatch `json:"items"`
				Truncated bool           `json:"truncated"`
			}
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/leads/find", map[string]string{"email": email}, &res); err != nil {
				return failure("%v", err)
			}
			out := cmd.OutOrStdout()
			if g.json() {
				return printJSON(out, res)
			}
			for _, m := range res.Items {
				fmt.Fprintf(out, "%s  %s  %-12s %-10s %s\n", m.ID, m.CreatedAt.UTC().Format(time.DateOnly), m.FormID, m.Status, m.Match)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%d submission(s). \"mention\" means the address is only in a text: read the lead before erasing it.\n", len(res.Items))
			if res.Truncated {
				fmt.Fprintln(cmd.ErrOrStderr(), "There are more: erase these, then search again.")
			}
			return nil
		},
	}

	var yes bool
	erase := &cobra.Command{
		Use:   "erase LEAD-ID...",
		Short: "Erase submissions for good (needs --yes); safe to repeat",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, id := range args {
				if !cliLeadID.MatchString(id) {
					return usage("%q is not a lead id (ld_ and 24 hex digits)", id)
				}
			}
			if !yes {
				return usage("this destroys %d submission(s) for good; repeat with --yes", len(args))
			}
			c, err := newLeadsClient()
			if err != nil {
				return usage("%v", err)
			}
			done := []cliErasure{}
			notes := []string{}
			var failed []string
			var record []string // erased, or possibly erased: to be put in the erasure ledger
			for _, id := range args {
				var e cliErasure
				if err := c.do(cmd.Context(), http.MethodDelete, "/v1/leads/"+id, nil, &e); err != nil {
					failed = append(failed, id+": "+err.Error())
					// Only a refusal (4xx) says that nothing was erased.
					if !strings.HasPrefix(err.Error(), "4") {
						record = append(record, id)
					}
					continue
				}
				for _, n := range e.Notes {
					// the same note for every lead is said once
					if !slices.Contains(notes, n) {
						notes = append(notes, n)
					}
				}
				e.Notes = nil
				done = append(done, e)
				record = append(record, id)
			}
			out := cmd.OutOrStdout()
			if g.json() {
				if err := printJSON(out, map[string]any{"erased": done, "notes": notes}); err != nil {
					return err
				}
			} else {
				for _, e := range done {
					state := "erased"
					switch {
					case e.AlreadyErased:
						state = "was already erased"
					case e.UnderWay:
						state = "erased; a notification was being sent and may have arrived"
					case e.Notification == "cancelled":
						state = "erased; its waiting notification email was cancelled"
					}
					fmt.Fprintf(out, "%s  %s\n", e.LeadID, state)
				}
				for _, n := range notes {
					fmt.Fprintln(out, "note:", n)
				}
			}
			if len(record) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "A restore brings these back unless they are recorded: nothing does that by itself yet. On the server:")
				for _, id := range record {
					fmt.Fprintf(cmd.ErrOrStderr(), "  sudo can backup ledger-add --subject %s --class lead-submission\n", id)
				}
			}
			if len(failed) > 0 {
				return failure("not erased or not finished:\n  %s", strings.Join(failed, "\n  "))
			}
			return nil
		},
	}
	erase.Flags().BoolVar(&yes, "yes", false, "really erase")
	root.AddCommand(find, erase)
	return root
}
