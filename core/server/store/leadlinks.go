package store

import "context"

// LeadLink is what an adapter made from one lead (specs/067-lead-adapters). Ids only.
type LeadLink struct {
	ModuleID, LeadID string
	// Ref is the application's own id of the record, "" while it is not known.
	Ref  string
	Done bool
}

// GetLeadLink returns the link of a module and lead; ErrNotFound without one.
func (db *DB) GetLeadLink(ctx context.Context, module, lead string) (LeadLink, error) {
	l := LeadLink{ModuleID: module, LeadID: lead}
	err := db.QueryRowContext(ctx, `SELECT ref, done FROM lead_links WHERE module_id=? AND lead_id=?`, module, lead).Scan(&l.Ref, &l.Done)
	return l, notFound(err)
}

// PutLeadLink inserts or updates a link.
func (db *DB) PutLeadLink(ctx context.Context, l LeadLink) error {
	_, err := db.ExecContext(ctx, `INSERT INTO lead_links(module_id,lead_id,ref,done,created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(module_id,lead_id) DO UPDATE SET ref=excluded.ref, done=excluded.done`, l.ModuleID, l.LeadID, l.Ref, l.Done, db.Now().Unix())
	return err
}

// DeleteLeadLink forgets one link.
func (db *DB) DeleteLeadLink(ctx context.Context, module, lead string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM lead_links WHERE module_id=? AND lead_id=?`, module, lead)
	return err
}

// DeleteLeadLinks forgets every link of a module whose data was purged.
func (db *DB) DeleteLeadLinks(ctx context.Context, module string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM lead_links WHERE module_id=?`, module)
	return err
}

// LeadLinkModules are the modules that hold something made from a lead.
func (db *DB) LeadLinkModules(ctx context.Context, lead string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT module_id FROM lead_links WHERE lead_id=? ORDER BY module_id`, lead)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// OtherLeadsOfRef counts the other leads that led to the same record of a module.
func (db *DB) OtherLeadsOfRef(ctx context.Context, module, ref, lead string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lead_links WHERE module_id=? AND ref=? AND ref<>'' AND lead_id<>?`, module, ref, lead).Scan(&n)
	return n, err
}
