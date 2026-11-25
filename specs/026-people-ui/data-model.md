# Data model: People web UI (E13c)

The UI adds **no table and no stored field**. Everything it shows comes from the Service.

## In-memory session

| Field | Meaning |
|---|---|
| key | SHA-256 of the 256-bit cookie value (the cookie value itself is never stored) |
| `subject` | the authenticated subject (links to `people.sso_subject`) |
| `princ` | the `Principal` (subject, roles, manifest permissions) as last authenticated |
| `token` | the PAT presented at login; memory only; used for the 5-minute re-check |
| `csrf` | 256-bit form token for this session |
| `created`, `last`, `checked` | absolute, idle and re-authentication clocks |

Bounds: 5000 sessions, 5 per subject (oldest dropped). Idle 30 min, absolute 8 h, re-check 5 min
(`ui.idle_minutes`, `ui.absolute_hours`). A restart drops all sessions.

## Cookies

| Name (plain HTTP / TLS) | Value | Flags |
|---|---|---|
| `can_people_session` / `__Host-can_people_session` | session id | HttpOnly, SameSite=Strict, Path=/, Secure on TLS, session lifetime |
| `can_people_login_csrf` / `__Host-can_people_login_csrf` | double-submit token | same flags, 15 minutes |

## Configuration (`people.yaml`)

```yaml
ui:
  enabled: false          # default off: the module serves the JSON API only
  insecure_cookies: false # true only for plain-HTTP development
  trust_proxy: false      # take the client address from the last X-Forwarded-For entry
  notifications_url: ""   # optional link to the admin UI notifications page
  idle_minutes: 30
  absolute_hours: 8
```

## Seams

| Seam | Type | Default |
|---|---|---|
| Approvals waiting | `ApprovalSource` interface (`Waiting(ctx, subject) ([]ApprovalItem, error)`) | nil: no approvals section |
| Notifications | `NotificationsURL` string | empty: no link |

## Service helpers added for the UI

`Actor.Active()` (the policy gate for "may use the service at all"), `Service.SensitiveFieldsFor`
(fields readable, no read, no read audit), `Service.CanWriteGoal`, `Service.ReviewCapabilities`.
Each wraps a decision the corresponding write method already makes.
