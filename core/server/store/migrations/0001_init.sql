CREATE TABLE admin_users (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  username       TEXT NOT NULL UNIQUE,
  password_hash  TEXT NOT NULL,
  totp_secret    TEXT NOT NULL,
  totp_last_step INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL
);

CREATE TABLE sessions (
  id_hash    TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  ip         TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE login_attempts (
  key           TEXT PRIMARY KEY,
  failures      INTEGER NOT NULL,
  first_failure INTEGER NOT NULL,
  locked_until  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE modules (
  id              TEXT PRIMARY KEY,
  version         TEXT NOT NULL,
  namespace       TEXT NOT NULL,
  service_account TEXT NOT NULL,
  state           TEXT NOT NULL CHECK (state IN ('enabled','disabled','failed')),
  settings_hash   TEXT NOT NULL,
  hmac_secret     TEXT NOT NULL,
  manifest_json   TEXT NOT NULL,
  last_error      TEXT NOT NULL DEFAULT '',
  updated_at      INTEGER NOT NULL,
  UNIQUE (namespace, service_account)
);

CREATE TABLE capability_providers (
  capability TEXT NOT NULL,
  module_id  TEXT NOT NULL REFERENCES modules(id) ON DELETE CASCADE,
  service    TEXT NOT NULL,
  port       INTEGER NOT NULL,
  schema     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (capability, module_id)
);

CREATE TABLE primary_providers (
  capability TEXT PRIMARY KEY,
  module_id  TEXT NOT NULL
);

CREATE TABLE subscriptions (
  module_id TEXT NOT NULL REFERENCES modules(id) ON DELETE CASCADE,
  event     TEXT NOT NULL,
  service   TEXT NOT NULL,
  port      INTEGER NOT NULL,
  webhook   TEXT NOT NULL,
  PRIMARY KEY (module_id, event)
);

CREATE TABLE events (
  id        TEXT PRIMARY KEY,
  type      TEXT NOT NULL,
  time      INTEGER NOT NULL,
  source    TEXT NOT NULL,
  data_json TEXT NOT NULL
);
CREATE INDEX events_time ON events(time);

CREATE TABLE deliveries (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id        TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  module_id       TEXT NOT NULL,
  url             TEXT NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('pending','delivered','dead')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  UNIQUE (event_id, module_id)
);
CREATE INDEX deliveries_due ON deliveries(status, next_attempt_at);

CREATE TABLE delivery_attempts (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  delivery_id INTEGER NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
  attempt     INTEGER NOT NULL,
  at          INTEGER NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  error       TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE audit_log (
  seq       INTEGER PRIMARY KEY,
  time      INTEGER NOT NULL,
  actor     TEXT NOT NULL,
  action    TEXT NOT NULL,
  target    TEXT NOT NULL,
  detail    TEXT NOT NULL,
  prev_hash TEXT NOT NULL,
  hash      TEXT NOT NULL
);
