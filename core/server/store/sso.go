package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SSOIdentity is an external (OpenID Connect) identity bound to one admin user.
type SSOIdentity struct {
	ID         int64
	IssuerHash string
	Subject    string
	UserID     int64
	Username   string // of the bound admin user
	UserSource string // auth_source of the bound admin user: sso (provisioned) or local (linked by an admin)
	Email      string
	Name       string
	GroupsJSON string
	AMRJSON    string
	CreatedAt  time.Time
	LastLogin  time.Time
	LinkedBy   string
}

// ErrSSOConflict means the user already has an identity or the identity is already bound.
var ErrSSOConflict = errors.New("sso identity conflict")

const ssoCols = `i.id, i.issuer_hash, i.subject, i.user_id, u.username, u.auth_source, i.email, i.name, i.groups_json, i.amr_json, i.created_at, i.last_login, i.linked_by`

func scanSSO(row interface{ Scan(...any) error }) (SSOIdentity, error) {
	var i SSOIdentity
	var c, l int64
	err := row.Scan(&i.ID, &i.IssuerHash, &i.Subject, &i.UserID, &i.Username, &i.UserSource, &i.Email, &i.Name, &i.GroupsJSON, &i.AMRJSON, &c, &l, &i.LinkedBy)
	i.CreatedAt, i.LastLogin = time.Unix(c, 0), time.Unix(l, 0)
	return i, notFound(err)
}

// SSOIdentityByExternal finds the identity for an issuer and subject.
func (db *DB) SSOIdentityByExternal(ctx context.Context, issuerHash, subject string) (SSOIdentity, error) {
	return scanSSO(db.QueryRowContext(ctx, `SELECT `+ssoCols+` FROM sso_identities i JOIN admin_users u ON u.id=i.user_id WHERE i.issuer_hash=? AND i.subject=?`, issuerHash, subject))
}

// SSOIdentityByUser finds the identity bound to an admin user.
func (db *DB) SSOIdentityByUser(ctx context.Context, userID int64) (SSOIdentity, error) {
	return scanSSO(db.QueryRowContext(ctx, `SELECT `+ssoCols+` FROM sso_identities i JOIN admin_users u ON u.id=i.user_id WHERE i.user_id=?`, userID))
}

// SSOIdentityByID finds an identity by its row id.
func (db *DB) SSOIdentityByID(ctx context.Context, id int64) (SSOIdentity, error) {
	return scanSSO(db.QueryRowContext(ctx, `SELECT `+ssoCols+` FROM sso_identities i JOIN admin_users u ON u.id=i.user_id WHERE i.id=?`, id))
}

// ListSSOIdentities returns up to limit identities, newest login first.
func (db *DB) ListSSOIdentities(ctx context.Context, limit int) ([]SSOIdentity, error) {
	if limit < 1 || limit > 500 {
		limit = 500
	}
	rows, err := db.QueryContext(ctx, `SELECT `+ssoCols+` FROM sso_identities i JOIN admin_users u ON u.id=i.user_id ORDER BY i.last_login DESC, i.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SSOIdentity
	for rows.Next() {
		i, err := scanSSO(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// CreateSSOUser inserts a just-in-time provisioned user (no password, no TOTP, no roles) and its
// identity in one transaction. audit runs in the same transaction; its error aborts the creation.
func (db *DB) CreateSSOUser(ctx context.Context, username string, id SSOIdentity, audit func(tx *sql.Tx) error) (SSOIdentity, error) {
	now := db.Now()
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO admin_users(username, password_hash, totp_secret, created_at, auth_source) VALUES (?,?,?,?,?)`, username, "!", "", now.Unix(), SourceSSO)
		if err != nil {
			return err
		}
		uid, _ := res.LastInsertId()
		res, err = tx.ExecContext(ctx, `INSERT INTO sso_identities(issuer_hash, subject, user_id, email, name, groups_json, amr_json, created_at, last_login) VALUES (?,?,?,?,?,?,?,?,?)`,
			id.IssuerHash, id.Subject, uid, id.Email, id.Name, id.GroupsJSON, id.AMRJSON, now.Unix(), now.Unix())
		if err != nil {
			return err
		}
		id.ID, _ = res.LastInsertId()
		id.UserID = uid
		if audit != nil {
			return audit(tx)
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return SSOIdentity{}, ErrSSOConflict
		}
		return SSOIdentity{}, err
	}
	id.Username, id.UserSource, id.CreatedAt, id.LastLogin = username, SourceSSO, now, now
	return id, nil
}

