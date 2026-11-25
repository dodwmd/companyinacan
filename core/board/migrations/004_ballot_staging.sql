-- Secret ballots are STAGED while voting is open and written to `ballots` and `vote_receipts` only when voting
-- closes, in a cryptographically shuffled order (crypto/rand Fisher-Yates), after which the staging rows are deleted
-- and the database is vacuumed so the staging order does not survive in free pages. While voting is open the two
-- staging tables still exist: someone with live access to the database file during an open vote can see them
-- (docs/board.md, residual risks). They carry no timestamps; the audit trail carries no per-vote row for a secret ballot.

CREATE TABLE ballot_staging (
    id            TEXT PRIMARY KEY,
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    sens_choice   TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE receipt_staging (
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    voter_key     TEXT NOT NULL,
    person_id     TEXT NOT NULL,
    PRIMARY KEY (resolution_id, voter_key)
) WITHOUT ROWID;

CREATE TRIGGER ballot_staging_only_while_voting BEFORE INSERT ON ballot_staging
WHEN (SELECT status FROM resolutions WHERE id = NEW.resolution_id) != 'voting'
BEGIN SELECT RAISE(ABORT, 'ballots can only be cast while voting is open'); END;
CREATE TRIGGER receipt_staging_only_while_voting BEFORE INSERT ON receipt_staging
WHEN (SELECT status FROM resolutions WHERE id = NEW.resolution_id) != 'voting'
BEGIN SELECT RAISE(ABORT, 'votes can only be cast while voting is open'); END;
CREATE TRIGGER ballot_staging_no_update BEFORE UPDATE ON ballot_staging
WHEN NEW.id != OLD.id OR NEW.resolution_id != OLD.resolution_id
BEGIN SELECT RAISE(ABORT, 'ballots are not changed'); END;
CREATE TRIGGER receipt_staging_no_update BEFORE UPDATE ON receipt_staging
BEGIN SELECT RAISE(ABORT, 'vote receipts are not changed'); END;

-- Why a resolution was withdrawn while voting was open (sealed); the votes cast so far are finalised and kept.
CREATE TABLE resolution_withdrawals (
    resolution_id TEXT PRIMARY KEY REFERENCES resolutions (id),
    sens_reason   TEXT NOT NULL,
    withdrawn_by  TEXT NOT NULL,
    withdrawn_at  INTEGER NOT NULL
);
CREATE TRIGGER withdrawals_no_delete BEFORE DELETE ON resolution_withdrawals
BEGIN SELECT RAISE(ABORT, 'withdrawal reasons are kept'); END;
CREATE TRIGGER withdrawals_frozen BEFORE UPDATE ON resolution_withdrawals
WHEN NEW.resolution_id != OLD.resolution_id OR NEW.withdrawn_by != OLD.withdrawn_by OR NEW.withdrawn_at != OLD.withdrawn_at
BEGIN SELECT RAISE(ABORT, 'withdrawal reasons are append-only'); END;
