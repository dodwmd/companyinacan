# Board (boards, meetings, resolutions, minutes)

The in-house `board` module (library `core/board`, binary `cmd/can-board`, shared with the `entity` module,
see `docs/entity.md`). It keeps boards and committees with members and terms, meetings with notice and
quorum as data, agendas, board packs (document references, no bytes), attendance, declared interests and
recusal, resolutions voted by roll call, by written consent or by secret ballot, minutes with versions, and
action items linked to `work` issues by opaque id.

**This is a tool, not legal advice.** It records and computes what the data says. Which notice a meeting
needs, what makes it quorate, how a resolution passes, who must sign a consent, how minutes must be kept and
what directors owe the company depend on the company's constitution and on local law; none of that is
implemented. Each body carries its own rules as data and the operator is responsible for entering what applies.
Compliance with company law and data-protection law is the operator's responsibility.

**Status: experimental** (offline-tested; can-core installs the chart and CI waits for Ready on k3d; no admin UI; see the gaps).

## Why one binary, two modules

`board` and `entity` are separate modules (separate `module.yaml`, roles, data classes and capabilities) served
by one process (`can-board`) with one database, one keyring and one audit trail. Reasons: a resolution
authorises register entries and a filing, so they share ids and the audit chain; one SQLite connection, one
key file and one process keep the footprint at one service (measured 14.4 MiB idle, target 20) instead of two;
and an operator can still enable `board` without `entity`. The cost, documented as a gap: can-core does not
yet deploy two manifests as one release.

## What it holds

| Data | Class | Retention | Sealed fields |
|---|---|---|---|
| Minutes (versions, approval) | `board-minutes` | legal | content |
| Resolutions, versions, votes, consents, secret ballots, tallies | `board-resolutions` | legal | title, text, ballots |
| Board pack lists (document refs) | `board-packs` | 7y | title, note |
| Declared interests | `board-private` | 7y | nature |
| Audit trail | `board-audit` | legal | none (ids and field names only) |

People are opaque directory person ids (`pers_...`): no name, e-mail or address is stored in the board
module. Bodies, meetings, agenda lines and action item assignments are plain structure; the confidential
content is sealed with XChaCha20-Poly1305, bound to its row and column (a ciphertext copied elsewhere does not
open), with the key ring and rotation described below. Resolution versions and minutes also carry a content
hash that is checked on every open, so a swapped or edited ciphertext is detected.

## Roles and access

Roles come from the token; membership comes from the person link (`person_id`), never from the role alone. A
director token without a person id sees nothing. Roles: `director`, `officer`, `secretary` (the company
secretary), `admin`, `observer`, `audit-viewer` (`auditor` is reserved for can-core), `employee`,
`integration` (the can-core service account).

| | director (member of the body) | officer (invited) | secretary | admin | observer | audit-viewer | employee |
|---|---|---|---|---|---|---|---|
| Body, members, meetings, agenda | own bodies | meetings invited to | all | all (no content) | own bodies | all | no |
| Resolution (metadata) | own bodies | no | all | all | no | all | no |
| Resolution title and text | yes, unless recused or the subject | no | all | no | no | once decided | no |
| Per-voter votes | after close, unless recused/subject | no | after close (never a secret ballot) | no | no | after close | no |
| Draft minutes | chair, or attended | no | all | no | no | no | no |
| Approved minutes | own bodies | meetings invited to | all | no | own bodies | all | no |
| Board pack | own bodies | meetings invited to | all | no | no | no | no |
| Action items | own bodies and own | own | all | own | own | own | own |
| Draft, propose, amend a resolution | member (proposer only amends) | no | yes (proposer only amends) | no | no | no | no |
| Open and close the vote | chair | no | yes | no | no | no | no |
| Vote or sign | member, not recused, not the subject | no | never | no | no | no | no |
| Approve minutes | attended, not the drafter | no | no | no | no | no | no |
| Legal hold, erasure | no | no | no | yes | no | no | no |

The service returns **404** for a record the caller may not see (the same bytes as for a missing or malformed
id) and **403** only when the caller can see the record but not do the thing. The policy lives in one pure
function (`core/board/policy.go`); a table test checks every role against every relationship (member, chair,
attended, recused, conflicted, subject, drafter, proposer, invited, observer, self) for every action.

An admin sees the structure so they can run the service, never the content. Lists are filtered, not refused.

### Separation of duties

One function, `sodOK`, decides whether two actions are by different people: it folds subjects (NFKC,
invisible characters removed, case-folded) and also compares the directory person id. It is used for:

- **A proposer is never the sole approver.** A resolution cannot pass on votes whose only "for" is the proposer
  (or the module that requested it). A written consent whose only eligible signer is its proposer fails with
  reason `proposer_sole_approver`. For a secret ballot nobody can tell who voted for, so a pass needs at least
  two "for" ballots (a deliberate, conservative rule).
