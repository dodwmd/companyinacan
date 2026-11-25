-- entity module schema: company records. Registration numbers, tax ids and personal details are sealed
-- (sens_*, XChaCha20-Poly1305 bound to row and column). Structural facts (ids, dates, offices, counts) are
-- plain. No jurisdiction rules are implemented: what is required of an entity is data the operator enters.

CREATE TABLE legal_entities (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    jurisdiction      TEXT NOT NULL DEFAULT '',              -- free text; no jurisdiction rules are implemented
    entity_type       TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL CHECK (status IN ('active', 'dormant', 'dissolved')),
    incorporated_on   TEXT NOT NULL DEFAULT '',
    fiscal_year_end   TEXT NOT NULL DEFAULT '',              -- MM-DD
    sens_reg_number   TEXT,
    sens_tax_id       TEXT,
    equity_entity_ref TEXT NOT NULL DEFAULT '',              -- opaque `equity` entity id
    created_by        TEXT NOT NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

-- Registered agent and addresses, effective-dated. A new row of the same kind ends the previous one the
-- day before it starts: nothing is overwritten, the history stays. class entity-person (an agent may be an individual).
CREATE TABLE entity_contacts (
    id             TEXT PRIMARY KEY,
    entity_id      TEXT NOT NULL REFERENCES legal_entities (id),
    kind           TEXT NOT NULL CHECK (kind IN ('registered_agent', 'registered_office', 'principal_office', 'postal')),
    sens_name      TEXT,
    sens_address   TEXT,
    person_ref     TEXT NOT NULL DEFAULT '',                 -- opaque directory person id when the agent is a person
    effective_from TEXT NOT NULL,
    effective_to   TEXT,
    created_by     TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);
CREATE INDEX entity_contacts_entity ON entity_contacts (entity_id, kind, effective_from);

-- Statutory registers as append-only ledgers (class entity-register, kept by law). A mistake is corrected
-- by a later entry that names the one it corrects (corrects_id); nothing is edited or deleted.
CREATE TABLE register_entries (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT NOT NULL UNIQUE,
    entity_id    TEXT NOT NULL REFERENCES legal_entities (id),
    register     TEXT NOT NULL CHECK (register IN ('members', 'directors', 'charges')),
    kind         TEXT NOT NULL CHECK (kind IN ('appointed', 'ceased', 'allotted', 'transferred', 'created', 'satisfied')),
    party_ref    TEXT NOT NULL,                              -- opaque person or party id (a charge reference for charges)
    office       TEXT NOT NULL DEFAULT '' CHECK (office IN ('', 'director', 'officer', 'secretary', 'auditor')),
    title        TEXT NOT NULL DEFAULT '',
    class_ref    TEXT NOT NULL DEFAULT '',                   -- opaque `equity` share class id (members register)
    quantity     INTEGER NOT NULL DEFAULT 0,                 -- whole shares, signed: allotted +n, transferred -n from the holder
    effective_on TEXT NOT NULL,
    sens_detail  TEXT NOT NULL,                              -- JSON: name, address, date of birth, charge terms
    resolution_ref TEXT NOT NULL DEFAULT '',                 -- opaque board resolution id that authorised it
    corrects_id  TEXT,
    recorded_by  TEXT NOT NULL,
    recorded_at  INTEGER NOT NULL,
    attested_by  TEXT NOT NULL DEFAULT '',
    attested_on  TEXT
);
CREATE INDEX register_entries_entity ON register_entries (entity_id, register, effective_on);

-- Filings calendar: obligations are data; due dates are instances generated from them. class entity-filings.
CREATE TABLE filing_obligations (
    id               TEXT PRIMARY KEY,
    entity_id        TEXT NOT NULL REFERENCES legal_entities (id),
    name             TEXT NOT NULL,
    authority        TEXT NOT NULL DEFAULT '',
    first_due_on     TEXT NOT NULL,
    recurrence       TEXT NOT NULL CHECK (recurrence IN ('once', 'monthly', 'quarterly', 'annual')),
    lead_days        TEXT NOT NULL DEFAULT '30,7,0',          -- reminder points, days before the due date
    active           INTEGER NOT NULL DEFAULT 1,
    created_by       TEXT NOT NULL,
    created_at       INTEGER NOT NULL
);

CREATE TABLE filings (
    id             TEXT PRIMARY KEY,
    obligation_id  TEXT NOT NULL REFERENCES filing_obligations (id),
    entity_id      TEXT NOT NULL,
    due_on         TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('pending', 'filed', 'waived')),
    filed_on       TEXT,
    filed_by       TEXT NOT NULL DEFAULT '',
    doc_ref        TEXT NOT NULL DEFAULT '',                  -- opaque document id of the filing receipt
    reminders_sent TEXT NOT NULL DEFAULT '',                  -- lead days already announced, comma separated
    created_at     INTEGER NOT NULL,
    UNIQUE (obligation_id, due_on)
);
CREATE INDEX filings_due ON filings (status, due_on);

-- Document references (no bytes) and links to share classes in `equity` by opaque id.
CREATE TABLE entity_documents (
    id         TEXT PRIMARY KEY,
    entity_id  TEXT NOT NULL REFERENCES legal_entities (id),
    kind       TEXT NOT NULL DEFAULT '',
    doc_ref    TEXT NOT NULL,
    sens_title TEXT NOT NULL,
    added_by   TEXT NOT NULL,
    added_at   INTEGER NOT NULL,
    UNIQUE (entity_id, doc_ref)
);

CREATE TABLE share_class_links (
    id         TEXT PRIMARY KEY,
    entity_id  TEXT NOT NULL REFERENCES legal_entities (id),
    class_ref  TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (entity_id, class_ref)
);
