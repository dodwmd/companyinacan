-- Notices of the approval engine are marked, so that their limits are counted apart whatever type a
-- module's notice carries (specs/060-reminders). Earlier approval notices carry the engine's types and
-- were never authored by a module.
ALTER TABLE notifications ADD COLUMN platform INTEGER NOT NULL DEFAULT 0;
UPDATE notifications SET platform = 1 WHERE type IN ('approval.requested', 'approval.escalated', 'approval.decided') AND authored = 0;
