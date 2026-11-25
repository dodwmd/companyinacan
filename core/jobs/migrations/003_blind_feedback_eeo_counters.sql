-- Interviewer assignments are soft-removed so "has ever been assigned" survives removal:
-- blind feedback must not be bypassable by unassigning yourself, reading, and re-assigning.
ALTER TABLE interviewers ADD COLUMN removed_at INTEGER;

-- Diversity answers become pre-aggregated counters. The first design stored one row per
-- response with an autoincrement id, so row order tracked application order and could be
-- correlated with candidates by anyone holding the file or a backup. Counters carry no order,
-- no per-response row and nothing finer than a month. WITHOUT ROWID so even the file layout has
-- no insertion sequence for new values.
DROP TABLE eeo_responses;
CREATE TABLE eeo_counts (
    month TEXT NOT NULL,
    field TEXT NOT NULL,
    value TEXT NOT NULL,
    n     INTEGER NOT NULL,
    PRIMARY KEY (month, field, value)
) WITHOUT ROWID;
