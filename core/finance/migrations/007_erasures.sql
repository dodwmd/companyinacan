-- specs/056-erasure-finance-work: tombstones of party erasures, read by can-core into the erasure
-- ledger so that a restore erases again. The subject is the party's id with its creation time
-- ("party-<id>-<unix seconds>"): ids are handed out again after a restore of an older backup, the
-- pair is not. scrubbed = 0 means the write-ahead log still has to be truncated.
CREATE TABLE erasures (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT    NOT NULL,
    class      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    scrubbed   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (subject, class)
);
-- Parties erased before this release get their tombstone too.
INSERT INTO erasures (subject, class, created_at, scrubbed)
    SELECT 'party-' || id || '-' || created_at, 'finance-party-contact', erased_at, 1 FROM parties WHERE erased_at IS NOT NULL ORDER BY id;
