-- Interviewer assignments are soft-removed so "has ever been assigned" survives removal:
-- blind feedback must not be bypassable by unassigning yourself, reading, and re-assigning.
ALTER TABLE interviewers ADD COLUMN removed_at INTEGER;

-- Diversity answers become pre-aggregated counters. The first design stored one row per
-- response with an autoincrement id, so row order tracked application order and could be
-- correlated with candidates by anyone holding the file or a backup. Counters carry no order,
-- no per-response row and nothing finer than a month. WITHOUT ROWID so even the file layout has
-- no insertion sequence for new values.
--
-- The old per-response rows are DISCARDED, not migrated into counters: they were the linkable
-- data, and any carry-over would have to read them row by row. Historic diversity numbers from
-- before this migration are therefore gone; an operator who needs them must export them from a
-- backup before upgrading. The store runs VACUUM and a WAL checkpoint(TRUNCATE) straight after
-- this migration commits (they cannot run inside it), so the dropped pages do not linger.
DROP TABLE eeo_responses;
CREATE TABLE eeo_counts (
    month TEXT NOT NULL,
    field TEXT NOT NULL,
    value TEXT NOT NULL,
    n     INTEGER NOT NULL,
    PRIMARY KEY (month, field, value)
) WITHOUT ROWID;
