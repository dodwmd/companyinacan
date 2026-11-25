-- Waiting for something that is not the step's fault (can-core or the directory is down, the person
-- is not offboarded yet) is counted apart from attempts, so it backs off exponentially without ever
-- using up the attempts that decide a dead letter.
ALTER TABLE steps ADD COLUMN waits INTEGER NOT NULL DEFAULT 0;

-- A retried step must be able to ask for a FRESH approval: the approval request key carries gen,
-- which RetryRun increments.
ALTER TABLE steps ADD COLUMN gen INTEGER NOT NULL DEFAULT 0;
