-- The notification email of a lead, until can-core has accepted it (E03d). Written with the lead, so
-- a lead can never be stored without it; removed with the lead. It holds no personal data.
CREATE TABLE lead_email (
    lead_id         TEXT    PRIMARY KEY REFERENCES leads (id) ON DELETE CASCADE,
    status          TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL
);

CREATE INDEX lead_email_due ON lead_email (status, next_attempt_at);
