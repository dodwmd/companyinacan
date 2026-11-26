package files

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Every handler here parses a form, calls Store methods (the same ones the JSON API calls, in api.go)
// and renders. What a caller may see or do is decided by the Store alone; the Editor, Manager and
// Change fields below only choose which controls a page shows.

// viewerRole is the presentation side of a space role.
type viewerRole struct {
	Editor, Manager bool // what the role in the space is
	Change          bool // the role and the session's permission together allow changes
	Own             bool // and allow managing the space
}

func roleOf(role string, write bool) viewerRole {
	r := roleNames[role]
	v := viewerRole{Editor: r >= RoleEditor, Manager: r >= RoleManager}
	v.Change, v.Own = v.Editor && write, v.Manager && write
	return v
}

// text normalises a textarea: browsers send CRLF line ends, which the Store refuses as control characters.
func text(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func nextURL(path string, q url.Values, cursor string) string {
	if cursor == "" {
		return ""
	}
	q.Set("cursor", cursor)
	return path + "?" + q.Encode()
}

// ---- spaces ------------------------------------------------------------------------------------

type homeData struct {
	Spaces []Space
	Next   string
}

func (u *UI) home(c *uiCtx) error {
	pg, err := u.Store.ListSpaces(c.ctx(), c.p, c.q("cursor"), uiPageSize)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "home.html", "home.title", homeData{Spaces: pg.Items, Next: nextURL("/", url.Values{}, pg.Next)})
	return nil
}

func (u *UI) spaceCreate(c *uiCtx) error {
	sp, err := u.Store.CreateSpace(c.ctx(), c.p, c.form("name"))
	if err != nil {
		return err
	}
	return c.redirect("/spaces/"+sp.ID, "flash.space_created")
}

type folderRow struct {
	ID, Name, Path string
}

// folderTree orders what the page needs from a space's folder list.
type folderTree struct {
	byID map[string]Folder
	all  []Folder
}

func (u *UI) folders(c *uiCtx, spaceID string) (folderTree, error) {
	fs, err := u.Store.ListFolders(c.ctx(), c.p, spaceID)
	if err != nil {
		return folderTree{}, err
	}
	t := folderTree{byID: map[string]Folder{}, all: fs}
	for _, f := range fs {
		t.byID[f.ID] = f
	}
	return t, nil
}

// chain is the folder and its parents, outermost first (bounded: depth is at most 32).
func (t folderTree) chain(id string) []folderRow {
	var out []folderRow
	for i := 0; i < 64 && id != ""; i++ {
		f, ok := t.byID[id]
		if !ok {
			break
		}
		out = append([]folderRow{{ID: f.ID, Name: f.Name}}, out...)
		id = f.ParentID
	}
	return out
}

func (t folderTree) path(id string) string {
	var names []string
	for _, f := range t.chain(id) {
		names = append(names, f.Name)
	}
	return strings.Join(names, " / ")
}

// options lists every folder with its full path, for a move or upload select.
func (t folderTree) options() []folderRow {
	out := make([]folderRow, 0, len(t.all))
	for _, f := range t.all {
		out = append(out, folderRow{ID: f.ID, Name: f.Name, Path: t.path(f.ID)})
	}
	slices.SortStableFunc(out, func(a, b folderRow) int { return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)) })
	return out
}

type fileListRow struct {
	FileView
	FolderPath string
}

type spaceData struct {
	Space    Space
	Role     viewerRole
	Folder   *folderRow // nil at the top
	Crumbs   []folderRow
	Children []folderRow
	Files    []fileListRow
	Next     string
}

