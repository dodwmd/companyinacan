-- E03d: the email.send outbox (specs/039-email-send). One row per recipient. recipient, reply_to,
-- subject and body are personal data: they are blanked as soon as the message is sent, and seven days
-- after it is given up on. Rows are deleted 30 days after they finish.
CREATE TABLE email_outbox (
  id              TEXT PRIMARY KEY,
  source          TEXT NOT NULL,             -- module id, or '@core' for can-core's own mail
  source_id       TEXT NOT NULL DEFAULT '',  -- the sender's idempotency key
  seq             INTEGER NOT NULL DEFAULT 0, -- recipient index within one request
  request_hash    TEXT NOT NULL DEFAULT '',
  recipient       TEXT NOT NULL DEFAULT '',
  recipient_mask  TEXT NOT NULL,
  reply_to        TEXT NOT NULL DEFAULT '',
  subject         TEXT NOT NULL DEFAULT '',
  body            TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL CHECK (status IN ('pending','sent','dead')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  last_error      TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  queued_at       INTEGER NOT NULL,          -- created_at, or the last requeue
  finished_at     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX email_outbox_source_id ON email_outbox(source, source_id, seq) WHERE source_id <> '';
CREATE INDEX email_outbox_due ON email_outbox(status, next_attempt_at);
CREATE INDEX email_outbox_quota ON email_outbox(source, created_at);
CREATE INDEX email_outbox_created ON email_outbox(created_at);
