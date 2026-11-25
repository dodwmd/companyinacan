# Data Model: Identity Provisioning (E13d)

All timestamps are unix seconds (UTC). Nothing here holds names, emails, group-member lists beyond
person ids, tokens in clear, or downstream responses.

## can-core (migration `0007_identity_revoke.sql`)

| Table | Columns | Notes |
|---|---|---|
| `offboarded_parties` | `party_id` PK, `subject_ref`, `effective_date`, `recorded_at` | written only from `person.offboarded.v1` whose authenticated source is `people`; `subject_ref` is the first 32 hex of SHA-256 of `sso_subject`, empty when the person has no link |

## can-provision (`core/provision/migrations/001_provision.sql`)

| Table | Purpose |
|---|---|
| `runs` | `id` (`run_` + 24 hex, time-prefixed), `kind` (joiner, mover, leaver), `party_id`, `state`, `trigger`, `idem_key` (unique), `urgent`, `dry_run`, `privileged`, `effective_date`, `initiator`; a partial unique index allows one live run per (`party_id`, `kind`) |
| `steps` | (`run_id`, `ord`), `op`, `connector`, `arg` (a group name), `state`, `sensitive`, `optional`, `comp_op`, `attempts`, `not_before`, `next_at`, `approval_id`, `approved`, `external_id`, `error_code` (a machine code), `completed_at` |
| `accounts` | (`party_id`, `connector`) -> `external_id`, `state` (active, disabled, deleted) |
| `applied_groups` | (`party_id`, `connector`, `grp`): the memberships this module added |
| `urgent` | `party_id`, `flagged_by`, `flagged_at` |
| `seen_events` | event ids already handled (pruned after 30 days) |
| `scim_tokens` | `id`, `name`, `hash` (SHA-256), `scopes`, `created_by`, `expires_at`, `revoked_at` |
| `groups`, `group_members` | SCIM groups and their member person ids |
| `outbox` | events to publish through can-core, with attempts and next attempt |
| `audit` | hash-chained: `seq`, `time`, `actor`, `action`, `target`, `detail` (ids and counts), `prev_hash`, `hash` |

### Run states

`pending` -> `running` -> `succeeded` | `failed` | `compensating` -> `compensated` | `failed`;
`scheduled` (waiting for an effective date), `awaiting_approval`, `cancelled`.

### Step states

`pending`, `running`, `awaiting_approval`, `done`, `skipped` (nothing to do, dry run, or an optional
step that gave up), `dead` (dead letter), `rejected` (approval refused), `comp_pending`, `compensated`,
`comp_failed`.

### Events published

`provision.step.completed.v1` (`run_id`, `party_id`, `kind`, `step`, `op`, `connector`, `completed_at`)
and `provision.failed.v1` (the same with `reason`: `retries_exhausted`, `permanent`, `rejected`,
`compensation_failed`, and `failed_at`).
