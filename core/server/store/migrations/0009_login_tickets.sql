-- E11c: single sign-on from the admin UI into the module web UIs (specs/036-module-ui-sso).

-- A launch ticket: minted for a signed-in administrator, bound to ONE module (the audience), valid
-- for at most 60 seconds and redeemable once. Only the SHA-256 of the ticket is stored.
CREATE TABLE login_tickets (
  id          TEXT PRIMARY KEY,
  token_hash  TEXT NOT NULL UNIQUE,
  subject     TEXT NOT NULL,
  module_id   TEXT NOT NULL,
  return_path TEXT NOT NULL DEFAULT '',
  -- SHA-256 of the nonce cookie set in the browser that launched: redeem needs it (login CSRF).
  binding_hash TEXT NOT NULL,
  -- Unix seconds the module session may not outlive (the identity provider revalidation of the
  -- launching admin session); 0 = no such limit.
  session_not_after INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  used_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX login_tickets_expires ON login_tickets(expires_at);

-- The server-side handle a module keeps for a session that began with a ticket, so it can ask "is
-- this person still allowed" without any long-lived credential. Only the hash is stored.
CREATE TABLE ui_launch_sessions (
  handle_hash TEXT PRIMARY KEY,
  ticket_id   TEXT NOT NULL,
  subject     TEXT NOT NULL,
  module_id   TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  revoked_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ui_launch_sessions_subject ON ui_launch_sessions(subject);
CREATE INDEX ui_launch_sessions_expires ON ui_launch_sessions(expires_at);
