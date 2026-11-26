-- specs/049-contract-esign: in-house "simple electronic signature". An envelope freezes one contract
-- version (its text hash and title) and the people asked to sign it; signing_events is a hash chain
-- over what happened. Link tokens are stored only as SHA-256. Defence in depth, as in 001: the rules
-- live in the store and these triggers refuse the edits the rules forbid. The retention purge and the
-- party erasure open retention_gate for the length of their own transaction.
CREATE TABLE signing_envelopes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    contract_id INTEGER NOT NULL REFERENCES contracts (id),
    version     INTEGER NOT NULL,
    sha256      TEXT    NOT NULL,
    title       TEXT    NOT NULL,
    ordered     INTEGER NOT NULL,
    link_days   INTEGER NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('pending', 'completed', 'declined', 'revoked')),
    sender      TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    closed_at   INTEGER,
    chain_head  TEXT    NOT NULL DEFAULT '',
    scrubbed    INTEGER NOT NULL DEFAULT 0          -- signers' personal data removed by a party erasure
);
CREATE INDEX signing_envelopes_contract ON signing_envelopes (contract_id);
CREATE UNIQUE INDEX signing_envelopes_pending ON signing_envelopes (contract_id) WHERE status = 'pending';

CREATE TABLE signing_signers (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    envelope_id    INTEGER NOT NULL REFERENCES signing_envelopes (id),
    position       INTEGER NOT NULL,
    role           TEXT    NOT NULL CHECK (role IN ('counterparty', 'company')),
    name           TEXT    NOT NULL,
    email          TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('waiting', 'invited', 'signed', 'declined')),
    token_hash     TEXT,                              -- SHA-256 of the live link token; NULL when there is none
    expires_at     INTEGER,
    sends          INTEGER NOT NULL DEFAULT 0,
    mail_status    TEXT    NOT NULL DEFAULT '',       -- '', pending, queued, not_allowed, failed
    cert_status    TEXT    NOT NULL DEFAULT '',       -- the same, for the completion certificate
    cert_sends     INTEGER NOT NULL DEFAULT 0,
    typed_name     TEXT    NOT NULL DEFAULT '',
    consent_text   TEXT    NOT NULL DEFAULT '',
    ip             TEXT    NOT NULL DEFAULT '',       -- per the IP policy (full, truncated or empty)
    signed_at      INTEGER,
    declined_at    INTEGER,
    decline_reason TEXT    NOT NULL DEFAULT '',
    UNIQUE (envelope_id, position)
);
CREATE UNIQUE INDEX signing_signers_token ON signing_signers (token_hash) WHERE token_hash IS NOT NULL;

CREATE TABLE signing_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    envelope_id INTEGER NOT NULL REFERENCES signing_envelopes (id),
    seq         INTEGER NOT NULL,
    kind        TEXT    NOT NULL,
    signer_id   INTEGER NOT NULL DEFAULT 0,
    data        TEXT    NOT NULL,                     -- JSON; digests only, no personal data
    at          INTEGER NOT NULL,
    prev_hash   TEXT    NOT NULL,
    hash        TEXT    NOT NULL,
    UNIQUE (envelope_id, seq)
);

CREATE TRIGGER signing_envelopes_frozen BEFORE UPDATE OF contract_id, version, sha256, title, ordered, link_days, sender, created_at ON signing_envelopes
BEGIN
    SELECT RAISE(ABORT, 'what was sent for signature is immutable');
END;
CREATE TRIGGER signing_envelopes_closed BEFORE UPDATE ON signing_envelopes
    WHEN OLD.status <> 'pending' AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'a closed signing request is immutable');
END;
CREATE TRIGGER signing_envelopes_no_delete BEFORE DELETE ON signing_envelopes WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'signing records are immutable');
END;
CREATE TRIGGER signing_signers_frozen BEFORE UPDATE OF envelope_id, position, role ON signing_signers
BEGIN
    SELECT RAISE(ABORT, 'signers of a signing request are immutable');
END;
CREATE TRIGGER signing_signers_identity BEFORE UPDATE OF name, email ON signing_signers WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'signers of a signing request are immutable');
END;
CREATE TRIGGER signing_signers_decided BEFORE UPDATE OF status, token_hash, typed_name, consent_text, ip, signed_at, declined_at, decline_reason ON signing_signers
    WHEN OLD.status IN ('signed', 'declined') AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'a signature or refusal is immutable');
END;
CREATE TRIGGER signing_signers_no_late_insert BEFORE INSERT ON signing_signers
    WHEN (SELECT status FROM signing_envelopes WHERE id = NEW.envelope_id) <> 'pending'
BEGIN
    SELECT RAISE(ABORT, 'a closed signing request is immutable');
END;
CREATE TRIGGER signing_signers_no_delete BEFORE DELETE ON signing_signers WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'signing records are immutable');
END;
CREATE TRIGGER signing_events_no_update BEFORE UPDATE ON signing_events
BEGIN
    SELECT RAISE(ABORT, 'signing evidence is append-only');
END;
CREATE TRIGGER signing_events_no_delete BEFORE DELETE ON signing_events WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'signing evidence is append-only');
END;

-- A signed contract's identity and text never change again, however it was signed.
CREATE TRIGGER contracts_signed_frozen BEFORE UPDATE OF party_id, template_id, title, current_version, signed_at, signed_sha256, esign_provider, esign_id ON contracts
    WHEN OLD.signed_at IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a signed contract is immutable');
END;
CREATE TRIGGER contracts_signed_status BEFORE UPDATE OF status ON contracts
    WHEN OLD.signed_at IS NOT NULL AND NEW.status NOT IN ('signed', 'active', 'expired', 'terminated')
BEGIN
    SELECT RAISE(ABORT, 'a signed contract is immutable');
END;
CREATE TRIGGER contract_versions_none_after_signing BEFORE INSERT ON contract_versions
    WHEN (SELECT signed_at FROM contracts WHERE id = NEW.contract_id) IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a signed contract is immutable');
END;
