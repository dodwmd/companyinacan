-- E11a: RBAC assignments, approval engine, notifications (specs/015-approvals-notifications).

-- Role assignments. subject is "user:<admin username>" or "party:<id>"; scope is '' (everything)
-- or a resource prefix such as "company/acme" or "expenses/*".
CREATE TABLE rbac_assignments (
  subject    TEXT NOT NULL,
  role       TEXT NOT NULL,
  scope      TEXT NOT NULL DEFAULT '',
  granted_by TEXT NOT NULL,
  granted_at INTEGER NOT NULL,
  PRIMARY KEY (subject, role, scope)
);
CREATE INDEX rbac_assignments_role ON rbac_assignments(role);

-- Operator overrides of the role grants declared in module manifests.
CREATE TABLE rbac_overrides (
  role       TEXT NOT NULL,
  permission TEXT NOT NULL,
  effect     TEXT NOT NULL CHECK (effect IN ('grant','revoke')),
  set_by     TEXT NOT NULL,
  set_at     INTEGER NOT NULL,
  PRIMARY KEY (role, permission)
);

-- Existing admins keep full access: the single admin of earlier versions becomes an owner.
INSERT INTO rbac_assignments(subject, role, scope, granted_by, granted_at)
  SELECT 'user:' || username, 'owner', '', 'migration', created_at FROM admin_users
   WHERE length(username) BETWEEN 1 AND 64 AND username NOT GLOB '*[^A-Za-z0-9_.@-]*';
-- An admin whose name is not a valid subject (older versions allowed any characters) gets no role
-- and cannot use role-gated pages until renamed; `can-core admin roles list` shows them without roles.

CREATE TABLE approval_policies (
  type        TEXT PRIMARY KEY,
  policy_json TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE approval_requests (
  id              TEXT PRIMARY KEY,
  module_id       TEXT NOT NULL,
  type            TEXT NOT NULL,
  requester       TEXT NOT NULL,
  ref             TEXT NOT NULL DEFAULT '',
  title           TEXT NOT NULL DEFAULT '',
  amount          INTEGER NOT NULL DEFAULT 0,
  payload_json    TEXT NOT NULL DEFAULT '{}',
  status          TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','cancelled','expired')),
  outcome_reason  TEXT NOT NULL DEFAULT '',
  idem_key        TEXT NOT NULL,
  request_hash    TEXT NOT NULL,
  policy_json     TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL DEFAULT 0,
  escalate_at     INTEGER NOT NULL DEFAULT 0,
  escalated       INTEGER NOT NULL DEFAULT 0,
  decided_at      INTEGER NOT NULL DEFAULT 0,
  UNIQUE (module_id, idem_key)
);
CREATE INDEX approval_requests_status ON approval_requests(status, created_at);
CREATE INDEX approval_requests_requester ON approval_requests(requester, created_at);
CREATE INDEX approval_requests_due ON approval_requests(status, expires_at, escalate_at);

CREATE TABLE approval_steps (
  request_id TEXT NOT NULL REFERENCES approval_requests(id) ON DELETE CASCADE,
  idx        INTEGER NOT NULL,
  stage      INTEGER NOT NULL,
  name       TEXT NOT NULL,
  quorum     INTEGER NOT NULL,
  status     TEXT NOT NULL CHECK (status IN ('waiting','active','approved','rejected','skipped')),
  approvers  TEXT NOT NULL,   -- JSON: resolved approver specs (roles, users, manager snapshot)
  PRIMARY KEY (request_id, idx)
);

CREATE TABLE approval_decisions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id   TEXT NOT NULL REFERENCES approval_requests(id) ON DELETE CASCADE,
  step_idx     INTEGER NOT NULL DEFAULT -1,
  actor        TEXT NOT NULL,
  on_behalf_of TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL CHECK (kind IN ('approve','reject','comment','cancel','escalate','expire','system')),
  comment      TEXT NOT NULL DEFAULT '',
  at           INTEGER NOT NULL
);
CREATE INDEX approval_decisions_request ON approval_decisions(request_id, id);
-- One vote per actor per step.
CREATE UNIQUE INDEX approval_votes ON approval_decisions(request_id, step_idx, actor) WHERE kind IN ('approve','reject');

CREATE TABLE approval_delegations (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  delegator TEXT NOT NULL,
  delegate  TEXT NOT NULL,
  from_ts   INTEGER NOT NULL,
  to_ts     INTEGER NOT NULL,
  revoked   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX approval_delegations_delegate ON approval_delegations(delegate, to_ts);

CREATE TABLE notifications (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  subject    TEXT NOT NULL,
  type       TEXT NOT NULL,
  dedupe_key TEXT NOT NULL DEFAULT '',
  data_json  TEXT NOT NULL DEFAULT '{}',
  link       TEXT NOT NULL DEFAULT '',
  urgent     INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  read_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX notifications_subject ON notifications(subject, read_at, id);
CREATE UNIQUE INDEX notifications_dedupe ON notifications(subject, dedupe_key) WHERE dedupe_key <> '';

CREATE TABLE notify_prefs (
  subject TEXT NOT NULL,
  type    TEXT NOT NULL,           -- notification type or '*'
  channel TEXT NOT NULL,           -- 'email' or 'inapp'
  enabled INTEGER NOT NULL,
  PRIMARY KEY (subject, type, channel)
);

CREATE TABLE notify_settings (
  subject     TEXT PRIMARY KEY,
  email       TEXT NOT NULL DEFAULT '',
  quiet_start INTEGER NOT NULL DEFAULT -1,  -- minutes after midnight in tz; -1 = no quiet hours
  quiet_end   INTEGER NOT NULL DEFAULT -1,
  tz          TEXT NOT NULL DEFAULT 'UTC',
  digest      TEXT NOT NULL DEFAULT 'off' CHECK (digest IN ('off','hourly','daily'))
);

-- Delivery log: one row per (notification, channel) attempt or decision.
CREATE TABLE notify_deliveries (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  notification_id INTEGER NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
  channel         TEXT NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('sent','failed','queued','suppressed')),
  detail          TEXT NOT NULL DEFAULT '',
  at              INTEGER NOT NULL
);
CREATE INDEX notify_deliveries_notification ON notify_deliveries(notification_id);
CREATE INDEX notify_deliveries_queued ON notify_deliveries(status, channel, at);

-- Operator-defined outbound webhook channels (Slack-style), from company.yaml.
CREATE TABLE notify_channels (
  name TEXT PRIMARY KEY,
  url  TEXT NOT NULL
);
