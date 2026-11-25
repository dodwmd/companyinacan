-- Epic E14b: performance reviews and time off. Timestamps are unix seconds (UTC); dates are
-- YYYY-MM-DD text. Columns named *_sealed hold XChaCha20-Poly1305 ciphertext bound to the row
-- (AAD), never plaintext. Applies on top of an existing 001/002 database: nothing is altered.

-- ---- reviews (data class performance-reviews) ------------------------------------------------

-- Templates and cycle shells hold no personal data (questions are generic).
CREATE TABLE review_templates (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    questions  TEXT NOT NULL,                 -- JSON array of questions
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE review_cycles (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    state          TEXT NOT NULL CHECK (state IN ('draft', 'open', 'calibration', 'closed')),
    template_id    TEXT REFERENCES review_templates (id),
    questions      TEXT NOT NULL,             -- snapshot of the template when the cycle was created
    anonymity      TEXT NOT NULL CHECK (anonymity IN ('anonymous', 'attributed')),
    min_responses  INTEGER NOT NULL CHECK (min_responses >= 3),
    rating_max     INTEGER NOT NULL CHECK (rating_max BETWEEN 3 AND 10),
    due_date       TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL,
    opened_at      INTEGER,
    calibration_at INTEGER,
    closed_at      INTEGER,
    created_at     INTEGER NOT NULL
);

-- One row per person in a cycle: the review itself. All free text and every rating is sealed.
CREATE TABLE review_participants (
    id                   TEXT PRIMARY KEY,
    cycle_id             TEXT NOT NULL REFERENCES review_cycles (id) ON DELETE CASCADE,
    person_id            TEXT NOT NULL REFERENCES people (id),
    self_sealed          TEXT,
    self_submitted_at    INTEGER,
    mgr_sealed           TEXT,                 -- manager answers, rating and summary (one JSON blob)
    mgr_author           TEXT,                 -- person id of the manager who wrote it
    mgr_submitted_at     INTEGER,
    calib_sealed         TEXT,                 -- calibrated rating and calibration notes (HR only)
    calibrated_by        TEXT,
    final_sealed         TEXT,                 -- frozen outcome at release request: rating and summary
    release_state        TEXT NOT NULL DEFAULT 'none' CHECK (release_state IN ('none', 'requested', 'released')),
    release_requested_by TEXT,
    release_decided_by   TEXT,
    approval_ref         TEXT NOT NULL DEFAULT '',
    release_note_sealed  TEXT,                 -- why a release was rejected (hr only)
    released_at          INTEGER,
    ack_at               INTEGER,
    ack_sealed           TEXT,
    touched_by           TEXT NOT NULL DEFAULT '[]', -- JSON: sha256 of every identity that wrote or requested (separation of duties; never shrinks, holds no names)
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    UNIQUE (cycle_id, person_id)
);
CREATE INDEX review_participants_person ON review_participants (person_id);

-- Peer and upward feedback requests. proposed_by is never shown to the nominee.
CREATE TABLE review_requests (
    id             TEXT PRIMARY KEY,
    participant_id TEXT NOT NULL REFERENCES review_participants (id) ON DELETE CASCADE,
    reviewer_id    TEXT NOT NULL REFERENCES people (id),
    kind           TEXT NOT NULL CHECK (kind IN ('peer', 'upward')),
    status         TEXT NOT NULL CHECK (status IN ('proposed', 'approved', 'declined', 'submitted')),
    proposed_by    TEXT NOT NULL,
    decided_by     TEXT,
    created_at     INTEGER NOT NULL,
    UNIQUE (participant_id, reviewer_id, kind)
);
CREATE INDEX review_requests_reviewer ON review_requests (reviewer_id);

-- Submitted feedback. No timestamp, and no reviewer in anonymous cycles: the answer cannot be
-- tied to its author through this table (ids are random, so row order tells nothing either).
CREATE TABLE review_responses (
    id             TEXT PRIMARY KEY,
    participant_id TEXT NOT NULL REFERENCES review_participants (id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('peer', 'upward')),
    reviewer_id    TEXT REFERENCES people (id),   -- NULL when the cycle is anonymous
    answers_sealed TEXT NOT NULL
);
CREATE INDEX review_responses_participant ON review_responses (participant_id);

-- ---- time off (data class people-timeoff) ----------------------------------------------------

-- Leave types and their accrual policy are data: the operator models local entitlements. Units
-- are half days ("halves"): 2 halves = 1 day.
CREATE TABLE leave_types (
    id                      TEXT PRIMARY KEY,
    key                     TEXT NOT NULL UNIQUE,
    name                    TEXT NOT NULL,
    paid                    INTEGER NOT NULL DEFAULT 1,
    sensitive               INTEGER NOT NULL DEFAULT 0,   -- the type and the reason are shown only to the person, their manager and HR
    accrual_mode            TEXT NOT NULL DEFAULT 'none' CHECK (accrual_mode IN ('none', 'annual', 'monthly')),
    grant_halves            INTEGER NOT NULL DEFAULT 0 CHECK (grant_halves >= 0),  -- per year (annual) or per month (monthly)
    carryover_cap_halves    INTEGER NOT NULL DEFAULT -1 CHECK (carryover_cap_halves >= -1), -- -1 unlimited
    allow_negative          INTEGER NOT NULL DEFAULT 0,
    negative_floor_halves   INTEGER NOT NULL DEFAULT 0 CHECK (negative_floor_halves >= 0),
    ignores_blackouts       INTEGER NOT NULL DEFAULT 0,
    sets_leave_status       INTEGER NOT NULL DEFAULT 0,   -- long leave moves the person to the leave employment status
    leave_status_min_halves INTEGER NOT NULL DEFAULT 0,
    active                  INTEGER NOT NULL DEFAULT 1,
    created_at              INTEGER NOT NULL
);

-- Public holiday calendar per location (matched to people.location, case-folded) and work week.
CREATE TABLE holiday_calendars (
    id         TEXT PRIMARY KEY,
    location   TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    work_days  INTEGER NOT NULL DEFAULT 31 CHECK (work_days BETWEEN 1 AND 127), -- bit 0 Monday .. bit 6 Sunday
    created_at INTEGER NOT NULL
);
CREATE TABLE holidays (
    calendar_id TEXT NOT NULL REFERENCES holiday_calendars (id) ON DELETE CASCADE,
    date        TEXT NOT NULL,
    name        TEXT NOT NULL,
    PRIMARY KEY (calendar_id, date)
);

CREATE TABLE blackouts (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    start_date TEXT NOT NULL,
    end_date   TEXT NOT NULL,
    team_id    TEXT REFERENCES teams (id),
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE leave_requests (
    id             TEXT PRIMARY KEY,
    person_id      TEXT NOT NULL REFERENCES people (id),
    type_id        TEXT NOT NULL REFERENCES leave_types (id),
    status         TEXT NOT NULL CHECK (status IN ('draft', 'pending', 'approved', 'rejected', 'cancelled')),
    start_date     TEXT NOT NULL,
    end_date       TEXT NOT NULL,
    start_part     TEXT NOT NULL DEFAULT 'full' CHECK (start_part IN ('full', 'pm')),
    end_part       TEXT NOT NULL DEFAULT 'full' CHECK (end_part IN ('full', 'am')),
    start_slot     INTEGER NOT NULL,           -- day number * 2 + 0 (am) or 1 (pm): overlap tests are on slots
    end_slot       INTEGER NOT NULL,
    halves         INTEGER NOT NULL DEFAULT 0, -- working half days, computed when submitted
    note_sealed    TEXT,
    approval_ref   TEXT NOT NULL DEFAULT '',
    submitted_by   TEXT NOT NULL DEFAULT '',
    decided_by     TEXT NOT NULL DEFAULT '',
    decided_at     INTEGER,
    cancelled_by   TEXT NOT NULL DEFAULT '',
    cancelled_at   INTEGER,
    status_applied INTEGER NOT NULL DEFAULT 0, -- 1: this request moved the person to the leave status; 2: and back
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE INDEX leave_requests_person ON leave_requests (person_id, start_slot);
CREATE INDEX leave_requests_range ON leave_requests (status, start_slot, end_slot);

-- Append-only balance ledger. Entries are never edited: a correction is a new entry.
CREATE TABLE leave_ledger (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id  TEXT NOT NULL REFERENCES people (id),
    type_id    TEXT NOT NULL REFERENCES leave_types (id),
    entry_date TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('grant', 'accrual', 'expire', 'use', 'reversal', 'adjust')),
    halves     INTEGER NOT NULL,
    ref        TEXT NOT NULL,                 -- request id, period key or adjustment id
    note_sealed TEXT,                         -- reason of a manual adjustment
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (person_id, type_id, kind, ref)    -- idempotent accruals, one use and one reversal per request
);
CREATE INDEX leave_ledger_person ON leave_ledger (person_id, type_id, entry_date);

CREATE TABLE ledger_guard (id INTEGER PRIMARY KEY CHECK (id = 1), open INTEGER NOT NULL);
INSERT INTO ledger_guard (id, open) VALUES (1, 0);

-- created_by alone may be rewritten (an erasure replaces the actor with 'erased').
CREATE TRIGGER leave_ledger_no_update BEFORE UPDATE OF person_id, type_id, entry_date, kind, halves, ref, created_at ON leave_ledger
BEGIN
    SELECT RAISE(ABORT, 'leave_ledger is append-only');
END;

-- Deleting is possible only while the guard is open, which the service does in the same
-- transaction as an erasure or a retention purge and nowhere else.
CREATE TRIGGER leave_ledger_no_delete BEFORE DELETE ON leave_ledger
WHEN (SELECT open FROM ledger_guard WHERE id = 1) = 0
BEGIN
    SELECT RAISE(ABORT, 'leave_ledger is append-only');
END;
