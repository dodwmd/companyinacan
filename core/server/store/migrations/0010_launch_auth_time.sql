-- E11c follow-up (specs/036-module-ui-sso): when the administrator really authenticated, carried from
-- the admin session through the ticket to the module session, and a revocation mark on tickets that
-- also covers a ticket already consumed but whose module session is not stored yet.

-- auth_at: Unix seconds of the identity provider's auth_time for an SSO session; 0 = not stated. A
-- local session authenticated (password and TOTP) when it was created, so it does not use this.
ALTER TABLE sessions ADD COLUMN auth_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE login_tickets ADD COLUMN auth_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE login_tickets ADD COLUMN revoked_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ui_launch_sessions ADD COLUMN auth_at INTEGER NOT NULL DEFAULT 0;
