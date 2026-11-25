# Data Model: Work Tracker (E14a)

SQLite, migration `core/work/migrations/001_work.sql`. Timestamps are unix seconds (UTC).

## projects

| Column | Notes |
|---|---|
| `key` | unique, `^[A-Z][A-Z0-9]{1,9}$` |
| `name`, `description` | one-line name (100), description (2000) |
| `workflow` | JSON: `{statuses:[{name,category}], initial, transitions:{from:[to...]}}`; names `^[a-z][a-z0-9_]{0,29}$`, at most 30 statuses; categories `todo`, `active`, `done` |
| `sla` | JSON map `P0..P3` to `{respond_hours, resolve_hours}`; a hint, defaults 1h/24h, 8h/7d, 3d/30d, 7d/90d |
| `next_number` | counter behind issue keys; incremented in the create transaction |
| `archived` | read-only when 1 |

## issues

| Column | Notes |
|---|---|
| `project_id`, `number` | unique pair; key is `<project key>-<number>` |
| `type` | `epic`, `story`, `task`, `bug`, `subtask` |
| `title` (200, one line), `description` (50,000) | |
| `priority` | `P0` to `P3`, default `P2` |
| `status` | a status of the project workflow; initial on create; changed only by a transition |
| `reporter` | the creating token's subject; never client-supplied |
| `assignee`, `due_date` (`YYYY-MM-DD`), `estimate` (0 to 10,000) | nullable |
| `parent_id` | same project; `ON DELETE SET NULL` |
| `sprint_id` | planned or active sprint of the same project; `ON DELETE SET NULL` |
| `rank` | fractional rank string; project-wide ordering |
| `version` | starts at 1; +1 on every update or transition that changes something; the ETag |
| `created_at`, `updated_at`, `resolved_at` | `resolved_at` is set when the status category is `done` |

Indexes: `(project_id, rank)`, assignee, parent, sprint, resolved. Labels live in `issue_labels`
(`^[a-z0-9][a-z0-9._:-]{0,39}$`, at most 20 per issue).

## Related tables

| Table | Purpose |
|---|---|
| `sprints` | per project; `state` planned, active, closed; a partial unique index allows one active sprint per project |
| `issue_links` | `blocks`, `relates`, `duplicates`; unique `(type, source, target)`; no self links; cycle checks in code |
| `comments` | author is the token subject; body at most 20,000 characters; append-only |
| `activity` | append-only history per issue: `created`, `updated` (with before and after), `transitioned`, `commented`, `linked`, `unlinked`, `attachment_added`, `attachment_removed` |
| `watchers` | reporter, assignee and commenters start watching; anyone with write can add or remove |
| `attachments` | metadata only: filename, content type, size, sha256, url, who added |
| `idempotency` | `(subject, key)` to request hash and stored response; purged after 7 days |
| `api_tokens` | id, kind, subject, name, `secret_hash` (SHA-256), scopes, projects, created_by, expiry, revoked, last used |
| `audit_log` | admin actions: token create and revoke, project create and update; never secrets |
| `outbox` | queued events with attempts and next attempt; delivered rows purged after 7 days |

## Rules enforced in code (store)

- Project scope on every path; out-of-scope is not found.
- Hierarchy: epic has no parent; story, task and bug sit under an epic; subtask needs a story, task or bug;
  same project; no ancestor cycles.
- Links: no self links, no repeats (`relates` in either order), no cycles for `blocks` and `duplicates`.
- Workflow transitions only along allowed edges; archived projects reject writes.
- Retention: resolved issues older than `retention_days` are deleted with their children;
  audit entries older than `audit_retention_days`.

## Entities exposed by the capability

`work.v1`: Issue, IssueList, CreateIssueRequest, BulkCreateRequest, UpdateIssueRequest,
TransitionRequest, AddCommentRequest, Comment. `projects.v1`: Project, ProjectList,
CreateProjectRequest. The module's full API is `contracts/openapi.yaml`.