- **Minutes**: the person who drafted or edited a draft cannot approve it, under either subject spelling or
  person id.
- **Register entries** (`docs/entity.md`): the recorder and the person an entry names cannot attest it.
- **Subjects and the recused cannot vote.** A resolution can name the people it is about (`subjects`): they are
  recused automatically, so a grantee who sits on the board does not approve their own grant. A director can
  declare an interest with treatment `recuse` (cannot vote, excluded from quorum when the body says so),
  `abstain` (may only abstain) or `disclosed` (may vote, on the record). Interests are declared before voting
  opens; during voting the resolution must be withdrawn and proposed again.

## Meetings, notice and quorum

Notice and quorum are data on the body: `notice_days`, a quorum fraction `quorum_num/quorum_den`, an absolute
`quorum_min`, whether the recused count towards quorum, and the pass threshold `pass_num/pass_den` (strict or
inclusive). A meeting needs the body's notice; shorter notice is accepted only with the opaque reference of a
recorded waiver. Quorum is `QuorumRequired` (the smallest integer at least the fraction of eligible members,
at least the minimum, never zero) against members recorded present or remote. All counts are integers: there is
no floating point anywhere in quorum or tally arithmetic (tests include a property test against exact
arithmetic and a fuzz target). Quorum is checked when voting opens, at each vote and again at close.

Attendance is editable until the meeting has approved minutes, then frozen by trigger.

## Resolutions

States: `draft`, `proposed`, `voting`, `passed`, `failed`, `withdrawn`; moves forward only (a trigger refuses
anything else).

- **Amendments create new versions.** Title and text are versioned; old versions stay. Only the proposer
  amends, and only before voting opens. After voting opens the text and thresholds are fixed.
- **Roll call** (`kind: meeting`): the chair or secretary opens the vote, which fixes the list of people who
  may vote (members in term minus subjects and the recused) and checks quorum; present members vote once;
  the chair or secretary closes it and the tally is recorded.
- **Written consent** (`kind: consent`): unanimous consent by circulation with a signature record
  (`signature_ref`, an opaque id from the signing provider). It passes by itself when every eligible member has
  signed, fails at once on a decline, and fails with reason `deadline` when the deadline passes (the sweep
  closes it). A written consent cannot abstain or be a secret ballot. Other consent rules (for example majority
  consent) are not implemented.
- **Secret ballot**: while voting is open each ballot is only **staged**: the sealed choice goes into
  `ballot_staging` (a random id and the sealed choice, no voter, no time) and a hash receipt with the voter
  into `receipt_staging`, which stops a second vote. No audit row is written per vote (a per-vote row would carry
  the voter and an increasing sequence). When voting closes (or the resolution is withdrawn) the service, in one
  transaction, re-seals each staged ballot under a new random id, writes the ballots into `ballots` and the
  receipts into `vote_receipts` in an order produced by a `crypto/rand` Fisher-Yates shuffle, writes one `voted`
  audit marker per voter sorted by person id (at close time), deletes the staging rows, and then runs `VACUUM`
  and `wal_checkpoint(TRUNCATE)` with `secure_delete` on so the staging order does not persist in free pages or
  the write-ahead log. After the close nothing in the database, the audit trail or the API records the order the
  votes were cast in; nobody, including the secretary, can read per-voter detail; the tally is computed from the
  sealed ballots. A vote cannot be changed once cast. The API never exposes ballot ids or order.
  **Residual risks (not protected):** (a) anyone who can read the live database file, its write-ahead log or
  the process memory **during an open vote** sees the staging tables in cast order and can pair a receipt with a
  ballot by position, and with the keyring can open the choices: the protection is against the API, the audit
  trail and a copy of the database taken after the close, not against an operator with live file access while
  voting is open; (b) backups and snapshots taken during the vote keep the staging; (c) the audit trail shows who
  voted (not how) once the vote closes; (d) a very small electorate, or a unanimous result, reveals votes by the
  tally alone; (e) a pass needs at least two "for" ballots because the proposer cannot be told apart.
- **Tallies are immutable.** Votes and ballots can be inserted only while voting is open and are never changed
  or deleted; a decided resolution cannot be updated; the database enforces this with triggers. No running
  tally is shown while voting is open.
- **Withdrawn**: before voting opens, a withdrawal records no tally. A resolution withdrawn **while voting is
  open** needs a reason (sealed, audited as a `withdraw` entry), keeps the votes cast so far (the ballots are
  finalised, the tally is recorded and stays readable to those who may read votes) and is then immutable.
- **Abstentions** are not votes cast: they do not count in the denominator of the pass threshold, and they never
  change the outcome (property-tested). They do count as present for quorum if the person is recorded present.
