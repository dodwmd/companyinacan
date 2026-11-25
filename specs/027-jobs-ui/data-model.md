# Data model: Recruiting web UI (E16b)

The UI adds **no table and no stored field of its own**. One additive column supports stage-move
reasons (below); everything else shown comes from the Store.

## Migration 004

```sql
ALTER TABLE stage_history ADD COLUMN note_sealed TEXT;
```

`note_sealed` is NULL for moves without a reason (API moves, hires, rejections, the initial entry).
It is sealed with XChaCha20-Poly1305 using `stage_history:<seq>:note_sealed` as additional data,
listed in `sealedCols` (key rotation), cleared by the `evaluations` purge class (erasure and
retention), and returned (`StageChange.Note`) only to callers who may read rejection notes.

## In-memory session

| Field | Meaning |
|---|---|
| key | SHA-256 of the 256-bit cookie value (the cookie value itself is never stored) |
| `subject` | the authenticated subject |
| `princ` | the `Principal` (subject, role, job list) as last authenticated |
| `token` | the token presented at login; memory only; used for the periodic re-check |
| `csrf` | 256-bit form token for this session |
| `created`, `last`, `checked` | absolute, idle and re-authentication clocks |

Bounds: 5000 sessions, 5 per subject (oldest dropped). Idle 30 min, absolute 8 h, re-check 1 minute
for writers, 5 minutes for others. A restart drops all sessions.

## Cookies

| Name (plain HTTP / TLS) | Value | Flags |
|---|---|---|
| `can_jobs_session` / `__Host-can_jobs_session` | session id | HttpOnly, SameSite=Strict, Path=/, Secure on TLS, session lifetime |
| `can_jobs_login_csrf` / `__Host-can_jobs_login_csrf` | double-submit token | same flags, 15 minutes |

## Configuration (`jobs.yaml`)

```yaml
ui:
  enabled: false          # default off: the module serves the API and the careers pages only
  insecure_cookies: false # plain-HTTP development only (drops Secure and the __Host- prefix)
  idle_minutes: 30        # 0 uses the default
  absolute_hours: 8
```

The client address for failed-login throttling follows the module's existing `trust_proxy`.
Request time bound follows `db_timeout_seconds`.

## Service additions (no new storage)

| Method | Purpose |
|---|---|
| `Principal.Caps()` | booleans for drawing links and forms |
| `Store.Require(ctx, p, action)` | the API guard's check for pages that draw a form first |
| `Store.FunnelCounts(ctx, p, jobID)` | active applications per stage, and hired/rejected/withdrawn totals; scoped; aggregate only |
| `Store.ListMyInterviews` | active applications the caller is currently assigned to, with the caller's own scorecard state |
| `Store.IsAssigned` | whether the caller is currently on the panel of an application in scope |
| `Store.GetCandidateSummary` | name, tags, source, talent-pool flag; unseals nothing |
| `Store.SearchTalentPool` | talent-pool list with a bounded name prefix |
| `Store.MoveApplicationNote` | move with a required sealed reason |
