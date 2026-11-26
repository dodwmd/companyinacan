# Work tracker

`work` is the in-house, API-first issue tracker: projects, epics, stories, tasks, bugs and
subtasks, P0 to P3 priorities, configurable workflows, sprints and boards, comments, links,
watchers and a per-issue activity log. A manager's list of stories from a planning meeting is
one API call; a script that retries never creates duplicates.

Status: **experimental**. The service (`cmd/can-work`, `core/work`) is built and tested
offline and measured locally; the image has not been built or run on a cluster, and there is
a web UI (E14c, [work-ui.md](work-ui.md)) is served under `/ui/`. See
`specs/011-work-tracker/tasks.md` for the known gaps.

## Concepts

| Thing | Notes |
|---|---|
| Project | Key such as `ACME` (2 to 10 upper-case letters or digits). Owns a workflow, SLA hints and sprints. |
| Issue | Key `ACME-123`, numbered per project. Types: `epic`, `story`, `task`, `bug`, `subtask`. |
| Hierarchy | epic, then story/task/bug, then subtask. An epic has no parent; a subtask needs a story, task or bug; parents live in the same project. |
| Priority | `P0` drop everything, `P1` this sprint, `P2` planned (default), `P3` backlog. Each carries an SLA hint (response and resolution hours) that a project can override. Hints are advice; nothing escalates on them yet. |
| Status | Per-project workflow. Default: `todo`, `in_progress`, `in_review`, `done`, `cancelled`, with allowed moves (`todo` cannot jump to `done`). Statuses belong to a category (`todo`, `active`, `done`); entering a `done` status sets `resolved_at`. |
| Links | `blocks`, `relates`, `duplicates`. Self links, repeats and cycles are refused. |
| Version | Every issue has a `version` (also the `ETag`). Send `If-Match: "<version>"` to avoid overwriting someone else's edit. |
| Sprint | `planned`, then `active` (one per project), then `closed`. The board shows one column per status in rank order. |

## Tokens and scopes

Every call needs `Authorization: Bearer <token>`.

* **Bootstrap admin token**: `CAN_WORK_ADMIN_TOKEN` (32+ characters, high entropy; weak values are refused at start) from the module Secret. It
  holds every scope on every project. Use it to mint real tokens, then keep it out of daily use.
* **API tokens** (`cwt_<id>_<secret>`): minted with `POST /v1/tokens`. The secret is shown once;
  only its SHA-256 is stored. A token has a `kind` (`personal` for a person, `service` for a bot
  or integration), a `subject` (who it acts as: the reporter, comment author and actor recorded
  on everything it does), `scopes`, optional `projects`, and an expiry. Personal tokens always
  expire (default 90 days, at most 365). Revoking is immediate; `last_used_at` is recorded. The
  subject `bootstrap-admin` is reserved for the environment token.

| Scope | Allows |
|---|---|
| `work.read` | read projects, issues, boards, sprints, comments, activity |
| `work.write` | everything above plus create/update/transition/comment/link/rank/sprints (implies read) |
| `work.admin` | everything above plus projects, workflows, tokens and the audit log (implies write) |

A token limited to `projects: ["ACME"]` cannot see or touch any other project. A key outside
its scope returns **404**, exactly like a key that does not exist (no existence oracle). Only an
unscoped admin creates projects or lists and revokes tokens; a creator can only hand out scopes
and projects it holds itself.

```sh
export CAN_WORK_URL=http://127.0.0.1:8080          # the module's ClusterIP service, or a port-forward
export ADMIN=$CAN_WORK_ADMIN_TOKEN

# A project, then a service token for the planning bot, limited to it.
curl -sS -X POST $CAN_WORK_URL/v1/projects -H "Authorization: Bearer $ADMIN" \
  -d '{"key":"ACME","name":"Acme launch"}'

curl -sS -X POST $CAN_WORK_URL/v1/tokens -H "Authorization: Bearer $ADMIN" \
  -d '{"kind":"service","subject":"svc:planner","name":"planning bot","scopes":["work.write"],"projects":["ACME"]}'
# => {"token":"cwt_5bc59458ad8398d7_...","info":{...},"note":"store the token now; it is not shown again"}

export TOKEN=cwt_...      # the value above
curl -sS $CAN_WORK_URL/v1/me -H "Authorization: Bearer $TOKEN"
```

