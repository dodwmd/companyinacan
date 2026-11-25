# People and performance (`can-people`)

The in-house `people` module holds the company's **directory** (people records, org structure,
employment lifecycle, onboarding and offboarding checklists) and **performance** data (1:1s and
goals). It provides the `directory` and `performance` capabilities and is the first module that stores
the most sensitive data classes in the platform.

> **This is a tool, not legal, HR or payroll advice.** It records and protects data; it does not
> compute pay or tax, and it does not know your jurisdiction's employment, privacy or retention
> law. The retention defaults below are starting points. The operator is responsible for compliance
> and should take advice from a qualified professional.

Status: **experimental**. Unit-, contract- and race-tested offline; not yet exercised on a cluster
(see [Gaps](#gaps)). Epic E13a, spec in `specs/012-people-directory/`.

## What it does

| Area | Features |
|---|---|
| Directory | People (employee, contractor) with a lifecycle `candidate -> onboarding -> active <-> leave -> offboarding -> former`, effective dating (future-dated changes apply on their date), status history, departments, teams, positions, a manager chain with cycle prevention |
| Fields | `directory-basic` (names, work contact, org, dates) and `people-sensitive` (date of birth, national id, home address, personal phone, salary). Sensitive fields are encrypted per field at rest and every read is audited per field |
| 1:1s | A series between a manager and one direct report, meetings, agenda items either side can add, shared notes, **manager-private notes**, action items that can link to a `work` issue, follow-ups from earlier meetings, history |
| Goals | Personal, team and company goals with key results, progress updates, alignment to a manager's, team's or company goal, and four visibility levels (private, manager, team, company) |
| Checklists | Onboarding and offboarding templates (assignee role, due date relative to an anchor, optional machine `action`), instantiated manually or automatically when a person enters a status; completion emits an event for later identity provisioning |
| Retention | Per data class retention, legal hold, erasure requests that tombstone and purge, an erasure ledger feed |
| API | REST JSON, OpenAPI 3.1 at `specs/012-people-directory/contracts/people-api.yaml`, bearer tokens, one policy layer |

Not included in this slice: time off, payroll (never built, see SPEC.md), reviews/360 cycles,
feedback and praise, the admin and portal UI, search and reporting.

## Roles and who can see what

Roles come from the caller's token (SSO groups): `employee`, `manager`, `hr`, `admin`. A role is
necessary but never sufficient: the policy layer (`core/people/policy.go`) also checks the caller's
relationship to the record (self, direct manager, anywhere in the manager chain, same team) and the
caller's own employment status. Anything not allowed is denied.

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
(carries the task's `action`, for example `identity.provision`, so a provisioning integration can react).

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
`sso_subject`; a token whose subject has no person record gets no staff access.

## Footprint

Idle RAM was measured locally: **13.8 MiB RSS idle, 22.3 MiB after 100 creates and 300 list requests**
(Linux, static binary, pure-Go SQLite). The manifest declares 30 MB requested and 64 MB limit.
Details and method: `specs/012-people-directory/measurements.md`. Not measured on k3d.

## Gaps

- No Helm templates yet (values only, as for forms), no image build or digest pin, no k3d or integration
  tests: the module is marked `experimental`.
- can-core does not yet create the keyring Secret, route `introspect`, accept the webhook, or append the
  tombstones to the E08 ledger (this module exposes the feed and the purge hook).
- Reporting-line approval chains (SPEC.md section 3.7) are not wired: `GET /v1/people/{id}/reports` and the
  manager link exist, the approval engine does not consume them yet.
- No UI, no portal, no time off, no reviews/360, no feedback, no `identity.provision` consumer, no
  `work` issue creation from action items (the link field exists).
- Write auditing is after the commit (see Audit); search indexing of sensitive classes is out by design.
