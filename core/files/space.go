package files

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const (
	maxNameBytes = 255
	maxDescBytes = 1000
)

func roleFromDB(s string) Role { return roleNames[s] }

// placeholders returns "?,?,?" for n values.
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anyArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// roleIn is the caller's effective role in a space: the strongest grant of the person and of their
// groups. RoleNone means "no access", which the caller must treat exactly like a missing space.
func (s *Store) roleIn(ctx context.Context, q dbq, spaceID string, p *Principal) (Role, error) {
	keys := p.memberKeys()
	rows, err := q.QueryContext(ctx, `SELECT role FROM space_members WHERE (space_id = ?) AND (principal IN (`+placeholders(len(keys))+`))`,
		append([]any{spaceID}, anyArgs(keys)...)...)
	if err != nil {
		return RoleNone, err
	}
	defer func() { _ = rows.Close() }()
	best := RoleNone
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return RoleNone, err
		}
		best = max(best, roleFromDB(r))
	}
	return best, rows.Err()
}

// need resolves the role and enforces a minimum: no access is ErrNotFound, too little is
// ErrForbidden (the caller knows the space exists because they can see it).
func (s *Store) need(ctx context.Context, q dbq, p *Principal, spaceID string, min Role, action string) (Role, error) {
	r, err := s.roleIn(ctx, q, spaceID, p)
	if err != nil {
		return RoleNone, err
	}
	if r == RoleNone {
		return RoleNone, ErrNotFound
	}
	if r < min {
		return r, ErrForbidden
	}
	return r, nil
}

// txFor is inTx for a request made by p: when the work is refused for lack of a role, the denial
// is audited after the transaction is gone (the store has one connection, so auditing from inside
// it would wait for itself).
func (s *Store) txFor(ctx context.Context, p *Principal, action, resource string, fn func(tx *sql.Tx) error) error {
	return s.forbid(ctx, p, action, resource, s.inTx(ctx, fn))
}

// forbid audits a refusal (throttled) and returns err unchanged.
func (s *Store) forbid(ctx context.Context, p *Principal, action, resource string, err error) error {
	if errors.Is(err, ErrForbidden) {
		s.auditDeny(ctx, p.Subject, action, resource)
	}
	return err
}

func cleanText(s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return "", invalid("too long (at most %d bytes)", max)
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return "", invalid("control characters are not allowed")
		}
	}
	return s, nil
}

func cleanLabel(s string, max int) (string, error) {
	s, err := cleanText(s, max)
	if err == nil && strings.ContainsAny(s, "\n\t") {
		return "", invalid("must be a single line")
	}
	return s, err
}

// ---- spaces --------------------------------------------------------------------------------------

// CreateSpace makes a space the caller manages.
func (s *Store) CreateSpace(ctx context.Context, p *Principal, name string) (*Space, error) {
	if !p.Has(PermWrite) {
		return nil, ErrForbidden
	}
	name, err := cleanLabel(name, 100)
	if err != nil || name == "" {
		return nil, invalid("a space needs a name of 1 to 100 characters")
	}
	if !userRe.MatchString(p.Subject) {
		return nil, ErrForbidden
	}
	id, now := newID("spc"), s.now()
	sealed, err := s.seal("spaces", id, "name_sealed", name)
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM spaces WHERE (owner = ?)`, p.Subject).Scan(&n); err != nil {
			return err
		}
		if n >= s.cfg.Limits.MaxSpacesPerOwner {
			return ErrQuota
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO spaces (id, name_sealed, owner, created_at) VALUES (?,?,?,?)`, id, sealed.String, p.Subject, ts(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO space_members (space_id, principal, role, added_by, added_at) VALUES (?,?,?,?,?)`, id, "user|"+p.Subject, "manager", p.Subject, ts(now)); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "space.create", "space:"+id, []string{"name"}, "ok")
	})
	if err != nil {
		return nil, err
	}
	return &Space{ID: id, Name: name, Owner: p.Subject, Role: "manager", CreatedAt: now.UTC()}, nil
}

func (s *Store) spaceView(id string, nameSealed sql.NullString, owner string, created int64, role Role) (*Space, error) {
	name, err := s.unseal("spaces", id, "name_sealed", nameSealed)
	if err != nil {
		return nil, err
	}
	sp := &Space{ID: id, Name: name, Role: role.String(), CreatedAt: fromUnix(created)}
	if role >= RoleManager {
		sp.Owner = owner
	}
	return sp, nil
}

// GetSpace returns a space the caller can see.
func (s *Store) GetSpace(ctx context.Context, p *Principal, id string) (*Space, error) {
	if !p.Has(PermRead) {
		return nil, ErrForbidden
	}
	if !validID("spc", id) {
		return nil, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	role, err := s.roleIn(ctx, s.db, id, p)
	if err != nil {
		return nil, err
	}
	if role == RoleNone {
		return nil, ErrNotFound
	}
	var name sql.NullString
	var owner string
	var created int64
	if err := s.db.QueryRowContext(ctx, `SELECT name_sealed, owner, created_at FROM spaces WHERE (id = ?)`, id).Scan(&name, &owner, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.spaceView(id, name, owner, created, role)
}

// ListSpaces lists the spaces the caller has a role in.
func (s *Store) ListSpaces(ctx context.Context, p *Principal, cursor string, limit int) (Page[Space], error) {
	out := Page[Space]{Items: []Space{}}
	if !p.Has(PermRead) {
		return out, ErrForbidden
	}
	if cursor != "" && !validID("spc", cursor) {
		return out, invalid("bad cursor")
	}
	limit = clampLimit(limit)
	keys := p.memberKeys()
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT sp.id, sp.name_sealed, sp.owner, sp.created_at, m.r FROM spaces sp
		JOIN (SELECT space_id, MAX(CASE role WHEN 'manager' THEN 3 WHEN 'editor' THEN 2 ELSE 1 END) AS r FROM space_members WHERE (principal IN (`+placeholders(len(keys))+`)) GROUP BY space_id) m ON (m.space_id = sp.id)
		WHERE (sp.id > ?) ORDER BY sp.id LIMIT ?`, append(anyArgs(keys), cursor, limit+1)...)
	if err != nil {
		return out, err
	}
	type row struct {
		id, owner string
		name      sql.NullString
		created   int64
		role      int
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.owner, &r.created, &r.role); err != nil {
			_ = rows.Close()
			return out, err
		}
		rs = append(rs, r)
	}
	_ = rows.Close()
	if len(rs) > limit {
		rs = rs[:limit]
		out.Next = rs[limit-1].id
	}
	for _, r := range rs {
		sp, err := s.spaceView(r.id, r.name, r.owner, r.created, Role(r.role))
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, *sp)
	}
	return out, nil
}