## Creating stories (safe to retry)

```sh
# One issue. The Idempotency-Key (8-128 chars, per caller) makes a retry return the original.
curl -sS -X POST $CAN_WORK_URL/v1/projects/ACME/issues \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: planning-2026-11-25-001" \
  -d '{"type":"story","title":"Customers can sign up with email","priority":"P1",
       "assignee":"bob@example.com","due_date":"2026-12-10","estimate":5,
       "labels":["web","onboarding"],"description":"Acceptance: ..."}'
# 201 Created, ETag: "1", Location: /v1/issues/ACME-1
# Repeat the same call: 200 OK with Idempotent-Replayed: true and the same issue.
# Same key, different body: 422 idempotency_mismatch.

# A manager's whole list from a planning meeting: all or none, one retry-safe call (max 100).
curl -sS -X POST $CAN_WORK_URL/v1/projects/ACME/issues/bulk \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: planning-2026-11-25-batch" \
  -d '{"issues":[
        {"type":"epic","title":"Q1 launch"},
        {"type":"story","title":"Signup","parent":"$0","priority":"P1"},
        {"type":"story","title":"Billing","parent":"$0","priority":"P2","assignee":"carol@example.com"}
      ]}'
# "parent": "$0" means "the item at index 0 of this request" (earlier items only), so a list can
# nest stories under an epic it creates without knowing the keys in advance.
```

The reporter is the token's subject; `status`, `key`, `reporter`, `version` and timestamps cannot
be set (the request is rejected with 400, naming the unknown field).

## Reading, searching and paging

```sh
curl -sS $CAN_WORK_URL/v1/issues/ACME-2 -H "Authorization: Bearer $TOKEN"        # with links, children, watchers

# Search. Terms are ANDed; quote the URL.
curl -sSG $CAN_WORK_URL/v1/issues -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'q=priority:P0,P1 is:open assignee:me -label:wip due<2026-12-31' \
  --data-urlencode 'sort=priority' --data-urlencode 'limit=50'
# => {"items":[...],"next_cursor":"Yzk"}   pass cursor=Yzk for the next page
```

| Term | Meaning |
|---|---|
| `status:todo,in_progress` | any of the listed values (also `priority:`, `type:`, `project:`, `reporter:`, `label:`) |
| `assignee:me`, `assignee:none` | the caller, or unassigned (`label:none`, `parent:none`, `sprint:none` work the same way) |
| `sprint:active`, `sprint:12` | the active sprint, or a sprint id |
| `parent:ACME-1`, `key:ACME-7` | children of an epic or story; one specific issue |
| `is:open` `is:resolved` `is:overdue` `is:blocked` | state shortcuts |
| `due<2026-12-01`, `created>=-7d`, `updated>today` | dates: `<`, `<=`, `>`, `>=`, `=` against `YYYY-MM-DD`, RFC 3339, `today`, `-12h`, `-7d`, `-2w` |
| `-status:done` | a leading `-` negates a term |
| `login "two words"` | bare words match title or description |

Queries are capped at 500 characters, 20 terms and 3 free-text terms (substring matches are the
expensive ones), run on a read-only connection pool with a 4 second deadline (a search that exceeds it is stopped with 503 `search_timeout`: narrow the query), and never block
writers. The language is parsed into fixed SQL
fragments and every value is bound as a parameter, so there is nothing to inject into.
`limit` is at most 100; `cursor` is opaque and offset based (a page can shift if issues are created
while you walk it) and stops at 10,000 results: narrow the query instead of paging deeper. Every
term is wrapped in parentheses and the token's project scope is ANDed onto the whole query, so no
combination of terms can return an issue outside the token's projects.

## Updating, transitioning, ranking