func (u *UI) space(c *uiCtx) error {
	sp, err := u.Store.GetSpace(c.ctx(), c.p, c.id())
	if err != nil {
		return err
	}
	tree, err := u.folders(c, sp.ID)
	if err != nil {
		return err
	}
	folder := c.q("folder")
	d := spaceData{Space: *sp, Role: roleOf(sp.Role, c.p.canWrite())}
	if folder != "" {
		f, ok := tree.byID[folder]
		if !ok {
			return ErrNotFound
		}
		d.Folder = &folderRow{ID: f.ID, Name: f.Name}
		d.Crumbs = tree.chain(f.ID)
	}
	for _, f := range tree.options() {
		if tree.byID[f.ID].ParentID == folder {
			d.Children = append(d.Children, f)
		}
	}
	pg, err := u.Store.ListFiles(c.ctx(), c.p, ListQuery{SpaceID: sp.ID, FolderID: folder, Cursor: c.q("cursor"), Limit: uiPageSize})
	if err != nil {
		return err
	}
	for _, f := range pg.Items {
		d.Files = append(d.Files, fileListRow{FileView: f, FolderPath: tree.path(f.FolderID)})
	}
	q := url.Values{}
	if folder != "" {
		q.Set("folder", folder)
	}
	d.Next = nextURL("/spaces/"+sp.ID, q, pg.Next)
	u.render(c.w, c.r, c, http.StatusOK, "space.html", "space.title", d)
	return nil
}

type trashData struct {
	Space Space
	Role  viewerRole
	Files []FileView
	Next  string
}

func (u *UI) trash(c *uiCtx) error {
	sp, err := u.Store.GetSpace(c.ctx(), c.p, c.id())
	if err != nil {
		return err
	}
	pg, err := u.Store.ListFiles(c.ctx(), c.p, ListQuery{SpaceID: sp.ID, Deleted: true, Cursor: c.q("cursor"), Limit: uiPageSize})
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "trash.html", "trash.title", trashData{Space: *sp, Role: roleOf(sp.Role, c.p.canWrite()),
		Files: pg.Items, Next: nextURL("/spaces/"+sp.ID+"/trash", url.Values{}, pg.Next)})
	return nil
}

// ---- members -----------------------------------------------------------------------------------

type membersData struct {
	Space   Space
	Role    viewerRole
	Members []Member
}

func (u *UI) members(c *uiCtx) error {
	sp, err := u.Store.GetSpace(c.ctx(), c.p, c.id())
	if err != nil {
		return err
	}
	ms, err := u.Store.ListMembers(c.ctx(), c.p, sp.ID)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "members.html", "members.title", membersData{Space: *sp, Role: roleOf(sp.Role, c.p.canWrite()), Members: ms})
	return nil
}

// memberForm reads kind and principal; a bare user name means "user:<name>".
func memberForm(c *uiCtx) (kind, id string) {
	kind, id = c.form("kind"), c.form("principal")
	if kind == "user" && id != "" && !strings.Contains(id, ":") {
		id = "user:" + id
	}
	return kind, id
}

// afterMemberChange stays on the access list unless the person changed their own grant, which may
// have taken the page away from them.
func afterMemberChange(c *uiCtx, spaceID, kind, id string) error {
	if kind == "user" && id == c.p.Subject {
		return c.redirect("/", "flash.member_saved")
	}
	return c.redirect("/spaces/"+spaceID+"/members", "flash.member_saved")
}

func (u *UI) memberSet(c *uiCtx) error {
	kind, id := memberForm(c)
	if err := u.Store.SetMember(c.ctx(), c.p, c.id(), Member{Kind: kind, ID: id, Role: c.form("role")}); err != nil {
		return err
	}
	return afterMemberChange(c, c.id(), kind, id)
}

func (u *UI) memberRemove(c *uiCtx) error {
	kind, id := c.form("kind"), c.form("principal")
	if err := u.Store.RemoveMember(c.ctx(), c.p, c.id(), kind, id); err != nil {
		return err
	}
	return afterMemberChange(c, c.id(), kind, id)
}

// ---- folders -----------------------------------------------------------------------------------

