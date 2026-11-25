package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/schema"
	"github.com/dodwmd/companyinacan/core/site"
	"github.com/dodwmd/companyinacan/core/staticsrv"
)

// renderSite validates <dir>/company.yaml against the schema and renders its site.
// Validation problems are returned as problems; renderer errors as err.
func renderSite(dir, baseURL string) (*site.Output, []problem.Problem, error) {
	doc, ps := config.Load(filepath.Join(dir, "company.yaml"))
	if doc == nil {
		return nil, ps, nil
	}
	ps = append(ps, schema.Validate(schema.Company, doc)...)
	if problem.HasErrors(ps) {
		problem.Sort(ps)
		return nil, ps, nil
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
	// root.FS() refuses to follow symlinks out of the company directory.
	out, err := site.Render(c, site.Options{BaseURL: baseURL, Assets: root.FS()})
	return out, ps, err
}

func newSiteCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Build and preview the company website",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newSiteBuildCmd(g), newSiteServeCmd(g))
	return cmd
}

func newSiteBuildCmd(g *globalOpts) *cobra.Command {
	var out, baseURL string
	var noCompress, force bool
	cmd := &cobra.Command{
		Use:   "build [dir]",
		Short: "Render the site: section of company.yaml into static files",
		Long: "Renders <dir>/company.yaml (default .) into static HTML, CSS, sitemap.xml and robots.txt.\n" +
			"Output is deterministic. Text files are also written gzip-precompressed unless --no-compress is given.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if out == "" {
				out = filepath.Join(dir, "public")
			}
			if overlaps(out, dir) {
				return failure("--out %s contains the company directory %s; choose a separate output directory", out, dir)
			}
			res, ps, err := renderSite(dir, baseURL)
			if err != nil {
				return failure("%s", err)
			}
			if problem.HasErrors(ps) {
				if err := printProblems(cmd, g, ps); err != nil {
					return err
				}
				return silent(ExitFailure)
			}
			if err := res.Write(out, site.WriteOptions{Force: force, Compress: !noCompress}); err != nil {
				return failure("%s", err)
			}
			files := res.Paths()
			w := cmd.OutOrStdout()
			if g.json() {
				return json.NewEncoder(w).Encode(struct {
					Out   string   `json:"out"`
					Files []string `json:"files"`
				}{out, files})
			}
			for _, f := range files {
				fmt.Fprintf(w, "  %s (%d bytes)\n", f, len(res.Files[f]))
			}
			fmt.Fprintf(w, "wrote %d files to %s\n", len(files), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "output directory (default <dir>/public)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "public origin for canonical/sitemap URLs (default https://<domain>)")
	cmd.Flags().BoolVar(&noCompress, "no-compress", false, "do not write precompressed .gz files")
	cmd.Flags().BoolVar(&force, "force", false, "write into a non-empty directory that is not a previous site output (only generated paths are overwritten; nothing is deleted)")
	return cmd
}

type serveOptions struct {
	Dir    string // company directory to build and watch
	Public string // already-built site directory to serve as is (skips building)
	Addr   string
}

func newSiteServeCmd(g *globalOpts) *cobra.Command {
	var o serveOptions
	cmd := &cobra.Command{
		Use:   "serve [dir]",
		Short: "Preview the site locally (rebuilds when company.yaml changes)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Dir = "."
			if len(args) == 1 {
				o.Dir = args[0]
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := serveSite(ctx, o, cmd.OutOrStdout(), nil); err != nil {
				return failure("%s", err)
			}
			return nil
		},
	}
	_ = g
	cmd.Flags().StringVar(&o.Addr, "addr", "127.0.0.1:8080", "listen address (loopback by default)")
	cmd.Flags().StringVar(&o.Public, "public", "", "serve this already-built directory instead of building")
	return cmd
}

// previewHandler rebuilds the site when company.yaml changes. A failed rebuild keeps the last good site.
type previewHandler struct {
	next    http.Handler
	dir     string
	site    string // directory the build is written to
	baseURL string
	out     io.Writer

	mu    sync.Mutex
	stamp string // modification stamp of company.yaml and the logo at the last build
}

// sourceStamp changes whenever company.yaml or the logo file it references changes.
func sourceStamp(dir string) string {
	files := []string{filepath.Join(dir, "company.yaml")}
	if doc, _ := config.Load(files[0]); doc != nil {
		if c, err := config.DecodeCompany(doc); err == nil && c.Site.Brand.Logo != "" {
			files = append(files, filepath.Join(dir, filepath.FromSlash(c.Site.Brand.Logo)))
		}
	}
	var b strings.Builder
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", f, st.ModTime().UnixNano(), st.Size())
		}
	}
	return b.String()
}

func (p *previewHandler) rebuild() {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := sourceStamp(p.dir)
	if s == "" || s == p.stamp {
		return
	}
	p.stamp = s
	res, ps, err := renderSite(p.dir, p.baseURL)
	if err == nil && res != nil {
		err = res.Write(p.site, site.WriteOptions{})
	}
	switch {
	case err != nil:
		fmt.Fprintln(p.out, "rebuild failed:", err)
	case problem.HasErrors(ps):
		for _, pr := range ps {
			fmt.Fprintln(p.out, pr.String())
		}
		fmt.Fprintln(p.out, "rebuild failed; still serving the last good build")
	default:
		fmt.Fprintln(p.out, "rebuilt site")
	}
}

func (p *previewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.rebuild()
	p.next.ServeHTTP(w, r)
}

// serveSite runs the preview server until ctx is cancelled. ready, when set, receives the
// listen address once the server accepts connections.
func serveSite(ctx context.Context, o serveOptions, out io.Writer, ready func(addr string)) error {
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	if ta, ok := ln.Addr().(*net.TCPAddr); ok && !ta.IP.IsLoopback() {
		fmt.Fprintf(out, "warning: listening on %s, which is not loopback; the preview has no authentication and is reachable from the network\n", addr)
	}
	var handler http.Handler
	if o.Public != "" {
		handler = staticsrv.New(staticsrv.Config{Dir: o.Public, AccessLog: out})
	} else {
		tmp, err := os.MkdirTemp("", "can-site-preview-")
		if err != nil {
			_ = ln.Close()
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		base := "http://" + addr
		res, ps, err := renderSite(o.Dir, base)
		if err == nil && problem.HasErrors(ps) {
			for _, pr := range ps {
				fmt.Fprintln(out, pr.String())
			}
			err = errors.New("company.yaml is invalid")
		}
		if err == nil {
			err = res.Write(tmp, site.WriteOptions{})
		}
		if err != nil {
			_ = ln.Close()
			return err
		}
		ph := &previewHandler{dir: o.Dir, site: tmp, baseURL: base, out: out}
		ph.stamp = sourceStamp(o.Dir)
		ph.next = staticsrv.New(staticsrv.Config{Dir: tmp, AccessLog: out})
		handler = ph
	}
	srv := staticsrv.NewServer(addr, handler)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(out, "serving on http://%s (Ctrl-C to stop)\n", addr)
	if ready != nil {
		ready(addr)
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// overlaps reports whether out is dir itself or one of its ancestors.
func overlaps(out, dir string) bool {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			a = p
		}
		if r, err := filepath.EvalSymlinks(a); err == nil {
			return r
		}
		return a
	}
	o, d := abs(out), abs(dir)
	rel, err := filepath.Rel(o, d)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
