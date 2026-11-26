# People and performance (`can-people`)

The in-house `people` module holds the company's **directory** (people records, org structure,
employment lifecycle, onboarding and offboarding checklists), **performance** data (1:1s, goals and
review cycles with 360 feedback) and **time off** (leave types, balances, requests, calendars). It provides the `directory` and `performance` capabilities and is the first module that stores
the most sensitive data classes in the platform.

> **This is a tool, not legal, HR or payroll advice.** It records and protects data; it does not
> compute pay or tax, and it does not know your jurisdiction's employment, privacy or retention
> law. The retention defaults below are starting points. The operator is responsible for compliance
> and should take advice from a qualified professional.

Status: **experimental**. Unit-, contract- and race-tested offline; not yet exercised on a cluster
(see [Gaps](#gaps)). Epic E13a, spec in `specs/012-people-directory/`; reviews and time off are epic
E14b, spec in `specs/022-reviews-timeoff/`.

## What it does

| Area | Features |
|---|---|
| Directory | People (employee, contractor) with a lifecycle `candidate -> onboarding -> active <-> leave -> offboarding -> former`, effective dating (future-dated changes apply on their date), status history, departments, teams, positions, a manager chain with cycle prevention |
| Fields | `directory-basic` (names, work contact, org, dates) and `people-sensitive` (date of birth, national id, home address, personal phone, salary). Sensitive fields are encrypted per field at rest and every read is audited per field |
| 1:1s | A series between a manager and one direct report, meetings, agenda items either side can add, shared notes, **manager-private notes**, action items that can link to a `work` issue, follow-ups from earlier meetings, history |
| Goals | Personal, team and company goals with key results, progress updates, alignment to a manager's, team's or company goal, and four visibility levels (private, manager, team, company) |
| Checklists | Onboarding and offboarding templates (assignee role, due date relative to an anchor, optional machine `action`), instantiated manually or automatically when a person enters a status; completion emits an event for later identity provisioning |
| Reviews | Templates (rating and text questions), cycles `draft -> open -> calibration -> closed`, self-assessment, manager review, peer and upward feedback with manager-approved nominations and per-cycle anonymity (never fewer than 3 answers shown), calibration notes and rating (HR only), release approved by a different HR person, acknowledgement, history, read-only links to the subject's goals. See [Reviews](#reviews) |
| Time off | Leave types with accrual policy as data (annual grant, monthly accrual, carry-over cap, negative balance allowance), an append-only balances ledger with as-of dates, requests approved by the manager (half days, blackouts, overlap checks), public holiday calendars per location, a team calendar showing only "out", cancellation with reversal entries, long leave moving the employment status. See [Time off](#time-off) |
| Retention | Per data class retention, legal hold, erasure requests that tombstone and purge, an erasure ledger feed |
| API | REST JSON, OpenAPI 3.1 at `specs/012-people-directory/contracts/people-api.yaml`, bearer tokens, one policy layer |

Not included: payroll (never built, see SPEC.md), pay or tax computation, statutory leave rules, praise,
the admin and portal UI, search and reporting.

## Importing from CSV

HR or an admin can load existing staff from a CSV. The first command only previews; `--apply` writes the
whole file or nothing:

```sh
export CAN_PEOPLE_URL=http://127.0.0.1:8080 CAN_PEOPLE_TOKEN=...
can import people staff.csv --map external_id="Employee No"
can import people staff.csv --map external_id="Employee No" --apply
```

Each row needs `external_id`, `given_name` and `family_name`. Running the same file again changes nothing,
and empty cells never clear a value. New rows default to active employees and cannot carry sensitive fields.
Active rows are treated as existing staff: they publish no `person.created.v1`, so identity provisioning
starts no joiner run for them (`--joiners` changes that; `onboarding` rows always publish it). Read the
preview's notes before applying. There is no web page for this yet.

**When someone imported this way leaves, check the leaver run.** Provisioning has no record of their
accounts, so it can only look them up (SCIM connectors, by work email) and otherwise ends the run failed
and lists it in its dead letters, naming each connector, and can-core if no `sso_subject` is linked. Remove
that access by hand, or link the accounts and retry. See
[provisioning](provisioning.md#accounts-provisioning-did-not-create).
Columns, limits and rules: [specs/047-csv-import](../specs/047-csv-import/spec.md).

## Roles and who can see what

Roles come from the caller's token (SSO groups): `employee`, `manager`, `hr`, `admin`. A role is
necessary but never sufficient: the policy layer (`core/people/policy.go`) also checks the caller's
relationship to the record (self, direct manager, anywhere in the manager chain, same team) and the
caller's own employment status. Anything not allowed is denied.

**Personal access tokens of managers (module 0.3.0).** A can-core token or launch session is a manager only
if it carries `people.team.manage`, which only the manifest's `manager` role holds. After upgrading, run
`can apply` (or let can-core reconcile) so the 0.3.0 manifest and its roles are recorded, then **every manager
must issue new personal access tokens with `people.team.manage` ticked**: tokens issued before keep their old
permissions and act as employees only. An operator who overrode the `manager` role's permissions adds
`people.team.manage` to it.

| Data | self | direct manager | skip-level manager | hr / admin | other staff |
|---|---|---|---|---|---|
| Basic fields | yes | yes | yes | yes | yes, for active, on-leave and onboarding people |
| Date of birth, national id, home address, personal phone, salary | read; write only address and phone | **no** | **no** | read and write | no |
| Status history | yes | no | no | yes | no |
| 1:1 agenda, shared notes, actions | yes | yes | **no** | **no** | no |
| Manager-private 1:1 notes | **no** | yes (the author) | **no** | **no** | no |
| Goals | always own | `manager`, `team`, `company` visibility | same | all but `private` | `team` (teammates), `company` |
| Checklists | yes | yes | no | yes | no |
| Create people, change status, set manager, legal hold, erasure, retention | no | no | no | yes | no |

Rules worth knowing:

- **Fail closed.** No recognised role, or a person record that is a candidate or former, gets
  nothing. A terminated manager's token may stay valid for a while upstream; their person record says
  `former`, so every call is refused. When a person becomes former, their reports lose the
  reporting line and HR must reassign them.
- **Elevated roles need an active person.** `manager`, `hr` and `admin` powers are suspended while the
  person is on leave or offboarding; self-service stays. The can-core service account (hr + admin, no
  person record) is exempt from this because it has no employment status; it never gets manager powers.
- **Leave is masked.** Colleagues see someone on leave as `active` (it can reveal health or family
  circumstances); the person, their manager chain and hr/admin see `leave`. The `status=leave` list
  filter is applied after masking, colleagues see `created_at` in place of `updated_at`, and
  `person.status_changed.v1` masks it the same way: `leave` is published as `active` and a change between
  `active` and `leave` publishes nothing. A consumer that needs to know about leave (payroll, say) must read
  the directory as hr.
- **identity.\* checklist actions** (for example `identity.provision`) cannot be assigned to the person
  themself and nobody can complete one about themself. A `manager`-role task always resolves to the
  person's *current* manager when read or completed.
- **Effective dates are UTC calendar dates** (`YYYY-MM-DD`): a change dated today takes effect at once, a
  future one at 00:00 UTC on its date, whatever the operator's or the person's local time zone.
- **Access ends on the effective date.** A due, future-dated status change is applied when the affected
  person next authenticates and by a one-minute background tick, not an hour later.
- **404, not 403,** when the caller may not see the record at all, so ids cannot be probed.
- **A manager who is replaced loses access** to the old 1:1 series immediately, private notes
  included. The new manager starts a new series and sees nothing of the old one. The report keeps
  their history.
- **HR and admin cannot read 1:1 content or private goals by role.** If your policy needs HR to see
  them, that is a deliberate product change, not a config flag.
- **Operators and database administrators can still read the database file.** Sensitive fields and
  all 1:1 notes are encrypted at the application level, so a copy of the file without the keyring is
  not enough, but whoever holds both the file and the keyring can read everything.

## Reviews

Permissions `people.reviews.read|write|admin`; the policy layer (`Decide`) decides everything, so a
role is necessary and the relationship, the cycle state and the review state are checked on every call.

| Part | Subject | Direct manager | Skip-level | HR / admin (not the subject) | Anyone else |
|---|---|---|---|---|---|
| The review exists, its status | yes | yes | released only | yes | 404 |
| Self-assessment | yes | once submitted | no | yes | no |
| Manager review | after release (answers only; not the proposed rating) | yes | no | yes | no |
| Calibration rating and notes | never | no | no | yes | no |
| Final outcome (rating, summary) | after release | after release | after release (summary only) | yes | no |
| Peer and upward feedback | aggregates after release | after the cycle leaves open, past the threshold | no | after the cycle leaves open, past the threshold | no |
| Feedback comments | never | after the cycle leaves open, past the threshold | no | after the cycle leaves open, past the threshold | no |

A personal access token from can-core (a *restricted* actor) additionally needs the matching permission for
each action, on top of its role: `people.reviews.read|write|admin`, `people.timeoff.read|write|approve|admin`
(`actionPerm` in `policy.go`; a test checks that every new action is mapped and that a token with only the
older people and performance permissions gets none of them).

HR and admin have no review power over their **own** review (they are treated as the subject), and a manager
on leave or terminated has none at all. Ratings, answers, summaries, calibration, feedback and
acknowledgement comments are sealed per part with an AAD binding them to their row. Every read is audited
before it is returned (fail closed); `touched_by` and release history are SHA-256 hashes, never names.

**Release.** HR freezes the outcome (the calibrated rating, else the manager's) and requests release; a
different HR person approves. The approver cannot be the subject, the manager who wrote the review, whoever
calibrated it, or anyone who ever requested its release; rejecting and re-requesting does not clear that.
Subjects are compared after NFKC normalisation, removal of invisible characters and case folding, on SSO
subject and person id alike, by one function. With an approval engine configured (type `review-release`)
the engine must have approved and its decider is held to the same rule. Approving publishes
`review.released.v1` (ids only); the subject then reads the outcome and acknowledges once.

**Feedback and anonymity.** The employee proposes peers, the manager approves or declines (or adds
directly); upward feedback comes only from direct reports and is added by the manager or HR. Reviewers see
the subject and the questions, never who nominated them. The employee sees only the nominations they made.
In an **anonymous** cycle a stored answer has no reviewer and no timestamp and a random id (the table is
`WITHOUT ROWID`, so physical order is the order of the random ids, not of submission); comments are
returned in an order derived from a hash of answer and question, different per question; no ETag or
`Last-Modified` is sent. Feedback is revealed **once**, when the cycle leaves `open`, and only if at least
`min_responses` answers exist (default 3, never lower; **upward** feedback needs at least 5). It is never
shown incrementally: while the cycle is open the view does not change when another person answers (not even
a count), because a view that moves lets a watcher compute the new answer from the change in the average.
The threshold applies per question: a question that fewer than the required number of people answered is omitted entirely (no average, no count, no comments), because answers are optional and a lone answer would be exact.
Nobody sees which reviewer has answered: a nomination reads `approved` whether or not it was answered
(only the reviewer sees their own open requests). In an anonymous cycle the audit entry for an answer names
the cycle, not the request or the person. After release the employee sees aggregates, never comments. In an **attributed** cycle the manager and HR also see
who wrote each comment. The audit entry for an anonymous answer carries its arrival time against the cycle, so in a cycle with one
participant (or whose answers someone can watch arrive) the audit log still shows when answers came in, though not from whom.
Limits: anonymity holds against API users, not against someone who can read the
database file and the WAL (write order, file offsets) together with the outbox and request tables, and free
text can identify its author by style or content. A threshold of k = 3 does not stop collusion: if k-1 of
the reviewers pool what they know, the last one is exposed; and a manager who asked exactly three peers knows
the answers came from those three. Raise `min_responses` for small teams or do not run anonymous feedback
for them.
Anonymous feedback is not linked to its author, so erasing the author cannot remove it (it names nobody).

## Time off

Permissions `people.timeoff.read|write|approve|admin`. **Entitlements and statutory rules vary by
jurisdiction: leave types, accruals, holidays and blackouts are data the operator enters, with no legal
rule engine; this is not legal advice.** Units are half days (`halves`; 2 = one day).

- **Leave types** (`POST /v1/leave-types`): `key` (vacation, sick, parental, unpaid or any custom), `accrual_mode` (`none` = untracked, no
  balance; `annual`; `monthly`), `grant_halves`, `carryover_cap_halves` (omit = unlimited), `allow_negative` with
  `negative_floor_halves`, `ignores_blackouts` (sick leave), `sensitive`, `sets_leave_status` with
  `leave_status_min_halves`. `sensitive` marks types such as sick and parental leave: reads of such requests are
  tagged `sensitive-type` in the audit log (colleagues never see a type anyway). Manually granted leave is `annual` with a grant of 0 and adjustments.
- **Ledger.** Append-only (database triggers): `grant`, `accrual`, `expire`, `use`, `reversal`, `adjust`; balance as of a date =
  sum of entries dated on or before it. Accruals run hourly and on demand (`POST /v1/timeoff/accruals/run`) for
  the current year, keyed by person, type, kind and period, so a rerun writes nothing; a balance above the cap
  expires at the turn of the year, before the new grant. Dates and the year turn use **UTC** (`today()`), so
  near midnight a person east or west of UTC can see a request refused or a grant arrive a few hours off from
  their local date. A `use` entry is dated at the request start: leave approved in December for January is
  charged to January, after the carry-over cap has been applied to the old year's balance, so it can be
  charged twice in effect (the cap may expire days the leave then consumes); correct with an adjustment. No proration: HR adjusts (`POST /v1/people/{id}/leave-adjustments`,
  never for one's own record; the reason is sealed and readable by the person and HR, not the manager). `use` is dated at the request's start date.
- **Requests.** `draft -> pending -> approved | rejected | cancelled`. Submitting computes working days from
  the person's location calendar (`POST /v1/holiday-calendars`: location matched case-folded, work week and
  holidays are data) and refuses, in one transaction, an overlap with another pending or approved request
  (half-day exact), a blackout period (company or team; not for types that ignore them), no working days, or
  an insufficient balance (pending requests reserve it). The check repeats on approval. Because the service
  holds one database connection, concurrent submissions and approvals serialise (tested with goroutines).
- **Approval.** The live direct manager decides; HR/admin can decide **any** request (not only when there is no
  manager: it is the fallback and the escape hatch, and every decision is audited); nobody
  decides their own request, with the same single separation function as reviews. With an approval engine
  (type `timeoff`, default chain `manager`) the engine must have approved and its decider is held to the rule.
  A rejected request is final. A manager's own request goes to their manager.
- **Cancellation.** The person until the leave starts, HR afterwards; an approved cancellation appends a
  `reversal` for the days not yet taken, taken from the half-day slots stored when the request was
  approved and capped at the `use` entry: a holiday added or removed since then neither mints nor loses
  balance. Both publish events.
- **Privacy.** The leave type and note (sealed) are visible to the person, their direct manager and HR only;
  skip-level managers and colleagues get 404. The **calendar** (`GET /v1/timeoff/calendar`, at most 92 days)
  shows colleagues only `out`: no type, reason, request id, status, and no pending requests. Long leave of a
  type with `sets_leave_status` moves the person to the `leave` status while it runs (and back), through the
  normal status machinery: colleagues see `active` and an unchanged `updated_at`, `person.status_changed.v1`
  is not published for it, the status history has no note, and a leave status that HR set is never reverted
  by the reconciler. On the calendar such leave looks like any other absence.
- **Events.** `timeoff.approved.v1` and `timeoff.cancelled.v1` carry ids, dates and a half-day count, never
  the type. **Dates reveal absence**: subscribe only the modules that need them (timesheets, to mark
  non-working days); do not fan them out. The finance module's side is not built.

## Encryption at rest

Sensitive columns (`people.sens_*`) and 1:1 notes are sealed with XChaCha20-Poly1305. The ciphertext
is bound to its row and column (`<person id>:<field>`, `<meeting id>:<notes kind>`) so a value copied
to another record does not open. The format is `v1.<key-id>.<base64url(nonce||ciphertext)>`.

The keyring is a file mounted from a Secret (`/etc/can-people/keys/keyring`, mode 0400; the service
refuses to start if it is readable by group or others):

```
# newest key first; the first line is the active key
k2 <64 hex characters>
k1 <64 hex characters>
```

The key file must be a **regular file** owned by the service user or root with mode 0400/0600: a symlink
is refused. Kubernetes Secret volumes expose files through `..data` symlinks, so mount the one key with
`subPath` (the chart values show it). The key id is carried in the ciphertext but is not part of the
authenticated data, so swapping the id inside a stored value can only make decryption fail, never succeed
under a different key; binding it into the AAD would change the stored format and is left for a later
format version.

Rotation: add a new key on top, restart, run `can-people rotate-keys` (it re-seals every value
written under an older key and prints how many), repeat until it reports 0, then remove the old key.
Re-sealing is a compare-and-swap, so a value written while rotation runs is never overwritten with a stale
copy (the pass retries). Status-history notes are rotated too. Removing a key that still protects data makes that data unreadable. Back up the keyring separately from
the database; **losing it loses the sensitive data**. Generate a key with `head -c 32 /dev/urandom | xxd -p -c 64`.

Also sealed: the free-text `note` on status changes. Not encrypted: names, work contact, dates and ids
(`directory-basic`), 1:1 agenda items and action items, goals. Protect the volume and backups as sensitive anyway (encrypted backups only, SPEC.md section 5).

## Audit

`Auditor` is an interface (`core/people/audit.go`). The default writes an append-only, hash-chained
`audit_log` table; can-core can supply an implementation that forwards to its own log. Recorded:

- every read of sensitive fields (per field names, never values), every read of a 1:1 meeting, every
  read of someone else's goal;
- every write, and every **denied** access attempt on sensitive resources;
- the actor is a person id (or `svc:<subject>`), never a name.

Reading a person's status history is audited too. Denial records are capped per actor (30 a minute, then
one "deny-throttled" notice) so a misbehaving client cannot fill the log. The audit table refuses UPDATE and
DELETE through the service (triggers), and rows are hash-chained. **That is not strong tamper evidence:**
anyone who can write the database file can drop the triggers and recompute the whole chain. To detect
that, keep the latest hash somewhere they cannot write (can-core's audit log, or periodic `verify-audit`
output stored elsewhere). An HMAC keyed outside the database is a possible later hardening.

A sensitive read happens **only after** its audit record was written: if the audit hook fails, the read
fails. Write audit is recorded after the commit, so a failure there is reported as an error although the
write persisted. `can-people verify-audit` recomputes the chain. The audit log is not erased with the
person (legal retention beats erasure; it holds opaque ids only).

## Retention, legal hold and erasure

Retention is per data class, from the day a person becomes former (1:1 meetings and closed goals
also expire by age). Defaults (override in `people.yaml` under `retention:` or in the module settings):

| Class | Sensitivity | Default |
|---|---|---|
| `directory-basic` | standard | 7y |
| `people-sensitive` | sensitive | 7y |
| `people-checklists` | standard | 2y |
| `performance-oneonone` | sensitive | 2y |
| `performance-goals` | sensitive | 2y |
| `performance-reviews` | sensitive | 2y (closed cycles also expire by age) |
| `people-timeoff` | sensitive | 7y (review against local payroll and leave record rules) |

Syntax is `30d`, `12m`, `7y`, `0` (purge as soon as the person leaves) or `legal` (never purged
automatically). The sweep runs hourly and on demand (`POST /v1/retention/run`, `can-people retention`).

- **Legal hold** (`PUT /v1/people/{id}/legal-hold`, hr or admin) blocks erasure and every retention purge
  for that person.
- **Erasure** (`POST /v1/people/{id}/erasure`, hr or admin) works only on former people and candidates
  and is refused (409) under a legal hold. It purges all five classes, blanks the record into a
  tombstone (the row stays, status `former`, names replaced, SSO link removed), and records
  `(subject, class, date)` for each class. The **subject is the person's own random id**
  (`pers_<24 hex>`): opaque, and it satisfies the E08 ledger's subject and class patterns
  (`core/backup/ledger.go`). `GET /v1/erasures?after=<seq>` returns the tombstones for can-core to append
  to the erasure ledger.
- **After a restore**, replay the ledger with `can-people purge -subject <id> -class <class>` (or
  `-class all`). It is idempotent, ignores legal holds (the erasure was authorised when it was
  recorded) and is a no-op for an unknown subject.
- **No data left behind in the files.** Every erasure and every retention sweep that purged something then
  runs `VACUUM` and `PRAGMA wal_checkpoint(TRUNCATE)`, because `secure_delete` alone does not scrub the
  write-ahead log; a test greps `people.db` and `people.db-wal` for the erased name, email and note.
  The erasure is written to the audit log before the scrub; if the scrub fails (for example a long-lived
  reader blocks the checkpoint) the request returns an error saying the person is erased and audited, and
  repeating it is idempotent and retries the scrub.
  **Backups and snapshots taken earlier still contain the data** until they expire (default horizon 35
  days, SPEC.md section 3.7). The erasure ledger (E08, `core/backup/ledger.go`) is how a restore re-applies
  the erasure: `purge` is replayed before workloads start.
- **Erasing a manager deletes their 1:1 series**, including the report's side of those meetings (shared
  notes, agenda, actions): a series is personal data of both participants and cannot be kept without the
  erased party. Erasing a *report* likewise deletes the series from the manager's view.
- **Residue that remains by design:** the status history's transitions and dates (notes are cleared), audit
  entries (opaque ids and field names), the tombstone row itself, and the `erasures` rows. Attribution
  columns (`created_by`, `done_by`, `recorded_by`, `author_id`) that named the erased person are replaced
  with `erased`; goal-update comments they wrote are cleared.
- `person.erased.v1` is published (once) so consumers can drop their own copies by `party_id`.
- **Kept on purpose:** the status history's dates and transitions (employment records are often legally
  required; notes are cleared), and the audit log. Event payloads contain only ids and machine values.
- Backups keep erased data until the last snapshot containing it expires (SPEC.md section 3.7).

## Events

Delivered at least once through the outbox (webhook, HMAC-signed with the same scheme as the forms
gateway), de-duplicate on `id`. Payloads carry ids and machine values only. Schemas in
`schemas/events/`:

`person.created.v1`, `person.status_changed.v1` (when a change takes effect, with `effective_date`),
`person.offboarded.v1`, `person.erased.v1`, `oneonone.scheduled.v1`, `goal.updated.v1` (never for a
`private` goal: even an id-only event would reveal that it exists), `checklist.task_completed.v1`
(carries the task's `action`, for example `identity.provision`, so a provisioning integration can react),
`review.cycle.opened.v1`, `review.released.v1` (ids only: never a rating or text), `timeoff.approved.v1` and
`timeoff.cancelled.v1` (ids, dates and a half-day count, never the leave type; dates reveal absence, see Time off).

A consumer that wants "hired" subscribes to `person.status_changed.v1` and filters on `to: active`
(`from: onboarding` for a first start; `from: offboarding` is a rescinded notice; leave never appears, it is masked as `active`, see Roles above). The `from` and `to`
fields are in the schema for exactly this.

SPEC.md names `person.hired.v1` and `person.left.v1`; this module publishes the finer-grained
`person.created`, `person.status_changed` and `person.offboarded` instead (a hire is `status_changed` to
`active`; `person.offboarded` carries the `effective_date` that `person.left` was specified to carry).

## Running it

```
can-people serve -config /etc/can-people/people.yaml
can-people check-config | healthcheck | rotate-keys | retention | verify-audit | purge -subject ID -class CLASS | version
```

```yaml
# people.yaml (secrets are never in this file)
listen: ":8081"              # ClusterIP only; there is no public listener
db_path: /data/people.db     # SQLite, WAL, secure_delete on
keyring_file: /etc/can-people/keys/keyring
introspect: { url: "" }      # can-core token introspection; empty = service account only
webhook: { url: "" }         # event delivery target; empty = events wait in the outbox
retention: {}                # per class overrides
reviews: { min_responses: 3 } # default anonymity threshold for new cycles (3..50)
```

Environment: `CAN_PEOPLE_API_TOKEN` (required, 24+ characters; the can-core service account, roles hr + admin),
`CAN_PEOPLE_WEBHOOK_SECRET` (required when a webhook is set), `CAN_PEOPLE_INTROSPECT_TOKEN`.

Requests are rate limited per subject, and requests whose credentials **fail** are limited per address
(429 with `Retry-After` once the bucket is empty). Authentication always runs first, so a flood of bad
tokens from an address never blocks a request that presents a valid credential. The limiter tables are
bounded: when a table is full it is cleared, which forgives every tracked address at once, so this slows
guessing and scraping but is not a substitute for network policy. `introspect.url` must be https or loopback unless
`introspect.allow_insecure_http` is set for a protected in-cluster Service; the endpoint receives every
user's bearer token. Introspection answers are cached in a bounded LRU, refusals for 5 s. Note that every *distinct* bad token triggers an outbound introspection call before any limit applies (the cache only absorbs repeats of the same token for 5 s), so keep the introspection endpoint able to take that load or front it with network policy.

Authentication is an interface (`Authenticator`). Shipped: static bearer tokens (constant time) and an
introspection client (`POST {"token"}` -> `{"active","subject","roles"}`, unknown roles dropped, positive
answers cached 30 s, every error denies). A user's `subject` is linked to a person through
`sso_subject`; a token whose subject has no person record gets no staff access. Nobody sets, changes or clears
the link of their own account, because equity relies on it to know who a caller is
([equity.md](equity.md#holder-identity)); another administrator or the service token does. hr and admin may
filter `GET /v1/people?sso_subject=`.

## Footprint

Idle RAM was measured locally: **13.8 MiB RSS idle, 22.3 MiB after 100 creates and 300 list requests**
(Linux, static binary, pure-Go SQLite) before reviews and time off; with them, **14.9 MiB idle and 22.3 MiB after
load** (`specs/022-reviews-timeoff/measurements.md`). The manifest declares 30 MB requested and 64 MB limit.
Details and method: `specs/012-people-directory/measurements.md`. Not measured on k3d.

## Reporting-line approvals seam

The seam between this module and the approval engine (`docs/approvals.md`, can-core) is narrow and
one-directional. Everything below is read from the code, not run on a cluster.

- **Engine side.** `core/server/approvals.DirectoryReportingLine` resolves the `directory` capability
  and calls `GET /v1/people/{id}` with can-core's credential (`CAN_DIRECTORY_TOKEN_FILE`), reading only
  `manager_id`. A step whose approver is "manager of requester" gets `party:<manager_id>`. No provider,
  no credential, an unknown person or an empty `manager_id` mean "no manager" (the step falls back or
  refuses); any other failure is an error, never a silent fallback.
- **This module's side.** It serves that read and exposes the manager link and `GET /v1/people/{id}/reports`
  (direct reports only). The engine uses neither the report list nor a multi-level chain: one hop per step.
- **Not wired yet.** Only `party:<person id>` requesters have a manager. Requesters that arrive as SSO
  users or admin tokens are `user:<name>`, and nothing maps `sso_subject` to a person id until SSO (E11)
  lands, so today they take the fallback. Leave masking does not apply to the lookup (can-core's service
  account is hr and admin, so it sees the true manager), and the lookup does not check whether that manager is
  active: a former manager has no `manager_id` link on their reports (see Fail closed), but one on leave is still named.

## Gaps

- No published image or digest pin; on a cluster it has only been shown to install and become Ready (k3d, CI):
  the module is marked `experimental`.
- can-core does not yet create the keyring Secret, route `introspect`, accept the webhook, or append the
  tombstones to the E08 ledger (this module exposes the feed and the purge hook).
- Reporting-line approval chains (SPEC.md section 3.7) are only half wired, see "Reporting-line approvals
  seam" below: the directory side exists, the identity mapping and multi-level chains do not.
- No UI, no portal, no `identity.provision` consumer, no `work` issue creation from action items (the link
  field exists).
- Reviews and time off (E14b): the can-core approval client is not wired into `cmd/can-people` (the
  `ApprovalEngine` seam and a fake exist, as for equity), so the service decides itself and the `timeoff` and
  `review-release` approval types are declared but unused until then; accruals do not prorate (use
  adjustments); `use` entries are dated at the request's start, not per day; an HR-scheduled status change on a
  person with approved long leave can be dropped as stale; leave-status changes depend on the hourly and
  per-minute tick; calendars and holidays ship empty (the operator enters them); no notifications are sent
  (the E11a engine can notify on the events); anonymity is not proof against someone with database access;
  the finance module does not consume `timeoff.*` yet; no cluster verification.
- Write auditing is after the commit (see Audit); search indexing of sensitive classes is out by design.

## Web UI

The module can serve a self-service web UI (profile, directory, org chart, 1:1s, goals, time off, reviews) next to the API: see [`people-ui.md`](people-ui.md). It is off unless `ui.enabled` is set.
