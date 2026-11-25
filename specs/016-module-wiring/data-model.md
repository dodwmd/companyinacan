# Data model (E03c)

## can-core state (migration `0004_module_wiring.sql`)

| Table | Columns | Notes |
|---|---|---|
| `api_tokens` | `id` (pk, `pat_<16 hex>`), `name`, `module_id`, `subject`, `permissions` (JSON array), `token_hash` (unique, SHA-256 hex), `created_by`, `created_at`, `expires_at`, `revoked_at` (0 = valid) | The token value is never stored. Pruned 30 days after expiry or revocation. At most 50 live tokens per module. |
| `event_dedupe` | `(source, source_id)` pk, `event_id`, `created_at` | A publisher's retry returns `event_id`. Pruned after 7 days. |
| `secret_rotations` | `(module_id, key)` pk, `requested_by`, `requested_at` | Consumed by the reconciler after a successful install. |

## Kubernetes (per module namespace)

`Secret can-module` (written by can-core only): `CAN_API_URL`, `CAN_MODULE_ID`, `CAN_WEBHOOK_SECRET`,
`CAN_TOKEN_FILE`, `CAN_TOKEN_AUDIENCE`, plus the generated keys:

| Module | Generated keys |
|---|---|
| work | `CAN_WORK_ADMIN_TOKEN` |
| forms | `CAN_FORMS_TOKEN_KEY`, `CAN_FORMS_API_TOKEN`, `CAN_FORMS_API_READ_TOKEN` |
| finance | `CAN_FINANCE_ADMIN_TOKEN`, `CAN_FINANCE_LINK_SECRET` |
| jobs | `CAN_JOBS_ADMIN_TOKEN`, `CAN_JOBS_HASH_KEY` (never rotated), `CAN_JOBS_FORM_SECRET`, `keyring` |
| people | `CAN_PEOPLE_API_TOKEN`, `keyring` |

Tokens are 32 random bytes as 64 hex characters (at least 32 characters, 16 distinct: above every
module's own minimum). A keyring is lines of `k<N> <64 hex>`, newest first.

Pod templates of Deployments and StatefulSets carry `can.dev/secret-version`.

## Module state

| Module | Change |
|---|---|
| people | migration `003_hire_ref.sql`: `people.hire_ref` (the jobs candidate id), unique where set |
| finance | migration `002_core_approvals.sql`: `core_approvals(ref, approval_id, version, attempt, status, requested_by, created_at, decided_at)` |
| jobs | none (uses the existing `approval_ref`) |
| work, forms | none |

## Token record, as the API shows it

`{id, name, module, subject, permissions[], created_at, expires_at, revoked, expired}`. Never the token
or its hash.

## Introspection answer

`{active, kind: "user"|"module", subject?, permissions?[], expires_at?, module_id?, namespace?, service_account?}`.
