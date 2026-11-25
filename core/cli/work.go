package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/work"
)

// `can work` talks to the work tracker's REST API with an API token, so managers and
// automation can script it. Configuration comes from the environment so tokens stay out of
// shell history and process listings: CAN_WORK_URL (default http://127.0.0.1:8080) and
// CAN_WORK_TOKEN.
func newWorkCmd(g *globalOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "work",
		Short: "Create, update, list and transition work items through the work API",
		Long: "Talks to the work tracker's REST API. Set CAN_WORK_URL (default http://127.0.0.1:8080) and\n" +
			"CAN_WORK_TOKEN (an API token). Creates carry an idempotency key, so a retried command\n" +
			"never duplicates an issue; pass --idempotency-key to make a script re-runnable.",
	}
	client := func() (*work.Client, error) {
		url := os.Getenv("CAN_WORK_URL")
		if url == "" {
			url = "http://127.0.0.1:8080"
		}
		c, err := work.NewClient(url, os.Getenv("CAN_WORK_TOKEN"))
		if err != nil {
			return nil, usage("%v", err)
		}
		return c, nil
	}
	run := func(cmd *cobra.Command, fn func(ctx context.Context, c *work.Client) (any, error)) error {
		c, err := client()
		if err != nil {
			return err
		}
		v, err := fn(cmd.Context(), c)
		if err != nil {
			return failure("%v", err)
		}
		return printWork(cmd, g, v)
	}

	// create
	{
		var typ, title, desc, prio, assignee, due, parent, idem, descFile string
		var labels []string
		var estimate float64
		var sprint int64
		cmd := &cobra.Command{
			Use:   "create PROJECT",
			Short: "Create an issue (story, task, bug, epic or subtask)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if strings.TrimSpace(title) == "" {
					return usage("--title is required")
				}
				if descFile != "" {
					b, err := readInput(cmd, descFile)
					if err != nil {
						return failure("%v", err)
					}
					desc = string(b)
				}
				in := work.CreateIssue{Type: typ, Title: title, Description: desc, Priority: prio, Labels: labels}
				f := cmd.Flags()
				if f.Changed("assignee") {
					in.Assignee = &assignee
				}
				if f.Changed("due") {
					in.DueDate = &due
				}
				if f.Changed("parent") {
					in.Parent = &parent
				}
				if f.Changed("estimate") {
					in.Estimate = &estimate
				}
				if f.Changed("sprint") {
					in.SprintID = &sprint
				}
				if idem == "" {
					idem = work.NewIdempotencyKey()
				}
				return run(cmd, func(ctx context.Context, c *work.Client) (any, error) {
					var is work.Issue
					_, err := c.Do(ctx, "POST", "/v1/projects/"+url.PathEscape(args[0])+"/issues", in, &is, "Idempotency-Key", idem)
					return &is, err
				})
			},
		}
		f := cmd.Flags()
		f.StringVar(&typ, "type", "task", "epic, story, task, bug or subtask")
		f.StringVar(&title, "title", "", "one-line summary (required)")
		f.StringVar(&desc, "description", "", "longer description")
		f.StringVar(&descFile, "description-file", "", "read the description from a file (- for stdin)")
		f.StringVar(&prio, "priority", "P2", "P0 (drop everything), P1 (this sprint), P2 (planned), P3 (backlog)")
		f.StringVar(&assignee, "assignee", "", "who does the work")
		f.StringVar(&due, "due", "", "due date, YYYY-MM-DD")
		f.StringVar(&parent, "parent", "", "parent issue key, e.g. ACME-1")
		f.Float64Var(&estimate, "estimate", 0, "estimate (points or hours)")
		f.Int64Var(&sprint, "sprint", 0, "sprint id")
		f.StringSliceVar(&labels, "label", nil, "label (repeatable)")
		f.StringVar(&idem, "idempotency-key", "", "make the create safe to repeat (default: random per run)")
		root.AddCommand(cmd)
	}

	// bulk-create
	{
		var file, idem string
		cmd := &cobra.Command{
			Use:   "bulk-create PROJECT",
			Short: "Create many issues atomically from a JSON file ({\"issues\": [...]} or a bare array)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				raw, err := readInput(cmd, file)
				if err != nil {
					return failure("%v", err)
				}
				var items []work.CreateIssue
				var wrapped struct {
					Issues []work.CreateIssue `json:"issues"`
				}
				if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Issues != nil {
					items = wrapped.Issues
				} else if err := json.Unmarshal(raw, &items); err != nil {
					return usage("%s: want {\"issues\": [...]} or an array of issues: %v", file, err)
				}
				if idem == "" {
					idem = work.NewIdempotencyKey()
				}
				return run(cmd, func(ctx context.Context, c *work.Client) (any, error) {
					var out struct {
						Items []work.Issue `json:"items"`
					}
					_, err := c.Do(ctx, "POST", "/v1/projects/"+url.PathEscape(args[0])+"/issues/bulk", map[string]any{"issues": items}, &out, "Idempotency-Key", idem)
					return out.Items, err
				})
			},
		}
		cmd.Flags().StringVar(&file, "file", "-", "JSON file (- for stdin)")
		cmd.Flags().StringVar(&idem, "idempotency-key", "", "make the whole batch safe to repeat (default: random per run)")
		root.AddCommand(cmd)
	}

	// get
	root.AddCommand(&cobra.Command{
		Use:   "get KEY",
		Short: "Show one issue with its links, children and watchers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *work.Client) (any, error) {
				var d work.IssueDetail
				_, err := c.Do(ctx, "GET", "/v1/issues/"+url.PathEscape(args[0]), nil, &d)
				return &d, err
			})
		},
	})

	// list
	{
		var q, project, sortBy string
		var limit int
		var all bool
		cmd := &cobra.Command{
			Use:   "list",
			Short: "Search issues (query language: status:todo priority:P0,P1 assignee:me label:x -type:bug is:open)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return run(cmd, func(ctx context.Context, c *work.Client) (any, error) {
					items := []work.Issue{}
					cursor := ""
					for {
						v := url.Values{}
						v.Set("q", q)
						if project != "" {
							v.Set("project", project)
						}
						if sortBy != "" {
							v.Set("sort", sortBy)
						}
						v.Set("limit", strconv.Itoa(limit))
						if cursor != "" {
							v.Set("cursor", cursor)
						}
						var page struct {
							Items      []work.Issue `json:"items"`
							NextCursor string       `json:"next_cursor"`
						}
						if _, err := c.Do(ctx, "GET", "/v1/issues?"+v.Encode(), nil, &page); err != nil {
							return nil, err
						}
						items = append(items, page.Items...)
						if !all || page.NextCursor == "" || len(items) >= 10000 {
							break
						}
						cursor = page.NextCursor
					}
					return items, nil
				})
			},
		}
		f := cmd.Flags()
		f.StringVarP(&q, "query", "q", "", "search query")
		f.StringVar(&project, "project", "", "limit to one project key")
		f.StringVar(&sortBy, "sort", "", "created, -created, updated, -updated, priority, due, rank")
		f.IntVar(&limit, "limit", 50, "page size (max 100)")
		f.BoolVar(&all, "all", false, "follow pages (up to 10000 issues)")
		root.AddCommand(cmd)
	}

	// update
	{
		var title, desc, prio, assignee, due, parent string
		var estimate float64
		var sprint, ifMatch int64
		var addLabels, rmLabels []string
		cmd := &cobra.Command{
			Use:   "update KEY",
			Short: "Change fields of an issue (use --if-match for optimistic concurrency)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				f := cmd.Flags()
				body := map[string]any{}
				set := func(flag, field string, v any) {
					if f.Changed(flag) {
						body[field] = v
					}
				}
				orNull := func(s string) any {
					if s == "" || s == "none" {
						return nil
					}
					return s
				}
				set("title", "title", title)
				set("description", "description", desc)
				set("priority", "priority", prio)
				set("assignee", "assignee", orNull(assignee))
				set("due", "due_date", orNull(due))
				set("parent", "parent", orNull(parent))
				set("estimate", "estimate", estimate)
				if f.Changed("sprint") {
					if sprint == 0 {
						body["sprint_id"] = nil
					} else {
						body["sprint_id"] = sprint
					}
				}
				if len(addLabels) > 0 {
					body["labels_add"] = addLabels
				}
				if len(rmLabels) > 0 {
					body["labels_remove"] = rmLabels
				}
				if len(body) == 0 {
					return usage("nothing to change: give at least one field flag")
				}
				var hdr []string
				if f.Changed("if-match") {
					hdr = []string{"If-Match", `"` + strconv.FormatInt(ifMatch, 10) + `"`}
				}
				return run(cmd, func(ctx context.Context, c *work.Client) (any, error) {
					var is work.Issue
					_, err := c.Do(ctx, "PATCH", "/v1/issues/"+url.PathEscape(args[0]), body, &is, hdr...)
					return &is, err
				})
			},
		}
		f := cmd.Flags()
		f.StringVar(&title, "title", "", "new title")
		f.StringVar(&desc, "description", "", "new description")
		f.StringVar(&prio, "priority", "", "P0, P1, P2 or P3")
		f.StringVar(&assignee, "assignee", "", "new assignee (\"none\" unassigns)")
		f.StringVar(&due, "due", "", "due date YYYY-MM-DD (\"none\" clears)")
		f.StringVar(&parent, "parent", "", "parent issue key (\"none\" clears)")
		f.Float64Var(&estimate, "estimate", 0, "estimate")
		f.Int64Var(&sprint, "sprint", 0, "sprint id (0 moves it to the backlog)")
		f.StringSliceVar(&addLabels, "add-label", nil, "add a label (repeatable)")
		f.StringSliceVar(&rmLabels, "remove-label", nil, "remove a label (repeatable)")
		f.Int64Var(&ifMatch, "if-match", 0, "only apply if the issue is still at this version")
		root.AddCommand(cmd)
	}

	// transition
	root.AddCommand(&cobra.Command{
		Use:   "transition KEY STATUS",
		Short: "Move an issue along its workflow (for example in_progress, in_review, done)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *work.Client) (any, error) {
				var is work.Issue
				_, err := c.Do(ctx, "POST", "/v1/issues/"+url.PathEscape(args[0])+"/transitions", map[string]string{"to": args[1]}, &is)
				return &is, err
			})
		},
	})

	// comment
	{
		var body string
		cmd := &cobra.Command{
			Use:   "comment KEY [TEXT]",
			Short: "Add a comment (TEXT, --body, or standard input with --body -)",
			Args:  cobra.RangeArgs(1, 2),
			RunE: func(cmd *cobra.Command, args []string) error {
				text := body
				if len(args) == 2 {
					text = args[1]
				}
				if text == "-" {
					b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
					if err != nil {
						return failure("%v", err)
					}
					text = string(b)
				}
				if strings.TrimSpace(text) == "" {
					return usage("a comment needs text")
				}
				return run(cmd, func(ctx context.Context, c *work.Client) (any, error) {
					var out work.Comment
					_, err := c.Do(ctx, "POST", "/v1/issues/"+url.PathEscape(args[0])+"/comments", map[string]string{"body": text}, &out)
					return &out, err
				})
			},
		}
		cmd.Flags().StringVar(&body, "body", "", "comment text (- reads standard input)")
		root.AddCommand(cmd)
	}
	return root
}

func readInput(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(cmd.InOrStdin(), 8<<20))
	}
	return os.ReadFile(path)
}

// printWork prints JSON with --output json and one readable line per issue otherwise.
func printWork(cmd *cobra.Command, g *globalOpts, v any) error {
	out := cmd.OutOrStdout()
	if g.json() {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	line := func(i *work.Issue) {
		who := "-"
		if i.Assignee != nil {
			who = *i.Assignee
		}
		fmt.Fprintf(out, "%-10s %-2s %-12s %-14s %s\n", i.Key, i.Priority, i.Status, who, i.Title)
	}
	switch t := v.(type) {
	case *work.Issue:
		line(t)
	case *work.IssueDetail:
		line(&t.Issue)
		if t.Description != "" {
			fmt.Fprintf(out, "\n%s\n", t.Description)
		}
		for _, l := range t.Links {
			fmt.Fprintf(out, "  %s (%s) %s %s\n", l.Type, l.Direction, l.Key, l.Title)
		}
	case []work.Issue:
		for i := range t {
			line(&t[i])
		}
	case *work.Comment:
		fmt.Fprintf(out, "comment %d added to %s\n", t.ID, t.Issue)
	default:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	return nil
}
