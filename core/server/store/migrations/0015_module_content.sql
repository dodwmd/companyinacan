-- Upgrade detection by content: what each module was installed from, and upgrades that wait for an
-- operator because they widen what the module may do (core/server/reconcile).

-- JSON of the hashes of the manifest, chart and platform templates last installed; '' for records
-- made before this column, which are applied once more to fill it.
ALTER TABLE modules ADD COLUMN content_json TEXT NOT NULL DEFAULT '';
-- JSON of the held upgrade ({"approval": digest, "widens": [...]}); '' when none.
ALTER TABLE modules ADD COLUMN pending_json TEXT NOT NULL DEFAULT '';
-- Digest of the widening the operator approved; '*' approves whatever comes next (after a restore).
ALTER TABLE modules ADD COLUMN approved_upgrade TEXT NOT NULL DEFAULT '';
-- JSON of the settings, exposure and mail relay the running release was installed with, to say which
-- requested changes a held upgrade keeps from it; '' when unknown.
ALTER TABLE modules ADD COLUMN applied_json TEXT NOT NULL DEFAULT '';
-- A webhook signing secret that is being put into a module but is not recorded yet. It is stored
-- before the cluster is touched and reused by every retry, so a release signed with it stays readable.
CREATE TABLE module_pending_secrets (
  module_id TEXT PRIMARY KEY,
  secret    TEXT NOT NULL
);
