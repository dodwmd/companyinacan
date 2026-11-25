# Research: Work Tracker (E14a)

## R1. Build in-house, not wrap, for this slice
- **Decision**: Implement a lean tracker in Go. A Plane wrapper stays a separate, later module for
  8 GB+ servers; both provide the `work` capability.
- **Rationale**: SPEC.md section 6.1 and 3.3 name the in-house `work` as the target on small
  servers, and Plane brings a multi-service stack with private datastores (about 1.5 to 2 GB). The
  data (people, priorities, comments) is sensitive and must share the platform's party model
  later. The in-house service also lets the API, idempotency and scoped tokens be exactly what
  managers' automation needs on day one.
- **Alternatives**: Plane (AGPL, heavy, API differs), Redmine (lighter, dated, Ruby), Vikunja (no
  issue-tracker semantics), Kanboard (not Jira-like).

## R2. Storage and concurrency
- **Decision**: SQLite via `modernc.org/sqlite` (as in E05), one connection, WAL, embedded
  migrations; every multi-step operation in one transaction using only the `tx`.
- **Rationale**: One connection serialises writers, so the per-project key counter
  (`UPDATE projects SET next_number = next_number + 1 ... RETURNING`) and rank neighbour reads
  cannot race. Human-speed traffic makes throughput irrelevant; idle memory is minimal.
- **Read pool**: searches and boards use a second, read-only connection pool (WAL readers do not
  block the writer, and `query_only` is on) with a 4 s deadline, 3 free-text terms at most and an
  offset cap of 10,000, so one expensive search cannot serialise the service. (QA finding.)
- **Cost**: a write waits for other writes; reads of single issues still share the writer connection. Acceptable here; a multi-node or high-write
  deployment would need Postgres (the `requires.database` field makes that a module-level choice).
- **Pitfall recorded**: with one connection, calling `s.db` inside a transaction deadlocks. Helpers
  therefore take a `dbq` (`*sql.DB` or `*sql.Tx`).

## R3. Fractional ranks
- **Decision**: Rank is a string over `0-9a-z` read as a base-36 fraction, compared
  lexicographically; ranks never end in `0`; `rankBetween(lo, hi)` returns a string strictly
  between. New issues rank after the project's maximum.
- **Respacing**: appends bump the first bumpable character (growth about one character per 18
  appends); when any rank exceeds 24 characters the project's ranks are rewritten evenly spaced, in
  order. A move leaves `version` and `updated_at` alone (it is not an edit and must not invalidate
  ETags) but is logged in the activity.
- **Rationale**: Reordering writes one row, with no renumbering and no locking of neighbours, and
  ordering is a plain index scan (`issues_project_rank`).
- **Limit**: repeated insertion at one spot grows the string by one character per about 35
  insertions; a rebalance job is not needed at this scale (property test: 2,000 random inserts
  and 200 front inserts stay sorted and unique).
- **Alternatives**: integer gaps with renumbering (writes many rows); linked lists (hard to query).

## R4. Search language
- **Decision**: A small tokenizer and a fixed table of fields, each producing a SQL fragment with
  bound parameters; unknown fields, long queries (500 chars), many terms (20) and many values (20)
  are errors. `LIKE` patterns escape `\`, `%` and `_`. Sort is a whitelist.
- **Rationale**: "Simple, safe, no SQL injection" with no parser dependency. Because values are
  never spliced into SQL, injection is structurally impossible, and the tests assert both that the
  payload travels as an argument and that the table survives.
- **Pagination**: offset cursors (opaque, base64) capped at 100,000, limit capped at 100. Offsets can
  shift under concurrent creates; keyset paging per sort order is a later improvement.
- **Alternatives**: JQL-style grammar with OR and parentheses (more power, a real parser, more
  surface); FTS5 (not in the pure-Go build by default).

## R5. Authentication and tokens
- **Decision**: Bearer tokens `cwt_<16 hex id>_<43 base64url secret>` (256 bits); store
  SHA-256(secret); compare in constant time; lookup by id; scopes `work.read|write|admin`;
  optional project list; personal tokens must expire (default 90 days, max 365); revocation and
  `last_used_at` (written at most once a minute). A bootstrap admin token comes from the
  environment. All behind `Authenticator`; `Chain` combines them.
- **Rationale**: A fast hash is right for high-entropy random secrets (no stretching needed) and
  keeps authentication cheap; the secret is never recoverable from the database. An unknown id and
  a wrong secret cost the same.
- **Not done**: user-delegated tokens minted by people themselves (needs SSO, E11), and OIDC or can-core
  introspection (the `Authenticator` seam is where it goes).

## R6. Hiding other projects
- **Decision**: A key outside the principal's project scope returns `ErrNotFound`, in the store, for
  every read and write path, including link targets, bulk keys, sprints, boards and search.
- **Rationale**: 403 would confirm that a key exists. The test compares the 404 body for a missing
  key with that for a forbidden one and requires them to be identical.
- **Residual**: timing differences between "missing" and "forbidden" are negligible but not
  eliminated (both are index lookups; the scope check happens before the database for out-of-scope keys).

## R7. Footprint (measured)
Static linux/amd64 build (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`, 12.3 MB binary),
run locally with an empty database:

