package files

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// Permissions of the files API (modules/files/module.yaml). write includes read and admin includes
// both. They are the coarse gate: a space role (viewer, editor, manager) can only narrow them.
const (
	PermRead  = "files.read"
	PermWrite = "files.write"
	PermAdmin = "files.admin"
)

// Role is a role inside one space. A person's effective role is the strongest of the roles granted
// to them and to the groups they belong to.
type Role int

// Roles, weakest first.
const (
	RoleNone Role = iota
	RoleViewer
	RoleEditor
	RoleManager
)

var roleNames = map[string]Role{"viewer": RoleViewer, "editor": RoleEditor, "manager": RoleManager}

func (r Role) String() string {
	for n, v := range roleNames {
		if v == r {
			return n
		}
	}
	return "none"
}

// Sentinel errors. Handlers map them to statuses; messages are written by this package and carry
// no personal data.
var (
	ErrNotFound    = errors.New("not found")
	ErrForbidden   = errors.New("forbidden")
	ErrInvalid     = errors.New("invalid request")
	ErrConflict    = errors.New("conflict")
	ErrTooLarge    = errors.New("file too large")
	ErrQuota       = errors.New("quota exceeded")
	ErrType        = errors.New("unsupported file type")
	ErrUnavailable = errors.New("unavailable")
	ErrInfected    = errors.New("the file was refused by the virus scanner")
	ErrIntegrity   = errors.New("stored content failed its checksum")
	ErrRate        = errors.New("too many failed attempts")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}
func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, a...))
}

var (
	idRe      = regexp.MustCompile(`^(spc|fld|fil|shr)_[0-9a-f]{24}$`)
	subjectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	userRe    = regexp.MustCompile(`^(user|svc):[A-Za-z0-9_.@-]{1,64}$`)
	groupRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	refRe     = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,31}:[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

func validID(prefix, s string) bool { return len(s) > 4 && s[:4] == prefix+"_" && idRe.MatchString(s) }

// Principal is an authenticated caller.
type Principal struct {
	Subject string
	Perms   []string
	Groups  []string // opaque directory group ids, from a GroupResolver
	// Restore marks the restore-only credential: it may call the purge hook, ignoring holds and
	// retention dates, and nothing else. It is not a permission a token can be given.
	Restore bool
}

// Has reports whether the principal holds a permission (write includes read; admin includes both).
func (p *Principal) Has(perm string) bool {
	if slices.Contains(p.Perms, PermAdmin) {
		return true
	}
	if perm == PermRead && slices.Contains(p.Perms, PermWrite) {
		return true
	}
	return slices.Contains(p.Perms, perm)
}

// memberKeys are the access-list keys that can grant this principal a role.
func (p *Principal) memberKeys() []string {
	ks := []string{"user|" + p.Subject}
	for _, g := range p.Groups {
		ks = append(ks, "group|"+g)
	}
	return ks
}

// Space is a container of files with its own access list.
type Space struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner,omitempty"` // managers only
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Member is one grant in a space's access list.
type Member struct {
	Kind string `json:"kind"` // user or group
	ID   string `json:"id"`
	Role string `json:"role"`
}

// Folder organises files inside a space. It carries no access list of its own.
type Folder struct {
	ID        string    `json:"id"`
	SpaceID   string    `json:"space_id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// FileView is a file as one caller may see it. Fields above the caller's role are omitted:
// viewers get the plain description, editors add checksum, scan state and the uploader, managers
// add the subject, reference, hold and retention.
type FileView struct {
	ID          string     `json:"id"`
	SpaceID     string     `json:"space_id"`
	FolderID    string     `json:"folder_id,omitempty"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	ContentType string     `json:"content_type"`
	Size        int64      `json:"size"`
	Version     int        `json:"version"`
	Status      string     `json:"status"`
	Rev         int        `json:"rev"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Role        string     `json:"role"`
	SHA256      string     `json:"sha256,omitempty"`
	Scan        string     `json:"scan,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
	Ref         string     `json:"ref,omitempty"`
	Subject     string     `json:"subject,omitempty"`
	LegalHold   *bool      `json:"legal_hold,omitempty"`
	RetainUntil *time.Time `json:"retain_until,omitempty"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// VersionView is one immutable version of a file.
type VersionView struct {
	N           int       `json:"n"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	SHA256      string    `json:"sha256,omitempty"`
	Scan        string    `json:"scan,omitempty"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Purged      bool      `json:"purged,omitempty"`
}

// Share is a share link as listed (the token is shown once, at creation).
type Share struct {
	ID        string     `json:"id"`
	FileID    string     `json:"file_id"`
	Version   int        `json:"version"`
	Label     string     `json:"label,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	OneTime   bool       `json:"one_time"`
	Uses      int        `json:"uses"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Page is a list result with the cursor of the next page ("" at the end).
type Page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next_cursor,omitempty"`
}