// TouchSSOIdentity records the claims of a successful sign-in.
func (db *DB) TouchSSOIdentity(ctx context.Context, id int64, email, name, groupsJSON, amrJSON string) error {
	_, err := db.ExecContext(ctx, `UPDATE sso_identities SET email=?, name=?, groups_json=?, amr_json=?, last_login=? WHERE id=?`, email, name, groupsJSON, amrJSON, db.Now().Unix(), id)
	return err
}

// LinkSSOIdentity binds an identity to an existing local admin user (an explicit administrator
// action). The user the identity was provisioned for is deleted with its sessions and the roles the
// sign-in sync gave it. audit runs in the same transaction.
func (db *DB) LinkSSOIdentity(ctx context.Context, identityID, targetUserID int64, linkedBy string, audit func(tx *sql.Tx, from SSOIdentity, target AdminUser) error) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		from, err := scanSSO(tx.QueryRowContext(ctx, `SELECT `+ssoCols+` FROM sso_identities i JOIN admin_users u ON u.id=i.user_id WHERE i.id=?`, identityID))
		if err != nil {
			return err
		}
		target, err := scanAdmin(tx.QueryRowContext(ctx, `SELECT id, username, password_hash, totp_secret, created_at, auth_source FROM admin_users WHERE id=?`, targetUserID))
		if err != nil {
			return err
		}
		if target.AuthSource != SourceLocal || from.UserID == target.ID {
			return ErrSSOConflict
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sso_identities WHERE user_id=?`, target.ID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSSOConflict
		}
		if audit != nil {
			if err := audit(tx, from, target); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sso_identities SET user_id=?, linked_by=? WHERE id=?`, target.ID, linkedBy, identityID); err != nil {
			return err
		}
		if from.UserSource == SourceSSO {
			if err := revokeSubjectTokens(ctx, tx, "user:"+from.Username, db.Now()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM rbac_assignments WHERE subject=?`, "user:"+from.Username); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM admin_users WHERE id=? AND auth_source='sso'`, from.UserID); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnlinkSSOIdentity removes an identity. A provisioned (sso) user is deleted with its sessions and
// role assignments; a local user stays and only loses the link. audit runs in the same transaction.
func (db *DB) UnlinkSSOIdentity(ctx context.Context, identityID int64, audit func(tx *sql.Tx, id SSOIdentity) error) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		id, err := scanSSO(tx.QueryRowContext(ctx, `SELECT `+ssoCols+` FROM sso_identities i JOIN admin_users u ON u.id=i.user_id WHERE i.id=?`, identityID))
		if err != nil {
			return err
		}
		if audit != nil {
			if err := audit(tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sso_identities WHERE id=?`, identityID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND source='sso'`, id.UserID); err != nil {
			return err
		}
		if id.UserSource == SourceSSO {
			if err := revokeSubjectTokens(ctx, tx, "user:"+id.Username, db.Now()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM rbac_assignments WHERE subject=?`, "user:"+id.Username); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM admin_users WHERE id=? AND auth_source='sso'`, id.UserID)
		}
		return err
	})
}

// revokeSubjectTokens revokes the personal access tokens a user issued. The username of a provisioned
// user is derived from (issuer, subject), so the same person signing in again gets the same name: without
// this their old tokens would come back to life with the new account.
func revokeSubjectTokens(ctx context.Context, tx *sql.Tx, subject string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE subject=? AND revoked_at=0`, now.Unix(), subject); err != nil {
		return err
	}
	// And to module sessions that began with a launch ticket (E11c).
	if _, err := tx.ExecContext(ctx, `UPDATE ui_launch_sessions SET revoked_at=? WHERE subject=? AND revoked_at=0`, now.Unix(), subject); err != nil {
		return err
	}
	// The same applies to Web Push devices (E19a): they belong to the account being deleted.
	_, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE subject=?`, subject)
	return err
}

// SSOOwnerCount counts SSO-provisioned or linked users who hold an unscoped owner role.
func (db *DB) SSOOwnerCount(ctx context.Context) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sso_identities i JOIN admin_users u ON u.id=i.user_id JOIN rbac_assignments a ON a.subject='user:'||u.username WHERE a.role='owner' AND a.scope=''`).Scan(&n)
	return n, err
}
