-- The worker must not be starved by runs that can only wait (a leaver whose last day is months away,
-- an approval nobody has answered yet): a run is selected only when it is due (wake_at <= now),
-- oldest examination first, and every pass records examined_at.
ALTER TABLE runs ADD COLUMN wake_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN examined_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX runs_due ON runs(state, wake_at, examined_at);

-- Any state change (an approval granted, a retry, a release for an urgent termination, a cancelled
-- offboarding) makes the run due at once.
CREATE TRIGGER runs_wake AFTER UPDATE OF state ON runs WHEN NEW.state <> OLD.state
BEGIN
  UPDATE runs SET wake_at = 0 WHERE id = NEW.id;
END;
