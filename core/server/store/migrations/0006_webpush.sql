-- E19a: Web Push (specs/025-pwa-webpush).

-- One row per browser/device a user enabled push on. The endpoint is a capability URL of the
-- browser's push service: it is never logged and never shown whole. p256dh and auth are the
-- browser's public key and auth secret that payloads are encrypted for (RFC 8291).
CREATE TABLE push_subscriptions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  subject    TEXT NOT NULL,
  endpoint   TEXT NOT NULL UNIQUE,
  p256dh     TEXT NOT NULL,
  auth       TEXT NOT NULL,
  label      TEXT NOT NULL DEFAULT '',   -- coarse device label derived from the User-Agent, never the raw header
  created_at INTEGER NOT NULL,
  last_ok_at INTEGER NOT NULL DEFAULT 0,
  fail_count INTEGER NOT NULL DEFAULT 0  -- consecutive failed deliveries; the row is dropped at 10
);
CREATE INDEX push_subscriptions_subject ON push_subscriptions(subject);

-- The VAPID application-server key pair (RFC 8292), created on first use. Only the private scalar
-- is a secret; it lives in the state database unless CAN_VAPID_KEY_FILE points at a mounted Secret.
-- Rotating it drops every subscription (browsers bind a subscription to the public key).
CREATE TABLE push_vapid (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  private_key TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);