// ---- members -------------------------------------------------------------------------------------

func memberKey(kind, id string) (string, error) {
	switch kind {
	case "user":
		if !userRe.MatchString(id) {
			return "", invalid("a user is `user:<name>`")
		}
	case "group":
		if !groupRe.MatchString(id) {
			return "", invalid("a group is an opaque directory group id")
		}
	default:
		return "", invalid("kind is user or group")
	}
	return kind + "|" + id, nil
}

// ListMembers lists a space's access list. Managers only.
func (s *Store) ListMembers(ctx context.Context, p *Principal, spaceID string) ([]Member, error) {
	if !p.Has(PermRead) {
		return nil, ErrForbidden
	}
	if !validID("spc", spaceID) {
		return nil, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if _, err := s.need(ctx, s.db, p, spaceID, RoleManager, "members.list"); err != nil {
		return nil, s.forbid(ctx, p, "members.list", "space:"+spaceID, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT principal, role FROM space_members WHERE (space_id = ?) ORDER BY principal LIMIT 1000`, spaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Member{}
	for rows.Next() {
		var pr, role string
		if err := rows.Scan(&pr, &role); err != nil {
			return nil, err
		}
		kind, id, _ := strings.Cut(pr, "|")
		out = append(out, Member{Kind: kind, ID: id, Role: role})
	}
	return out, rows.Err()
}

// SetMember grants or changes a role. Managers only; the last manager cannot be demoted.
func (s *Store) SetMember(ctx context.Context, p *Principal, spaceID string, m Member) error {
	if !p.Has(PermWrite) {
		return ErrForbidden
	}
	if !validID("spc", spaceID) {
		return ErrNotFound
	}
	key, err := memberKey(m.Kind, m.ID)
	if err != nil {
		return err
	}
	if _, ok := roleNames[m.Role]; !ok {
		return invalid("role is viewer, editor or manager")
	}
	return s.txFor(ctx, p, "member.set", "space:"+spaceID, func(tx *sql.Tx) error {
		if _, err := s.need(ctx, tx, p, spaceID, RoleManager, "member.set"); err != nil {
			return err
		}
		var cur sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT role FROM space_members WHERE (space_id = ?) AND (principal = ?)`, spaceID, key).Scan(&cur); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if cur.String == "manager" && m.Role != "manager" {
			if err := s.keepAManager(ctx, tx, spaceID); err != nil {
				return err
			}
		}
		if !cur.Valid {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM space_members WHERE (space_id = ?)`, spaceID).Scan(&n); err != nil {
				return err
			}
			if n >= s.cfg.Limits.MaxMembersPerSpace {
				return ErrQuota
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO space_members (space_id, principal, role, added_by, added_at) VALUES (?,?,?,?,?)
			ON CONFLICT (space_id, principal) DO UPDATE SET role = excluded.role, added_by = excluded.added_by, added_at = excluded.added_at`,
			spaceID, key, m.Role, p.Subject, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "member.set", "space:"+spaceID, []string{"member:" + m.Kind, "role:" + m.Role}, "ok")
	})
}

