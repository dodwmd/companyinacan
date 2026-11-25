# Data Model: PWA Hardening and Web Push (E19a)

Migration `core/server/store/migrations/0006_webpush.sql`. Nothing existing changes shape;
`notify_prefs.channel` and `notify_deliveries.channel` are free text and gain the value `webpush`.

## push_subscriptions

One row per browser or device a user enabled push on.

| Column | Type | Notes |
|---|---|---|
| `id` | integer PK | Shown to the owner; used by the UI remove form |
| `subject` | text | `user:<name>`; at most 10 rows per subject (enforced in `Subscribe`) |
| `endpoint` | text UNIQUE | The push service URL: a capability. Validated (https, 443, public DNS name, at most 500 characters). Never logged, never shown whole |
| `p256dh` | text | Browser public key, base64url, 65-byte P-256 point (validated on the curve) |
| `auth` | text | Browser auth secret, base64url, 16 bytes |
| `label` | text | Coarse device label from a fixed vocabulary ("Chrome on Android"), never the raw User-Agent |
| `created_at` | integer | Unix seconds |
| `last_ok_at` | integer | Last accepted delivery, 0 = never |
| `fail_count` | integer | Consecutive failed deliveries; reset on success or re-subscribe; the row is deleted at 10 |

Rules: re-subscribing a known endpoint refreshes keys and label (not a new device, so allowed at the
cap); an endpoint held by another user moves to the caller (the same browser signed in as someone else);
404/410 from the push service deletes the row; rotation of the VAPID key deletes all rows.

## push_vapid

At most one row (`id = 1`): `private_key` (base64url P-256 scalar, a secret), `created_at`. Absent when
`CAN_VAPID_KEY_FILE` is used or before first use.

## Existing tables, new values

- `notify_prefs(subject, type, channel='webpush', enabled)`: per-type switch; absent = on.
- `notify_deliveries(notification_id, channel='webpush', status, detail, at)`: `sent` (detail
  "N of M devices"), `failed` (detail names the last error, never the endpoint), `suppressed`
  (`preference`, `quiet hours`, `rate limited`, `push queue is full`).
- Events: `notification.sent.v1` with `channel: "webpush"` after at least one device accepted.

## In memory only

The lazily created push client (HTTP transport, VAPID key) and a map of last test-notification times
per user (bounded at 1000 entries, then cleared). None exists until first use.
