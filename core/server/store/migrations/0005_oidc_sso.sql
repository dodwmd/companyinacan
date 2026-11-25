-- OpenID Connect single sign-on (epic E11b, specs/021-oidc-sso).

-- Where an admin user authenticates: 'local' (password and TOTP) or 'sso'. An SSO user has no
-- password ('!' never verifies) and no TOTP secret.
ALTER TABLE admin_users ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'local' CHECK (auth_source IN ('local','sso'));

-- Sessions remember how they were created; an SSO session with revalidate_at > 0 stops being
-- accepted at that time so that sign-in is repeated at the identity provider.
ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT 'local';
ALTER TABLE sessions ADD COLUMN revalidate_at INTEGER NOT NULL DEFAULT 0;

-- External identities. The key is (issuer_hash, subject): the issuer's sha256 (hex, first 16
-- characters) and the `sub` claim. Email is informational and is never used to find a user.
CREATE TABLE sso_identities (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  issuer_hash TEXT NOT NULL,
  subject     TEXT NOT NULL,
  user_id     INTEGER NOT NULL UNIQUE REFERENCES admin_users(id) ON DELETE CASCADE,
  email       TEXT NOT NULL DEFAULT '',
  name        TEXT NOT NULL DEFAULT '',
  groups_json TEXT NOT NULL DEFAULT '[]',
  amr_json    TEXT NOT NULL DEFAULT '[]',
  created_at  INTEGER NOT NULL,
  last_login  INTEGER NOT NULL,
  linked_by   TEXT NOT NULL DEFAULT '',
  UNIQUE (issuer_hash, subject)
);
