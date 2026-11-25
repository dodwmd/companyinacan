-- E13d QA: revoking the access of someone who left also disables their administrator account, so a
-- new session cannot be started with it. disabled_at is 0 for an enabled account.
ALTER TABLE admin_users ADD COLUMN disabled_at INTEGER NOT NULL DEFAULT 0;