// keepAManager refuses to remove the only manager of a space.
func (s *Store) keepAManager(ctx context.Context, tx dbq, spaceID string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM space_members WHERE (space_id = ?) AND (role = 'manager')`, spaceID).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return conflict("a space needs at least one manager")
	}
	return nil
}

// RemoveMember withdraws a grant. Managers only; the last manager stays.
func (s *Store) RemoveMember(ctx context.Context, p *Principal, spaceID, kind, id string) error {
	if !p.Has(PermWrite) {
		return ErrForbidden
	}
	if !validID("spc", spaceID) {
		return ErrNotFound
	}
	key, err := memberKey(kind, id)
	if err != nil {
		return err
	}
	return s.txFor(ctx, p, "member.remove", "space:"+spaceID, func(tx *sql.Tx) error {
		if _, err := s.need(ctx, tx, p, spaceID, RoleManager, "member.remove"); err != nil {
			return err
		}
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM space_members WHERE (space_id = ?) AND (principal = ?)`, spaceID, key).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if cur == "manager" {
			if err := s.keepAManager(ctx, tx, spaceID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM space_members WHERE (space_id = ?) AND (principal = ?)`, spaceID, key); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "member.remove", "space:"+spaceID, []string{"member:" + kind}, "ok")
	})
}

// ---- folders -------------------------------------------------------------------------------------

// CreateFolder adds a folder (parent "" for the top level). Editors and managers.
func (s *Store) CreateFolder(ctx context.Context, p *Principal, spaceID, parentID, name string) (*Folder, error) {
	if !p.Has(PermWrite) {
		return nil, ErrForbidden
	}
	if !validID("spc", spaceID) {
		return nil, ErrNotFound
	}
	name, err := cleanLabel(SafeFilename(name), 100)
	if err != nil || name == "" {
		return nil, invalid("a folder needs a name of 1 to 100 characters")
	}
	if parentID != "" && !validID("fld", parentID) {
		return nil, invalid("bad parent folder")
	}
	id, now := newID("fld"), s.now()
	sealed, err := s.seal("folders", id, "name_sealed", name)
	if err != nil {
		return nil, err
	}
	err = s.txFor(ctx, p, "folder.create", "space:"+spaceID, func(tx *sql.Tx) error {
		if _, err := s.need(ctx, tx, p, spaceID, RoleEditor, "folder.create"); err != nil {
			return err
		}
		depth := 1
		if parentID != "" {
			var pd int
			if err := tx.QueryRowContext(ctx, `SELECT depth FROM folders WHERE (id = ?) AND (space_id = ?)`, parentID, spaceID).Scan(&pd); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return invalid("the parent folder is not in this space")
				}
				return err
			}
			depth = pd + 1
		}
		if depth > s.cfg.Limits.MaxFolderDepth {
			return invalid("folders can be nested %d deep at most", s.cfg.Limits.MaxFolderDepth)
		}
		var parent any
		if parentID != "" {
			parent = parentID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folders (id, space_id, parent_id, depth, name_sealed, created_by, created_at) VALUES (?,?,?,?,?,?,?)`,
			id, spaceID, parent, depth, sealed.String, p.Subject, ts(now)); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "folder.create", "folder:"+id, []string{"name"}, "ok")
	})
	if err != nil {
		return nil, err
	}
	return &Folder{ID: id, SpaceID: spaceID, ParentID: parentID, Name: name, CreatedAt: now.UTC()}, nil
}

// ListFolders lists every folder of a space (bounded).
func (s *Store) ListFolders(ctx context.Context, p *Principal, spaceID string) ([]Folder, error) {
	if !p.Has(PermRead) {
		return nil, ErrForbidden
	}
	if !validID("spc", spaceID) {
		return nil, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if _, err := s.need(ctx, s.db, p, spaceID, RoleViewer, "folder.list"); err != nil {
		return nil, s.forbid(ctx, p, "folder.list", "space:"+spaceID, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, parent_id, name_sealed, created_at FROM folders WHERE (space_id = ?) ORDER BY id LIMIT 5000`, spaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Folder{}
	for rows.Next() {
		var f Folder
		var parent, sealed sql.NullString
		var created int64
		if err := rows.Scan(&f.ID, &parent, &sealed, &created); err != nil {
			return nil, err
		}
		if f.Name, err = s.unseal("folders", f.ID, "name_sealed", sealed); err != nil {
			return nil, err
		}
		f.SpaceID, f.ParentID, f.CreatedAt = spaceID, parent.String, fromUnix(created)
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteFolder removes an empty folder. A folder that still holds a file, deleted ones included,
// or another folder is a conflict.
func (s *Store) DeleteFolder(ctx context.Context, p *Principal, id string) error {
	if !p.Has(PermWrite) {
		return ErrForbidden
	}
	if !validID("fld", id) {
		return ErrNotFound
	}
	return s.txFor(ctx, p, "folder.delete", "folder:"+id, func(tx *sql.Tx) error {
		var space string
		if err := tx.QueryRowContext(ctx, `SELECT space_id FROM folders WHERE (id = ?)`, id).Scan(&space); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := s.need(ctx, tx, p, space, RoleEditor, "folder.delete"); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM folders WHERE (parent_id = ?)) + (SELECT COUNT(*) FROM files WHERE (folder_id = ?) AND (status != 'purged'))`, id, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return conflict("the folder is not empty")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET folder_id = NULL WHERE (folder_id = ?)`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE (id = ?)`, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "folder.delete", "folder:"+id, nil, "ok")
	})
}
