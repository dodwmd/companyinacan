-- E11e: users and roles administration (specs/044-users-roles).

-- Where an invitation or a new set-up link is mailed to. Optional; erased when the account is deleted.
ALTER TABLE admin_users ADD COLUMN email TEXT NOT NULL DEFAULT '';
-- The administrator who stored that address. It is shown with the address, and an address somebody
-- else stored is forgotten when the account gains a role.
ALTER TABLE admin_users ADD COLUMN email_set_by TEXT NOT NULL DEFAULT '';

-- When the person last confirmed their password and a code again in this session (step-up).
ALTER TABLE sessions ADD COLUMN reauth_at INTEGER NOT NULL DEFAULT 0;

-- One pending one-time set-up link per account. Only the SHA-256 of the token is stored. totp_secret
-- is the secret being enrolled; it moves to the account when the link is used.
-- purpose: invite (new account) and reset (password and code) set both; mfa keeps the password, which
-- must be typed again, and sets only the code. failures counts the password attempts taken on an mfa
-- link, each before it is checked.
CREATE TABLE user_invites (
  user_id     INTEGER PRIMARY KEY REFERENCES admin_users(id) ON DELETE CASCADE,
  token_hash  TEXT NOT NULL UNIQUE,
  totp_secret TEXT NOT NULL,
  purpose     TEXT NOT NULL CHECK (purpose IN ('invite','reset','mfa')),
  created_by  TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  failures    INTEGER NOT NULL DEFAULT 0
);

-- Names of deleted accounts. A name is never given to a new account: module data, approvals and the
-- audit log know people as user:<name>, and a new person must not inherit them.
CREATE TABLE deleted_users (
  username   TEXT PRIMARY KEY COLLATE NOCASE,
  deleted_at INTEGER NOT NULL
);
