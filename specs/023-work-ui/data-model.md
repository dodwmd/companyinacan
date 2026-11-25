# Data Model: Work Tracker Web UI (E14c)

The UI adds no table and no migration. It reads and writes the tracker's existing entities through
the `Store` (`specs/011-work-tracker/data-model.md`). The only new state is in memory.

## UI session (memory only)

| Field | Notes |
|---|---|
| id | 256 random bits, base64url; only in the cookie. The table is keyed by SHA-256(id) |
| token | the access token presented at sign-in, kept to authenticate every request again. Never logged, never written to disk, never rendered |
| subject | the principal's subject, for the per-subject cap and the rate-limit key |
| csrf | 192 random bits, rendered into every form of this session |
| created, last | idle timeout 8 h since `last`, absolute timeout 24 h since `created` |

Bounds: 5000 sessions in total, 20 per subject (the oldest is dropped), expired entries are removed
whenever a session is created. A restart empties the table.

## Cookies

| Name | Value | Lifetime |
|---|---|---|
| `work_session` / `__Host-work_session` | session id | 24 h maximum |
| `work_login_csrf` / `__Host-work_login_csrf` | double-submit token of the login form | 1 h, removed on success |

## Query parameters (state kept in the URL)

| Page | Parameters |
|---|---|
| Project home | `q`, `status`, `priority`, `type`, `assignee`, `label`, `sort`, `scope`, `page` |
| Board | `sprint` (`active`, `backlog`, `all`, sprint id) |
| Issue | `ca`, `aa` (comment and activity cursors), `ok` (fixed set of notice names) |
| Dashboard | `assignees` |
| Epics | `page` |

Nothing in a query parameter is trusted: each is validated or limited before use, and `ok` only
selects one of a fixed set of messages.