- **A member whose term ends while voting is open** stays on the voter list (the eligible count was fixed) but
  loses access with the term, so cannot vote; a written consent then cannot complete and fails at its deadline.
- **A recused director or the subject of a resolution** sees that the resolution exists, its status, kind,
  thresholds, tally once decided, and the list of declared interests (person ids and treatments, not the
  nature); they do not see the title or text, and cannot vote.

### The equity approval seam

Equity declares that `grant` approvals need the board. This module is the consumer side of that seam; equity
itself is not modified here. The `resolutions` capability (`schemas/capabilities/resolutions.v1.json`) lets
can-core's approval adapter for equity map the engine's calls onto resolutions:

| Equity `ApprovalEngine` | `resolutions` capability |
|---|---|
| `Request{Type: "grant", Ref: grt_..., Requester}` | `POST /v1/resolution-requests` with `type: equity.grant`, `ref`, `body_id`, `requester` (subject of the person who asked), `subjects` (the grantee's person id) creates a draft resolution; idempotent on (type, ref) |
| `Status(ref)` | `GET /v1/resolution-outcomes?type=equity.grant&ref=...` answers `pending`, `approved`, `rejected` or `cancelled` with `decided_on`; never text or votes |
| the decision | `board.resolution.passed.v1` carries `resolution_id`, `body_id`, `result: passed`, `passed_on`, and the `external_type` and `external_ref` so the requester can correlate |

The secretary completes the generated draft, attaches a meeting and runs it like any resolution. The requester
cannot be the sole approver, and a grantee who is a director is recused. Only the `integration` role (the
can-core service account) may use these two verbs, and it can do nothing else. What is not built: the equity
side of the adapter, and can-core routing between the two services (gaps below).

## Minutes

A meeting's minutes are versions: a draft (secretary only; readable by the secretary and the members who
attended) is approved by a director who attended and did not draft or edit it, after the meeting is marked
held. An approved version is immutable (service and trigger; only the sealed ciphertext can be re-sealed by key
rotation, and its content hash is frozen). A correction is a new version that must be approved again; the
earlier version becomes `superseded` and stays on record. Minutes are never deleted.

## Audit

Every read of a record or a sensitive field is audited **before** the data is returned (fail closed: if the
audit write fails, the read fails with an error and returns nothing). A list is audited per record, in one
batch. Every write records its audit entry in the same transaction as the change: no entry, no change. The
audit table is append-only (triggers) and hash-chained; `can-board verify-audit` recomputes the chain. Refused
accesses are audited too, throttled per actor (a flood of denials cannot fill the log; the first suppressed one
is itself recorded). Entries hold ids and field names, never content. Keep the latest chain hash somewhere a
database administrator cannot write.

## Encryption and key rotation

`keyring_file` (mode 0600, 0400 or 0440, owned by the service user or root, not a symlink) holds lines of
`<key-id> <64 hex>`; the first key seals, the others only open. `can-board rotate-keys` re-seals every value
written with a retired key; each replacement is a compare-and-swap (it only applies if the stored ciphertext is
still the one read), so a concurrent write is never overwritten with stale data; values that changed are
retried. When it reports 0, remove the old key.

## Retention, legal hold and erasure

Minutes, resolutions (with votes and attendance) and the audit trail are statutory or legal records: retention
`legal`, never purged automatically and not configurable. Board pack titles and notes (`board-packs`, 7y) and
the nature of declared interests (`board-private`, 7y) are cleared by the sweep once old, unless the person is
under legal hold.

`POST /v1/people/{person}/erasure` (admin) clears what may be cleared for a directory person: the nature of
declared interests, and (entity module) addresses and dates of birth. It keeps what law requires: membership
terms, votes, attendance, minutes, register entries including the name a register must show. The response lists
what was retained and why. A legal hold (`PUT /v1/people/{person}/legal-hold`) or a current term blocks it.
Tombstones are listed at `GET /v1/erasures`, which can-core collects into the backup erasure ledger, and `can-board purge -subject <person>
-class <class|all>` re-applies them (idempotent) after a restore. Backups keep older data until they expire.

## Events

`board.meeting.scheduled.v1` (`meeting_id`, `body_id`, `starts_on`) and `board.resolution.passed.v1`
(`resolution_id`, `body_id`, `result`, `passed_on`, optionally `meeting_id`, `external_type`, `external_ref`).
No names, titles, text, vote counts, minutes or amounts. Delivered at least once, signed (HMAC), through an
outbox written in the same transaction as the change; consumers de-duplicate on the event `id`. Schemas:
`schemas/events/`.

## Configuration

`/etc/can-board/board.yaml` (strict: unknown keys are errors):

