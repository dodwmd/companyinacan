# Data model: module management and onboarding wizard (E12b)

Migration `core/server/store/migrations/0003_module_management.sql`.

## company_changes

A proposed or applied edit of `company.yaml`.

| Column | Meaning |
|---|---|
| `id` | 24 hex characters (random) |
| `actor`, `created_at` | who prepared it, when (Unix seconds) |
| `base_version` | stored document version it was prepared against (`rv:...` or `sha:...`) |
| `patch_json` | `{"ops":[{"path":["modules","web","enabled"],"value":true}, ...]}` (allow-listed paths, at most 200 ops, 64 KiB) |
| `acks_json` | acknowledgements given: `licence:<id>`, `experimental:<id>` |
| `summary` | short sanitised note (`catalogue`, `settings web`, `onboarding wizard`) |
| `state` | `proposed` or `applied` |
| `applied_at`, `run_id` | set when applied |

At most 50 `proposed` rows are kept (oldest dropped on insert). A proposal older than 24 hours cannot be
applied. An applied change is kept as the record of what the UI wrote.

## apply_runs

| Column | Meaning |
|---|---|
| `id`, `change_id`, `actor`, `started_at`, `finished_at` | |
| `state` | `running`, `succeeded`, `failed` |
| `actions_json` | the reconcile actions executed (`kind`, `module`, `reason`) |
| `failed_json` | module ids that failed |
| `error` | sanitised error of the reconcile |

A unique partial index on `state = 'running'` allows at most one running row (the single-flight lock).

## reconcile_status

One row (`id = 1`): time, ok, number of actions, number failed, sanitised error of the last reconcile of any
origin (periodic loop or UI).

## wizard_state

One row (`id = 1`): `status` (`open`, `completed`, `dismissed`), `data_json`, `revision`, `updated_at`,
`apply_key`, `change_id`.

`data_json` (`WizardData`): `name`, `domain`, `template`, `modules[]`, `acks[]`, `server_ram_mb`,
`relay{host, port, username, from, configured}`, `kit_ack`. The relay password is **not** stored here; it is
written once to the Secret `can-system/can-smtp-relay` (`host`, `port`, `username`, `password`, `from`).

## In memory

- `Entry` (catalogue): manifest, optional compiled settings schema, `Invalid` reason; `Status()`, `Deployable()`, `SourceAvailable()`.
- `Analysis`: `Needs` (state per required capability), `Provides` (served or planned), `Conflicts`.
- `FootprintResult`: platform, request, limit, database, total, server size, verdict.
- `Preview`: change, stale flag, diff, actions, per-module resource plan, footprint, analysis, issues, acknowledgements needed.
- `Health`: module readiness, last reconcile, certificates, disk, with a note when a source is unavailable.

## Interfaces (all with fakes)

`CompanyStore` (KubeStore, FileStore), `Reconciling` (Runner over `reconcile.Reconciler`), `SecretWriter`
(KubeSecrets, FakeSecrets), `Purger` (none in this build), `IngressStatus` (none), `DiskReader` (DirDisk,
Linux `statfs`), `ReadyProbe` (none), `NetInfo` (StaticNet from `CAN_PUBLIC_IPS`), `InstantProbe` (none).

## Permissions

| Permission | Granted by default to |
|---|---|
| `platform.view` | operator, auditor, owner (catalogue, status, runs read) |
| `platform.operate` | operator, owner (preview, apply, settings, stored company.yaml) |
| `platform.setup` (new) | owner only (wizard: identity, domain, relay credentials) |
| `platform.purge` (new) | owner only (destroy module data) |