```sh
# Update. Absent fields are left alone, null clears assignee, due_date, estimate, parent, sprint_id.
curl -sS -X PATCH $CAN_WORK_URL/v1/issues/ACME-2 -H "Authorization: Bearer $TOKEN" \
  -H 'If-Match: "1"' -d '{"priority":"P0","assignee":null,"labels_add":["urgent"]}'
# Stale version: 412 version_mismatch. Re-read, merge, retry. (A "version": N in the body is the same guard.)

# Move along the workflow. What is allowed now:
curl -sS $CAN_WORK_URL/v1/issues/ACME-2/transitions -H "Authorization: Bearer $TOKEN"
curl -sS -X POST $CAN_WORK_URL/v1/issues/ACME-2/transitions -H "Authorization: Bearer $TOKEN" -d '{"to":"in_progress"}'
# An illegal move is 409 and lists the allowed ones.

# Change many at once (each key succeeds or fails on its own; max 100).
curl -sS -X POST $CAN_WORK_URL/v1/issues/bulk-update -H "Authorization: Bearer $TOKEN" \
  -d '{"keys":["ACME-2","ACME-3"],"set":{"priority":"P1","labels_add":["q1"]},"transition":"in_progress"}'

# Backlog order: put ACME-5 above ACME-2 (writes one fractional rank, no renumbering). A move
# does not change the issue's version or updated_at (your ETag stays valid) but is logged in
# its activity; ranks are respaced automatically when they get long.
curl -sS -X POST $CAN_WORK_URL/v1/issues/ACME-5/rank -H "Authorization: Bearer $TOKEN" -d '{"before":"ACME-2"}'
```

## Comments, links, watchers, attachments

```sh
curl -sS -X POST $CAN_WORK_URL/v1/issues/ACME-2/comments -H "Authorization: Bearer $TOKEN" -d '{"body":"Started on this."}'
curl -sS -X POST $CAN_WORK_URL/v1/issues/ACME-2/links    -H "Authorization: Bearer $TOKEN" -d '{"type":"blocks","target":"ACME-3"}'
curl -sS -X PUT  $CAN_WORK_URL/v1/issues/ACME-2/watchers/svc:planner -H "Authorization: Bearer $TOKEN"   # yourself; adding others needs work.admin
curl -sS $CAN_WORK_URL/v1/issues/ACME-2/activity -H "Authorization: Bearer $TOKEN"     # who changed what, before and after

# Attachments are metadata only: the tracker stores no file bytes yet. url is never fetched.
curl -sS -X POST $CAN_WORK_URL/v1/issues/ACME-2/attachments -H "Authorization: Bearer $TOKEN" \
  -d '{"filename":"mockup.png","content_type":"image/png","size_bytes":48213,"url":"https://files.example.com/mockup.png"}'
```

## Sprints and boards

```sh
curl -sS -X POST $CAN_WORK_URL/v1/projects/ACME/sprints -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"Sprint 1","goal":"Signup works","starts_on":"2026-11-30","ends_on":"2026-12-11"}'
curl -sS -X PATCH $CAN_WORK_URL/v1/issues/ACME-2 -H "Authorization: Bearer $TOKEN" -d '{"sprint_id":1}'   # plan it in
curl -sS -X POST  $CAN_WORK_URL/v1/sprints/1/start    -H "Authorization: Bearer $TOKEN"
curl -sS $CAN_WORK_URL/v1/projects/ACME/board         -H "Authorization: Bearer $TOKEN"   # ?sprint=backlog|all|<id>
curl -sS -X POST  $CAN_WORK_URL/v1/sprints/1/complete -H "Authorization: Bearer $TOKEN" -d '{"move_to":2}'  # or {} for the backlog
# Issues that move get a new version, an activity entry and a work.issue.updated.v1 event (changes.sprint_id).
```

## Workflows and SLA hints

```sh
curl -sS -X PATCH $CAN_WORK_URL/v1/projects/ACME -H "Authorization: Bearer $ADMIN" -d '{
  "workflow":{"initial":"open",
    "statuses":[{"name":"open","category":"todo"},{"name":"qa","category":"active"},{"name":"shipped","category":"done"}],
    "transitions":{"open":["qa"],"qa":["open","shipped"]}},
  "sla":{"P0":{"respond_hours":1,"resolve_hours":8},"P1":{"respond_hours":8,"resolve_hours":72},
         "P2":{"respond_hours":72,"resolve_hours":720},"P3":{"respond_hours":168,"resolve_hours":2160}}}'
```

