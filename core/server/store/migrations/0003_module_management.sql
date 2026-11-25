-- Module management from the admin UI (epic E12b, specs/017-module-ui-wizard).

-- A proposed or applied company.yaml change. The patch is a list of allow-listed edits (see
-- core/server/modmgr/company.go); base_version is the stored company.yaml version it was prepared
-- against (optimistic concurrency).
CREATE TABLE company_changes (
  id           TEXT PRIMARY KEY,
  actor        TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  base_version TEXT NOT NULL,
  patch_json   TEXT NOT NULL,
  acks_json    TEXT NOT NULL DEFAULT '[]',
  summary      TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT 'proposed' CHECK (state IN ('proposed','applied')),
  applied_at   INTEGER,
  run_id       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX company_changes_state ON company_changes(state, created_at);

-- One reconcile run started from the UI. At most one is 'running' (enforced by the partial index).
CREATE TABLE apply_runs (
  id          TEXT PRIMARY KEY,
  change_id   TEXT NOT NULL DEFAULT '',
  actor       TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('running','succeeded','failed')),
  started_at  INTEGER NOT NULL,
  finished_at INTEGER,
  actions_json TEXT NOT NULL DEFAULT '[]',
  failed_json  TEXT NOT NULL DEFAULT '[]',
  error       TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX apply_runs_one_running ON apply_runs(state) WHERE state = 'running';

-- The last reconcile result of any origin (periodic loop or UI), shown on the status page.
CREATE TABLE reconcile_status (
  id      INTEGER PRIMARY KEY CHECK (id = 1),
  at      INTEGER NOT NULL,
  ok      INTEGER NOT NULL,
  actions INTEGER NOT NULL DEFAULT 0,
  failed  INTEGER NOT NULL DEFAULT 0,
  error   TEXT NOT NULL DEFAULT ''
);

-- The onboarding wizard (single row): resumable, idempotent.
CREATE TABLE wizard_state (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  status     TEXT NOT NULL CHECK (status IN ('open','completed','dismissed')),
  data_json  TEXT NOT NULL,
  revision   INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  apply_key  TEXT NOT NULL DEFAULT '',
  change_id  TEXT NOT NULL DEFAULT ''
);
