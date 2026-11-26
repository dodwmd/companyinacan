-- specs/056-erasure-finance-work: tombstones of person erasures, read by can-core into the erasure
-- ledger so that a restore erases again. The subject is a hash of the identifier that was erased,
-- never the identifier. scrubbed = 0 means the database files still have to be scrubbed.
CREATE TABLE erasures (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT    NOT NULL,
    class      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    scrubbed   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (subject, class)
);
