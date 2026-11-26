-- The account that submitted a request, as it is written, for the decision's notification
-- (specs/060-reminders). submitted_by stays case-folded: separation of duties compares it.
ALTER TABLE leave_requests ADD COLUMN submitted_account TEXT NOT NULL DEFAULT '';
