-- E03c: module wiring (specs/016-module-wiring).

-- Personal access tokens for the modules' APIs. Only the SHA-256 of the token is stored (tokens are
-- 256-bit random values, so a fast hash is enough); the token itself is shown once at issue.
CREATE TABLE api_tokens (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  module_id   TEXT NOT NULL,
  subject     TEXT NOT NULL,          -- user:<admin username> that issued it; the modules' principal
  permissions TEXT NOT NULL,          -- JSON array of module permissions, a subset of the manifest's
  token_hash  TEXT NOT NULL UNIQUE,
  created_by  TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  revoked_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX api_tokens_module ON api_tokens(module_id, revoked_at);

-- Publish de-duplication: a module that retries a publish (the ack was lost) sends the same
-- source_id and gets the original event back instead of a second delivery.
CREATE TABLE event_dedupe (
  source     TEXT NOT NULL,
  source_id  TEXT NOT NULL,
  event_id   TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (source, source_id)
);
CREATE INDEX event_dedupe_created ON event_dedupe(created_at);

-- Operator-requested secret rotations, consumed by the next reconcile of the module.
CREATE TABLE secret_rotations (
  module_id    TEXT NOT NULL,
  key          TEXT NOT NULL,
  requested_by TEXT NOT NULL,
  requested_at INTEGER NOT NULL,
  PRIMARY KEY (module_id, key)
);