```yaml
listen: ":8082"
db_path: /data/board.db
keyring_file: /etc/can-board/keys/keyring   # required
webhook: { url: "" }                         # can-core event endpoint; needs CAN_BOARD_WEBHOOK_SECRET
introspect: { url: "", allow_insecure_http: false }
retention: { board-packs: 7y, board-private: 7y, entity-person: 7y }
```

Environment: `CAN_BOARD_API_TOKEN` (at least 24 characters; the can-core service account with the `integration`
role only: it requests a resolution and reads the outcome of the requests it created, nothing else),
`CAN_BOARD_ADMIN_TOKEN` (optional, at least 24 characters, different; a separate service account with the `admin`
role only, for erasure, legal hold and tombstones), `CAN_BOARD_WEBHOOK_SECRET`, `CAN_BOARD_INTROSPECT_TOKEN`, optional
`CAN_BOARD_CONFIG`, `CAN_BOARD_KEY_FILE`. The introspection endpoint answers `{active, subject, roles,
person_id}`; unknown roles are dropped. Failed authentications are rate limited per address; a request with a
valid credential is never refused because of FAILED authentications from the same address (the failed-authentication limiter is per address and only failures count). Authenticated requests are limited per subject (burst 120, 20 per second), except the can-core service account, which carries all of can-core's traffic under one subject and is exempt. Request bodies are limited to 64
KiB and strict JSON; every read and write has a deadline; list sizes are capped. Creating a resolution honours
`Idempotency-Key`: the same key and body replays the first result (200), the same key with another body is 409,
and a replay re-reads the record under the caller's current rights.

Commands: `can-board serve | check-config | healthcheck | rotate-keys | retention | verify-audit |
purge -subject <pers id> -class <class|all> | version`. The background loop runs the due sweep (consent
deadlines, filing reminders) and the retention sweep hourly and delivers the outbox.

CSV: `GET /v1/bodies/{id}/resolutions/export.csv` exports the resolution register; every text cell is protected
against spreadsheet formula injection (a leading `=`, `+`, `-`, `@`, tab or newline is prefixed with `'`), the
whole export is audited per record before a byte is written, and titles appear only for callers allowed to read
the text.

## Limits and gaps

- **Not implemented, by design:** quorum, notice and voting law; who may call a meeting; proxies; casting votes;
  weighted votes; reserved matters; director duties; written-resolution rules beyond unanimity; electronic
  meeting rules; minute formalities. They are data fields (fractions, days, thresholds) or the operator's job.
- A secret ballot cannot hide a vote in a tiny electorate, and the audit trail shows who voted (not how).
- A person holding two subjects (a personal and an admin account) is two people to the service unless both carry
  the same `person_id`. Human tokens must carry `person_id` (an identity-provider contract): without it a
  director sees nothing, but an `admin` or `secretary` token without it is checked by subject only.
- Database triggers repeat the rules for bugs in the service; someone with write access to the SQLite file can
  drop them. Keep the file out of reach and a copy of the audit chain head elsewhere.
- Meeting titles, agenda titles and body names are plain (they are structure and appear in notices); put
  nothing confidential in them. No monetary amounts exist in this module (the entity module's register share counts are plaintext integers, see `docs/entity.md`).
- A response-time difference between "exists but not yours" (writes a denial audit row) and "does not exist"
  is possible; it is not equalised.
- Member terms are effective-dated but the service uses the term in force today for access; a director whose
  term ended loses access to old materials (their past votes stay in the record).
- No attachments upload (document references only), no calendar invitations or e-mail, no PDF minutes, no admin
  UI or director portal, no investor portal.
- Verified on a cluster only as far as "installs and becomes Ready" (k3d, CI). can-core generates the keyring and
  tokens, but does not route token introspection, deliver the events, or wire the equity approval adapter to
  `resolutions`. `board` and `entity` are installed as **two instances with separate databases**, so rules that
  need both (a director's access to an entity through board membership) deny.
- One writer: the database uses one connection; there is no horizontal scaling.

## Later review notes

- Entity records are scoped by body: a director reads a legal entity only through a body whose `entity_ref` is that
  entity (see `docs/entity.md`); the secretary, officer and audit-viewer see all entities.
- The secretary reads per-voter detail only after the vote closes, like everyone else.
- A resolution request from another module after a withdrawal creates a fresh resolution; identical concurrent
  requests return the same one. The requester has no access to the board: only the secretary (or the proposer
  director) amends or withdraws a draft, and the outcome verb returns only requests created by the same
  service-account subject.
- The `integration` role is exempt from the per-subject limiter, so each requested reference creates a draft
  resolution; the number of unfinished requested drafts per integration subject is capped at 600 (409 beyond it).
  `integration` and `admin` come only from the static service-account tokens: an SSO introspection answer
  that lists them is ignored.