// folderURL is a space page, in a folder when the id is one. Both parts are validated ids.
func folderURL(spaceID, folderID string) string {
	if !validID("spc", spaceID) {
		return "/"
	}
	if validID("fld", folderID) {
		return "/spaces/" + spaceID + "?folder=" + folderID
	}
	return "/spaces/" + spaceID
}

func (u *UI) folderCreate(c *uiCtx) error {
	parent := c.form("parent")
	if _, err := u.Store.CreateFolder(c.ctx(), c.p, c.id(), parent, c.form("name")); err != nil {
		return err
	}
	return c.redirect(folderURL(c.id(), parent), "flash.folder_created")
}

func (u *UI) folderDelete(c *uiCtx) error {
	if err := u.Store.DeleteFolder(c.ctx(), c.p, c.id()); err != nil {
		return err
	}
	return c.redirect(folderURL(c.form("space"), c.form("parent")), "flash.folder_deleted")
}

// ---- uploads -----------------------------------------------------------------------------------

type uploadData struct {
	Space           Space
	Role            viewerRole
	Folders         []folderRow
	Folder          string
	Types, MaxSize  string
	Accept          string
	ScannerRequired bool
}

func (u *UI) uploadForm(c *uiCtx) error {
	sp, err := u.Store.GetSpace(c.ctx(), c.p, c.id())
	if err != nil {
		return err
	}
	role := roleOf(sp.Role, c.p.canWrite())
	if !role.Change { // the Store refuses the post; do not offer a form that cannot succeed
		return ErrForbidden
	}
	tree, err := u.folders(c, sp.ID)
	if err != nil {
		return err
	}
	cfg := u.Store.cfg
	u.render(c.w, c.r, c, http.StatusOK, "upload.html", "upload.title", uploadData{Space: *sp, Role: role, Folders: tree.options(), Folder: c.q("folder"),
		Types: strings.Join(cfg.Limits.AllowedTypes, ", "), Accept: "." + strings.Join(cfg.Limits.AllowedTypes, ",."), MaxSize: humanSize(cfg.Limits.MaxFileBytes),
		ScannerRequired: cfg.ScannerRequired()})
	return nil
}

const uiMaxFieldBytes = 4 << 10

// uploadParts reads the text fields that come before the file (the form puts the file last) and
// returns the file part, unread. NextRawPart: the bytes are never transfer-decoded.
func uploadParts(mr *multipart.Reader, allowed ...string) (map[string]string, *multipart.Part, error) {
	fields := map[string]string{}
	for {
		part, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			return nil, nil, formError{"upload.no_file"}
		}
		if err != nil {
			return nil, nil, bodyError(err)
		}
		name := part.FormName()
		if name == "file" {
			if part.FileName() == "" {
				return nil, nil, formError{"upload.no_file"}
			}
			return fields, part, nil
		}
		if _, seen := fields[name]; seen || !slices.Contains(allowed, name) {
			return nil, nil, formError{"err.bad_form"}
		}
		b, err := io.ReadAll(io.LimitReader(part, uiMaxFieldBytes+1))
		if err != nil {
			return nil, nil, bodyError(err)
		}
		if len(b) > uiMaxFieldBytes {
			return nil, nil, formError{"err.bad_form"}
		}
		fields[name] = strings.TrimSpace(text(string(b)))
	}
}

func bodyError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return ErrTooLarge
	}
	return formError{"err.bad_form"}
}

// declaredSize is a lower bound of the file's size, for the quota check made before staging.
func declaredSize(r *http.Request) int64 { return max(0, r.ContentLength-uiUploadOverhead) }

func (u *UI) upload(c *uiCtx) error {
	fields, part, err := uploadParts(c.parts, "folder", "description", "subject", "ref")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx(), uploadBudget(u.Store.cfg))
	defer cancel()
	f, err := u.Store.Upload(ctx, c.p, UploadRequest{SpaceID: c.id(), FolderID: fields["folder"], Name: part.FileName(), Description: fields["description"],
		Subject: fields["subject"], Ref: fields["ref"], Body: part, Size: declaredSize(c.r)})
	if err != nil {
		return err
	}
	return c.redirect("/files/"+f.ID, "flash.uploaded")
}

