# Data Model: Admin UI (E12a)

The UI owns **no persistent state**. It reads and writes existing stores; the only additions to shared
packages are read helpers and account operations.

## Added to shared packages

| Where | Addition |
|---|---|
| `store` | `ListSessions(userID, now)`, `RevokeSession(userID, publicID)`, `PublicSessionID(Session)` (first 16 hex characters of the token hash; not secret) |
| `audit` | `Filter{Actor, ActionPrefix, Target, Before, Limit}` and `Log.Query` (newest first) |
| `auth` | `Service.ChangePassword`, `Service.RotateTOTP`; throttle key `acct:<username>` in `login_attempts` |
| `runtime` | env `CAN_COMPANY_NAME`, `CAN_COMPANY_DOMAIN`, `CAN_LEADS_TOKEN_FILE`; mounts the UI |

No schema migration.

## View models (package `adminui`, in memory, per request)

- **Page**: `Title`, `Active` nav key, `User`, `CSRF`, `Flash` (one-shot message from the `flash` query
  value, restricted to catalogue keys), `RequestID`, content struct.
- **ModuleCard**: id, name, version, state, last error, licence class, `SourceAvailable`, RAM MB, provides.
- **Company**: `Name`, `Domain` (from configuration; empty values render a hint).
- **DeliveryHealth**: counts of pending, delivered, dead over the most recent 500 deliveries, plus the dead
  rows (with requeue forms).
- **Lead** (from `leads.v1`): `id`, `form_id`, `status`, `email`, `name`, `fields` (string map; non-string
  values rendered as JSON text), `note`, `created_at`, `updated_at`.

## `LeadsClient`

```go
type LeadsClient interface {
    List(ctx context.Context, q LeadQuery) (LeadPage, error)   // status, cursor, limit
    Get(ctx context.Context, id string) (Lead, error)
    SetStatus(ctx context.Context, id, status string) error
}
```

Errors: `ErrLeadsUnavailable` (no provider or not configured), `ErrLeadNotFound`; anything else is a
gateway failure (502 page).
