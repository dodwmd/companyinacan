package adminui

import (
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/overview"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

// The business overview (specs/053-business-overview): a few aggregate figures per module and what
// needs attention. It is open to every signed-in person and shows each only what they may see: the
// module figures come from the modules themselves, which apply their own rules to the viewer.

// backupStale is how old the last database dump may be before the page says so (dumps run daily).
const backupStale = 36 * time.Hour

// maxWaiting bounds the approvals counted.
const maxWaiting = 200

// home is where sign-in lands (the root of the site): the business overview, which every signed-in
// person may open. The technical dashboard is at /dashboard.
func (s *Server) home(w http.ResponseWriter, r *http.Request, _ *sessionCtx) {
	flash := r.URL.Query().Get("flash")
	if _, ok := messagesEN[flash]; !ok || !strings.HasPrefix(flash, "flash.") {
		flash = ""
	}
	s.redirect(w, r, "/overview", flash)
}

func (s *Server) overviewRoutes() []route {
	return []route{{"GET /overview", s.authed(s.overviewPage)}}
}

// overviewLink is where a figure leads: a page of the admin UI (Href) or a page of a module's own
// UI, opened through the Apps launcher (App and Return).
type overviewLink struct{ Href, App, Return, Name, CSRF string }

type overviewRow struct {
	Label     string
	Value     string
	Amounts   []string
	Attention bool
	Link      *overviewLink
	// Module is set in the "needs attention" list only.
	Module string
}

type overviewSection struct {
	ID, Name    string
	Unavailable bool
	At          time.Time
	Rows        []overviewRow
}

type overviewData struct {
	Attention []overviewRow
	Sections  []overviewSection
	// Failed means the modules could not be listed, or the person's rights could not be read.
	Failed bool
	// Incomplete means something that may need attention is missing from the page.
	Incomplete bool
}

func (s *Server) overviewPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ctx, me := r.Context(), subjectOf(sc)
	d := overviewData{}
	// A part that cannot be read is said to be unavailable, never left out as if there were nothing.
	platform, err := s.allowed(r, sc, rbac.PermView)
	if err != nil {
		s.Log.Error("overview: cannot read the person's rights", "error", err)
		platform, d.Failed = false, true
	}

	// What can-core itself knows.
	if s.Approvals != nil {
		sec := overviewSection{ID: "approvals", Name: s.msg("nav.approvals")}
		if waiting, err := s.Approvals.Waiting(ctx, me, maxWaiting); err != nil {
			s.Log.Error("overview: cannot read approvals", "error", err)
			sec.Unavailable = true
		} else {
			n := strconv.Itoa(len(waiting))
			if len(waiting) >= maxWaiting {
				n += "+"
			}
			sec.Rows = []overviewRow{{Label: s.msg("ov.approvals"), Value: n, Attention: len(waiting) > 0, Link: &overviewLink{Href: "/approvals", Name: s.msg("nav.approvals")}}}
		}
		d.Sections = append(d.Sections, sec)
	}

	if s.Overview != nil {
		apps := map[string]string{}
		if s.Tickets != nil {
			list, err := s.Tickets.Apps(ctx, me)
			if err != nil { // only the links are lost
				s.Log.Error("overview: cannot list the apps", "error", err)
			}
			for _, a := range list {
				apps[a.ID] = a.Name
			}
		}
		mods, err := s.Overview.Collect(ctx, me)
		if err != nil {
			s.Log.Error("overview failed", "error", err)
			d.Failed = true
		}
		for _, m := range mods {
			sec := overviewSection{ID: m.ID, Name: m.Name, Unavailable: m.Unavailable, At: m.At}
			for _, v := range m.Values {
				sec.Rows = append(sec.Rows, s.overviewRow(m, v, apps, platform, sc.Session.CSRF))
			}
			d.Sections = append(d.Sections, sec)
		}
	}

	if platform && s.Mod != nil && s.Mod.LastDump != nil {
		d.Sections = append(d.Sections, s.backupSection(s.Mod.LastDump))
	}

	d.Incomplete = d.Failed
	for _, sec := range d.Sections {
		d.Incomplete = d.Incomplete || sec.Unavailable
		for _, row := range sec.Rows {
			if row.Attention {
				row.Module = sec.Name
				d.Attention = append(d.Attention, row)
			}
		}
	}
	s.render(w, r, http.StatusOK, "overview.html", s.page(r, sc, "overview", s.msg("ov.title"), d))
}

func (s *Server) overviewRow(m overview.Module, v overview.Value, apps map[string]string, platform bool, csrf string) overviewRow {
	row := overviewRow{Label: s.msg("ov." + "fig_" + strings.ReplaceAll(v.Key, ".", "_")), Value: strconv.FormatInt(v.Count, 10), Attention: v.Attention && v.Count > 0}
	if v.Unit != "" {
		row.Value += " " + v.Unit
	}
	for _, a := range v.Amounts {
		row.Amounts = append(row.Amounts, overview.FormatMoney(a.Currency, a.Minor))
	}
	switch {
	case m.ID == "forms":
		// forms has no web page of its own: new leads are the admin UI's leads view.
		if platform && s.Leads != nil {
			row.Link = &overviewLink{Href: "/leads?status=new", Name: s.msg("nav.leads")}
		}
	case v.Path != "" && apps[m.ID] != "":
		row.Link = &overviewLink{App: m.ID, Return: v.Path, Name: apps[m.ID], CSRF: csrf}
	}
	return row
}

// backupSection reports the last database dump run, which is all can-core knows about backups: the
// off-site copy is made on the server and is checked there (can status, docs/backup-restore.md).
func (s *Server) backupSection(last func() (backup.DumpRun, error)) overviewSection {
	sec := overviewSection{ID: "backup", Name: s.msg("ov.backup")}
	row := overviewRow{Label: s.msg("ov.backup_last"), Link: &overviewLink{Href: "/status", Name: s.msg("ov.backup_status")}}
	run, err := last()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		row.Value, row.Attention = s.msg("ov.backup_none"), true
	case err != nil:
		s.Log.Warn("overview: cannot read the last dump run", "error", err)
		sec.Unavailable = true
		return sec
	default:
		at := run.At.UTC().Format("2006-01-02 15:04") + " UTC"
		switch {
		case !run.OK:
			row.Value, row.Attention = s.msg("ov.backup_failed", at), true
		case s.Now().Sub(run.At) > backupStale:
			row.Value, row.Attention = s.msg("ov.backup_stale", at), true
		case len(run.Skipped) > 0:
			skipped := make([]string, 0, len(run.Skipped))
			for _, k := range run.Skipped {
				skipped = append(skipped, k.ID)
			}
			row.Value, row.Attention = s.msg("ov.backup_skipped", at, strings.Join(skipped, ", ")), true
		default:
			row.Value = s.msg("ov.backup_ok", at)
		}
	}
	sec.Rows = []overviewRow{row}
	return sec
}