func (u *UI) versionUpload(c *uiCtx) error {
	_, part, err := uploadParts(c.parts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx(), uploadBudget(u.Store.cfg))
	defer cancel()
	f, err := u.Store.UploadVersion(ctx, c.p, c.id(), part, declaredSize(c.r))
	if err != nil {
		return err
	}
	return c.redirect("/files/"+f.ID, "flash.version_added")
}

// ---- files -------------------------------------------------------------------------------------

type fileData struct {
	File     FileView
	Space    Space
	Role     viewerRole
	Crumbs   []folderRow
	Folders  []folderRow
	Versions []VersionView
	Shares   []shareRow
	Accept   string
	Deleted  bool
	Held     bool
	Retained bool

	DefaultHours, MaxHours int
}

type shareRow struct {
	Share
	State string // live, revoked, expired or used
}

func (u *UI) file(c *uiCtx) error {
	f, err := u.Store.GetFile(c.ctx(), c.p, c.id())
	if err != nil {
		return err
	}
	sp, err := u.Store.GetSpace(c.ctx(), c.p, f.SpaceID)
	if err != nil {
		return err
	}
	tree, err := u.folders(c, f.SpaceID)
	if err != nil {
		return err
	}
	cfg := u.Store.cfg
	d := fileData{File: *f, Space: *sp, Role: roleOf(f.Role, c.p.canWrite()), Crumbs: tree.chain(f.FolderID), Folders: tree.options(),
		Deleted: f.Status != "active", Held: f.LegalHold != nil && *f.LegalHold,
		Retained:     f.RetainUntil != nil && f.RetainUntil.After(u.Now()),
		DefaultHours: cfg.Share.DefaultTTLHours, MaxHours: cfg.Share.MaxTTLHours}
	if i := strings.LastIndexByte(f.Name, '.'); i >= 0 {
		d.Accept = strings.ToLower(f.Name[i:])
	}
	if d.Versions, err = u.Store.ListVersions(c.ctx(), c.p, f.ID); err != nil {
		return err
	}
	if d.Role.Editor {
		shares, err := u.Store.ListShares(c.ctx(), c.p, f.ID)
		if err != nil {
			return err
		}
		now := u.Now()
		for _, s := range shares {
			state := "live"
			switch {
			case s.RevokedAt != nil:
				state = "revoked"
			case !s.ExpiresAt.After(now):
				state = "expired"
			case s.OneTime && s.Uses > 0:
				state = "used"
			}
			d.Shares = append(d.Shares, shareRow{Share: s, State: state})
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "file.html", "file.title", d)
	return nil
}

// download is the API's content route for a session: the same Store call (audited before a byte is
// sent, checksum verified while streaming) and the same headers, always an attachment.
func (u *UI) download(c *uiCtx) error {
	if c.r.Method == http.MethodHead { // a HEAD must not count as a download
		c.w.Header().Set("Allow", "GET")
		u.renderError(c.w, c.r, c, http.StatusMethodNotAllowed, "err.invalid_title", "err.bad_form")
		return nil
	}
	version := 0
	if v := c.q("version"); v != "" {
		var err error
		if version, err = strconv.Atoi(v); err != nil || version < 1 {
			return ErrNotFound
		}
	}
	content, err := u.Store.OpenContent(c.ctx(), c.p, c.id(), version)
	if err != nil {
		return err
	}
	serveContent(c.w, c.r, content, false, u.Log)
	return nil
}

func (u *UI) fileUpdate(c *uiCtx) error {
	rev, err := strconv.Atoi(c.form("rev"))
	if err != nil || rev < 1 {
		return formError{"err.bad_form"} // without it a stale form would overwrite a newer change
	}
	name, desc, folder := c.form("name"), strings.TrimSpace(text(c.r.PostForm.Get("description"))), c.form("folder")
	if _, err := u.Store.UpdateFile(c.ctx(), c.p, c.id(), FilePatch{Name: &name, Description: &desc, FolderID: &folder, Rev: rev}); err != nil {
		return err
	}
	return c.redirect("/files/"+c.id(), "flash.saved")
}

func (u *UI) fileDelete(c *uiCtx) error {
	if err := u.Store.DeleteFile(c.ctx(), c.p, c.id()); err != nil {
		return err
	}
	return c.redirect("/files/"+c.id(), "flash.deleted")
}

func (u *UI) fileRestore(c *uiCtx) error {
	if err := u.Store.RestoreFile(c.ctx(), c.p, c.id()); err != nil {
		return err
	}
	return c.redirect("/files/"+c.id(), "flash.restored")
}

// ---- share links -------------------------------------------------------------------------------

type linkData struct {
	FileID string
	URL    string
	Share  Share
}

var linkHostRe = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?$`)

// shareURL is the link to give the recipient: share.base_url when set, else the address the person
// used to reach this page (the same host serves /s/). The page is theirs alone and never cached.
func (u *UI) shareURL(r *http.Request, token string) string {
	base := strings.TrimRight(u.Store.cfg.Share.BaseURL, "/")
	if base == "" && linkHostRe.MatchString(r.Host) {
		scheme := "http"
		if u.Secure {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/s/" + token
}

// shareCreate shows the new link in the answer to the post itself: the token is never put in a URL,
// a cookie or the session, and cannot be shown again.
func (u *UI) shareCreate(c *uiCtx) error {
	hours, err := strconv.Atoi(c.form("hours"))
	if err != nil || hours < 1 || hours > 24*366 {
		return formError{"share.bad_hours"}
	}
	version := 0
	if v := c.form("version"); v != "" {
		if version, err = strconv.Atoi(v); err != nil {
			return formError{"err.bad_form"}
		}
	}
	token, sh, err := u.Store.CreateShare(c.ctx(), c.p, c.id(), ShareRequest{TTL: time.Duration(hours) * time.Hour,
		OneTime: c.form("one_time") != "", Label: c.form("label"), Version: version})
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "share_created.html", "share.created_title", linkData{FileID: sh.FileID, URL: u.shareURL(c.r, token), Share: *sh})
	return nil
}

func (u *UI) shareRevoke(c *uiCtx) error {
	if err := u.Store.RevokeShare(c.ctx(), c.p, c.id()); err != nil {
		return err
	}
	if file := c.form("file"); validID("fil", file) {
		return c.redirect("/files/"+file, "flash.revoked")
	}
	return c.redirect("/", "flash.revoked")
}

// ---- administration: legal hold, retention, erasure --------------------------------------------

type adminRow struct {
	AdminFile
	Retained bool
}

type adminData struct {
	Subject string
	Files   []adminRow
	Next    string
	More    bool

	// erasure page
	Erasable int // files the erasure would destroy, among those listed
	Blocked  int // of those, under a legal hold or a retention date
	IsFileID bool
	SSO      bool
}

func (u *UI) adminRows(c *uiCtx, subject, cursor string) (adminData, string, error) {
	pg, err := u.Store.AdminFiles(c.ctx(), c.p, subject, cursor, uiPageSize)
	if err != nil {
		return adminData{}, "", err
	}
	d := adminData{Subject: subject}
	now := u.Now()
	for _, f := range pg.Items {
		d.Files = append(d.Files, adminRow{AdminFile: f, Retained: f.RetainUntil != nil && f.RetainUntil.After(now)})
	}
	return d, pg.Next, nil
}

func (u *UI) adminList(c *uiCtx) error {
	subject := strings.TrimSpace(c.q("subject"))
	d, next, err := u.adminRows(c, subject, c.q("cursor"))
	if err != nil {
		return err
	}
	q := url.Values{}
	if subject != "" {
		q.Set("subject", subject)
	}
	d.Next = nextURL("/admin/files", q, next)
	u.render(c.w, c.r, c, http.StatusOK, "admin_files.html", "admin.title", d)
	return nil
}

type holdData struct{ ID string }

// holdForm does not show the current state: an administrator reads no file, and the list it is
// reached from shows the hold and the date. Nothing is preselected, so a post says what it means.
func (u *UI) holdForm(c *uiCtx) error {
	if !validID("fil", c.id()) {
		return ErrNotFound
	}
	u.render(c.w, c.r, c, http.StatusOK, "hold.html", "hold.title", holdData{ID: c.id()})
	return nil
}

func (u *UI) holdSet(c *uiCtx) error {
	var hold bool
	switch c.form("hold") {
	case "on":
		hold = true
	case "off":
	default:
		return formError{"hold.choose"} // a request that does not say must never lift a hold
	}
	var until *time.Time
	date, clear := c.form("retain_until"), c.form("clear_retention") != ""
	switch {
	case date != "" && clear:
		return formError{"hold.date_and_clear"}
	case clear:
		until = &time.Time{}
	case date != "":
		t, err := time.Parse(time.DateOnly, date)
		if err != nil || t.IsZero() {
			return formError{"hold.bad_date"}
		}
		until = &t
	}
	if err := u.Store.SetHold(c.ctx(), c.p, c.id(), hold, until); err != nil {
		return err
	}
	return c.redirect("/admin/files", "flash.hold_saved")
}

func (u *UI) eraseForm(c *uiCtx) error {
	subject := strings.TrimSpace(c.q("subject"))
	d := adminData{}
	if subject != "" {
		var next string
		var err error
		if d, next, err = u.adminRows(c, subject, ""); err != nil {
			return err
		}
		d.More = next != ""
		for _, f := range d.Files {
			if f.Status == "purged" {
				continue
			}
			d.Erasable++
			if f.LegalHold || f.Retained {
				d.Blocked++
			}
		}
		d.IsFileID = validID("fil", subject)
	}
	d.SSO = c.ss.ssoHandle != ""
	u.render(c.w, c.r, c, http.StatusOK, "erase.html", "erase.title", d)
	return nil
}

type erasedData struct {
	Report  *Erasure
	Subject string
}

func (u *UI) erasePost(c *uiCtx) error {
	subject := c.form("subject")
	if !subjectRe.MatchString(subject) {
		return invalid("the subject must be an opaque id")
	}
	if c.r.PostForm.Get("confirm") != "ERASE" {
		return formError{"erase.type_erase"}
	}
	// Step-up: erasing is irreversible, so a session left open on an unlocked screen is not enough.
	// A token session is asked for its sign-in token again, which must still authenticate now; a
	// launch-ticket session needs a recent admin sign-in, confirmed by can-core (stepUpSSO).
	if c.ss.ssoHandle != "" {
		if err := u.stepUpSSO(c); err != nil {
			return err
		}
	} else {
		if !constEq(c.form("token"), c.ss.token) {
			return formError{"erase.reenter"}
		}
		p, err := u.authenticateToken(c.ctx(), c.ss.token)
		if errors.Is(err, ErrUnavailable) {
			return err
		}
		if err != nil || p == nil || p.Subject != c.ss.subject {
			return ErrForbidden
		}
		p.Groups = c.p.Groups
		c.p = p // EraseSubject checks the permissions the token has now
	}
	longOperation(c.w)
	rep, err := u.Store.EraseSubject(c.ctx(), c.p, subject)
	if errors.Is(err, errScrub) {
		u.Log.Error("erasure follow-up failed", "route", c.r.Pattern, "error", err)
		u.renderMsg(c.w, c.r, c, http.StatusInternalServerError, "err.internal_title", u.msg("erase.scrub_failed"))
		return nil
	}
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "erased.html", "erase.done_title", erasedData{Report: rep, Subject: subject})
	return nil
}
