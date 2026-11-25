-- Outcome of notification attempts per lead (email, webhook). Removed with the lead.
CREATE TABLE lead_deliveries (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    lead_id    TEXT    NOT NULL REFERENCES leads (id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL CHECK (kind IN ('email', 'webhook')),
    ok         INTEGER NOT NULL,
    attempts   INTEGER NOT NULL,
    last_error TEXT    NOT NULL DEFAULT '',
    at         INTEGER NOT NULL
);

CREATE INDEX lead_deliveries_lead ON lead_deliveries (lead_id);