| State | VmRSS |
|---|---|
| idle after start | 13,976 kB (13.6 MiB) |
| after creating 200 issues (with idempotency keys and labels), 100 searches of 100 rows and 100 board reads | 20,896 kB (20.4 MiB) |

Declared: `ram_mb: 20`, `ram_limit_mb: 48`. As with E05 the idle target is met approximately and
the limit has wide headroom; the figure after load is the one to watch. SPEC.md's about 40 MB
target for in-house `work` is comfortably beaten. Measured on the development machine (Linux),
not on a small server; the cluster measurement is part of the pending checks.

## R8. Webhooks and the outbox
- **Decision**: A transactional outbox table written in the same transaction as the change and a
  single dispatcher goroutine that signs and POSTs (HMAC-SHA256 over `timestamp.body`, headers
  `X-Can-*`, the forms scheme), retrying with doubling backoff from 30 s capped at 6 h for a 7-day
  delivery window, then dead-lettering (kept 90 days, logged, listed and replayable by an admin),
  no redirects, bounded response read. QA found the first design (8 attempts, about an hour) dropped
  events during any receiver outage longer than that.
- **Ordering**: events are taken oldest first but a failing one is retried later, so arrival order
  is not guaranteed; consumers use `version` and `time`. No webhook configured means nothing is queued.
- **Rationale**: E05's in-memory retry loses events on restart; here the SQLite outbox makes
  delivery at least once across restarts without a broker. When the can-core event bus lands the
  dispatcher is replaced by a publish call; the payloads do not change.
- **Events carry no bodies** (description, comment text) so a webhook receiver learns identifiers
  and changed field names, and fetches content through the API if its token allows. Updates report a
  changed description as `"(changed)"`.

## R9. Idempotency
- **Decision**: Key from `Idempotency-Key` (or the body's `idempotency_key`), scoped to the caller
  and, inside the hash, to the project and the request body. Stored in the same transaction as
  the create; a failed create stores nothing, so a corrected retry succeeds. Kept 7 days.
- **Behaviour**: replay returns the response stored at creation (not the current state), `200` with
  `Idempotent-Replayed: true`; same key plus a different body is `422 idempotency_mismatch`.

## R10. Hierarchy, links and workflow rules
- **Hierarchy**: epic, then story/task/bug, then subtask (the user-facing rule `epic -> story ->
  subtask`); a parent is in the same project. Because levels strictly decrease a cycle cannot form,
  but a recursive check still runs on re-parenting as defence in depth.
- **Links**: `blocks` and `duplicates` are directed and acyclic (recursive CTE reachability before
  insert); `relates` is symmetric and cycles are harmless.
- **Workflow**: statuses with categories (`todo`, `active`, `done`) and an explicit transition map,
  validated (names, initial not done, every transition targets a status, a done status is
  reachable from the initial status, and every status reachable from it can reach a done one, so
  issues cannot get stuck in a dead end). `resolved_at` is derived from the category, so custom workflows, retention and
  `is:open` work without knowing status names.
- **Status is not patchable**: only `POST .../transitions` changes it, so the workflow cannot be
  bypassed and the transitioned event is always emitted.

## R11. `$N` references in bulk create
- **Decision**: In a bulk request, `parent: "$N"` means the Nth earlier item.
- **Rationale**: A manager's planning list naturally says "these stories belong to that epic", and
  keys do not exist until the request is processed. The alternative (two calls) breaks atomicity and
  idempotency of the whole plan.

## R12. Threat notes
- Brute force: tokens are 256-bit; failed authentications from one address are answered 429 past a
  limit (default 30 per minute) while valid tokens are never throttled by it (a shared ingress
  address must not become a lockout lever); calls per token are limited (600 per minute). Behind a proxy set
  `trust_proxy` so the address is the real client's.
- Denial of service: request bodies, queries, list sizes, bulk sizes, label counts, attachments per
  issue, board column size and rate limiter maps are bounded.
- Injection: SQL (parameters only), CSV is not produced, HTML is not rendered (JSON API, CSP
  `default-src 'none'`), header injection (idempotency keys and attachment fields reject control
  characters), SSRF (attachment `url` and webhook redirects are never followed or fetched).
- Information leakage: logs omit bodies, queries and addresses; the audit log and events never
  hold secrets or bodies.
- Insider risk: the bootstrap admin token is all-powerful by design; mint scoped tokens and keep it
  in the Secret. Database administrators can read everything (SQLite file); backups arrive with E08.
