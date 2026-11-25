# Data Model: can-core state (SQLite)

Source: `core/server/store/migrations/0001_init.sql`. Times are Unix seconds. Secrets are stored only where
can-core must use them later (webhook HMAC secrets, TOTP seed) and live on the encrypted-at-rest PVC; they are never
returned by the APIs or written to logs.

| Table | Purpose | Key fields |
|---|---|---|
| `schema_migrations` | applied migration numbers | `version` |
| `admin_users` | the operator | `username` (unique), `password_hash` (argon2id PHC), `totp_secret` (base32), `totp_last_step` (replay guard) |
| `sessions` | admin sessions | `id_hash` = SHA-256 of the cookie token, `csrf_token`, `ip`, `expires_at`; cascades on user delete |
| `login_attempts` | throttling | `key` (`user:<name>` or `ip:<addr>`), `failures`, `first_failure`, `locked_until` |
| `modules` | recorded module state | `id`, `version`, `namespace`, `service_account` (unique pair = token identity), `state` in {enabled, disabled, failed}, `settings_hash`, `hmac_secret`, `manifest_json`, `last_error` |
| `capability_providers` | capabilities a module provides | `(capability, module_id)`, `service`, `port`, `schema`; replaced atomically on every registration |
| `primary_providers` | operator's `primary:` choices | `capability` -> `module_id` |
| `subscriptions` | event webhooks | `(module_id, event)`, `service`, `port`, `webhook` |
| `events` | outbox of published events | `id` (`evt_` + 128 random bits), `type`, `time`, `source`, `data_json`; pruned after 30 days once no delivery is pending |
| `deliveries` | delivery state per (event, module) | `status` in {pending, delivered, dead}, `attempts`, `next_attempt_at`, `url` |
| `delivery_attempts` | the delivery log | `delivery_id`, `attempt`, `at`, `status_code`, `error`, `duration_ms` |
| `audit_log` | hash chain | `seq`, `time`, `actor`, `action`, `target`, `detail` (JSON text), `prev_hash`, `hash` |

## Rules

- **Module state machine** (`reconcile`): `(none) -> enabled | failed` on install; `enabled -> enabled` on upgrade;
  `enabled | failed -> disabled` when no longer wanted; `disabled -> enabled` on re-enable; `failed -> enabled` on retry.
  Only `enabled` modules provide capabilities, receive events or may call the module API.
- **Record hash**: `SHA-256( "can-audit-v1" || len-prefixed(seq, time, actor, action, target, detail, prev_hash) )`;
  the first record's `prev_hash` is 64 zeros.
- **Registration** replaces all provider and subscription rows of a module in one transaction, so an upgrade that
  drops a capability stops providing it immediately.
- **Webhook URL** = `http://<service>.<namespace>.svc.cluster.local:<port><webhook>`, derived from the manifest at
  publish time; event data never influences it.
- **Defaults** for a subscription: service = module id; port = the port of the module's first `provides`, else 80.