A new workflow must keep every status that issues are in (422 lists the missing ones). Default
SLA hints: P0 1h/24h, P1 8h/7d, P2 3d/30d, P3 7d/90d (respond/resolve).

## Command line: `can work`

For managers and automation that prefer a command to curl. It reads `CAN_WORK_URL` (default
`http://127.0.0.1:8080`) and `CAN_WORK_TOKEN` from the environment, so the token never appears
on the command line.

```sh
export CAN_WORK_URL=http://127.0.0.1:8080 CAN_WORK_TOKEN=cwt_...

can work create ACME --type story --title "Customers can sign up" --priority P1 \
    --assignee bob@example.com --due 2026-12-10 --label web --idempotency-key planning-001
can work bulk-create ACME --file stories.json --idempotency-key planning-batch-001
can work list -q 'priority:P0,P1 is:open' --all          # follow pages
can work get ACME-2
can work update ACME-2 --priority P0 --assignee none --add-label urgent --if-match 3
can work transition ACME-2 in_progress
echo "Shipped to staging." | can work comment ACME-2 --body -
can work --output json list -q 'assignee:me' | jq '.[].key'
```

Creates carry an idempotency key (random per run unless you pass one), and the client retries a
create after a network error or 5xx only because of that key. Exit codes: 0 ok, 1 server or
network error (the message includes the API's code and field problems), 2 usage.

## Importing from CSV

`can import issues backlog.csv --project ACME` previews and `--apply` writes the whole file or nothing
(`CAN_WORK_URL`, `CAN_WORK_TOKEN`). Each row needs `external_id` and `title`; `parent_external_id` links
rows, in any order. Running the same file again changes nothing, and never moves an issue's status. It needs
`work.admin` (the file sets the status of new issues) on a token that is not limited to projects. Every new
issue publishes `issue.created.v1`. There is no web page for this yet. See
[specs/047-csv-import](../specs/047-csv-import/spec.md).

## Events and webhooks

Set `webhook.url` (and `CAN_WORK_WEBHOOK_SECRET`) and every change is queued in a transactional
outbox, in the same database transaction as the change, then POSTed:

| Event | When | Schema |
|---|---|---|
| `work.issue.created.v1` | an issue is created | `schemas/events/work.issue.created.v1.json` |
| `work.issue.updated.v1` | fields change (`changes.priority` shows a priority move) | `.../work.issue.updated.v1.json` |
| `work.issue.transitioned.v1` | a status move, with `to_category` | `.../work.issue.transitioned.v1.json` |
| `work.comment.created.v1` | a comment is added | `.../work.comment.created.v1.json` |

Events carry identifiers and changed fields, never comment or description text. They do carry
the issue **title** (created event) and any changed title or assignee, so treat the webhook
receiver as trusted with that much. This is a recorded decision, not an oversight: a notification
or chat consumer needs a human-readable label, and the alternative (identifiers only, so every
consumer calls back for the title) would need an event schema change and an extra authenticated
read per event. Titles are one line of plain text (control characters are refused), so they cannot
split a log line or forge a field in a receiver; description and comment text never leave the
service. A deployment that must keep titles private should not set a webhook URL. Headers:
`X-Can-Event-Id`, `X-Can-Event-Type`, `X-Can-Timestamp`, and
`X-Can-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))` (the same scheme as
the forms gateway). Delivery is at least once, per event, with caveats you should design for:

* Failures (network error, 5xx, 429, any non-2xx) are retried with exponential backoff (30 s,
  doubling, capped at 6 h) for **7 days**. The queue is the SQLite outbox, so a restart loses nothing.
* After 7 days an undelivered event is *dead-lettered*, not dropped: it is logged at error level
  ("webhook event dead-lettered"), listed by `GET /v1/events/dead` (admin, paged with `cursor`) for 90 days, and can be
  put back with `POST /v1/events/{id}/replay` (same event id, fresh 7-day window). Poll the dead
  list (or alert on that log line) so a broken receiver is noticed.
* **Ordering is not guaranteed** once a retry is involved: a later event can arrive before an
  earlier one that is waiting to be retried. Use the `version` in `work.issue.updated.v1`, the
  `time` of each event, or re-read the issue, instead of assuming arrival order.
* Redirects are not followed. De-duplicate on the event `id` and reject timestamps older than a
  few minutes (replayed events are re-signed with a current timestamp).

## Limits, rate limits and audit

| Limit | Default |
|---|---|
| Request body | 1 MiB (`max_body_bytes`, 413 above) |
| Requests per token | 600 per minute (`rate_limit.per_minute`, 429 with `Retry-After`) |
| Failed authentications | 30 per minute per client address; further *bad* tokens from that address get 429 after a 300 ms delay. A valid token always works, so a shared address cannot be used to lock others out |
| Title / description / comment | 200 / 50,000 / 20,000 characters |
| Labels | 20 per issue, `[a-z0-9][a-z0-9._:-]{0,39}` |
| Bulk operations | 100 items |
| Attachments | 50 per issue, 1 GiB declared size |

`GET /v1/audit` (unscoped admin) lists admin actions newest first: token create and revoke,
project create and update (including workflow and SLA changes), with the actor. Secrets are
never recorded. Issue-level history is `GET /v1/issues/{key}/activity`.

## Privacy and retention

Stored: issue text, assignee and reporter identifiers (the `subject` of a token; for people use
their email or directory id), comments, activity, attachment metadata, hashed tokens, the audit
log. The access log omits query strings, client addresses and bodies. Resolved issues (with
their comments, links and activity) are deleted `retention_days` after resolution (default 5
years; 0 keeps them), audit entries after `audit_retention_days` (default 7 years); open issues
are kept, and so is a resolved parent that still has a child to keep (no subtask is orphaned). A
purge runs hourly. The manifest's `data_classes` mirror these defaults.

**Erasing a person.** The tracker holds a person as the identifier they appear under: reporter, assignee,
comment author, activity actor, watcher, who added a link or attachment, API tokens, and copies of these in
activity details, cached replies and events.

```sh
curl -X POST "$CAN_WORK_URL/v1/people/erase" -H "Authorization: Bearer $CAN_WORK_TOKEN" -d '{"subject":"user:alice"}'
```

It needs `work.admin` and a token not limited to projects. The identifier is replaced by a random pseudonym,
the person's tokens are revoked, watcher entries and cached replies are deleted, and the database is scrubbed.
**Not changed:** titles, descriptions, comment texts and file names that mention the person, and the admin
audit log. Each spelling is a separate identifier. No event is published, so copies elsewhere (webhook
receivers, can-core's queued events and notifications) stay. The erasure is recorded as a hash of the
identifier (`GET /v1/erasures`) for the erasure ledger, and `can restore` repeats it with `can-work purge` on
a backup made before it ([specs/056](../specs/056-erasure-finance-work/spec.md)).

## Operating it

```sh
CAN_WORK_ADMIN_TOKEN=... can-work serve                       # defaults: :8080, /data/work.db
can-work check-config -config work.yaml                       # validate without starting
can-work healthcheck -addr 127.0.0.1:8080                     # for container probes
```

`work.yaml` is optional; every key is documented in `modules/work/README.md`. Footprint:
13.6 MiB RSS idle and 20.4 MiB after 200 creates and 200 searches for the API alone; with the web UI see `modules/work/README.md` (declared 28 MB, limit 64 MB).

## What is not here yet

No attachment upload or file storage (the UI shows attachment details), no UI sign-in through SSO (the UI takes a pasted token, see [work-ui.md](work-ui.md)), no per-person SSO (tokens are minted by
an admin; can-core introspection plugs in behind the `Authenticator` interface), no saved
filters or components, no time tracking, no source-control links, no `issue.priority_changed`
event or manager notifications (use `changes.priority` on `work.issue.updated.v1`), and no
Plane wrapper. See `specs/011-work-tracker/tasks.md`.
