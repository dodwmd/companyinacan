# Entity (company records and legal calendar)

The in-house `entity` module: legal entities, the registered agent and addresses with history, officers and
directors with effective dates, statutory registers (members, directors and officers, charges) as append-only
ledgers with as-of views, a filings calendar with reminder events, document references, and links to share
classes in `equity` by opaque id. It is served by the same binary as the `board` module (`cmd/can-board`,
library `core/board`): see `docs/board.md` for why, and for encryption, audit, configuration and commands, which
are shared.

**This is a tool, not legal advice.** Which registers a company must keep, what each must show, who may inspect
it, which filings are due and when, and what a late filing costs differ by jurisdiction and entity type; none of
that is implemented. An obligation is a name, an authority label, a first due date, a recurrence and reminder
points that the operator enters. Compliance with company law and data-protection law is the operator's
responsibility.

**Status: experimental** (offline-tested; can-core installs the chart and CI waits for Ready on k3d; no admin
UI). can-core runs `entity` as its own instance of the binary, with its own database and keyring: rules that
need board membership (a director or officer of a body tied to the entity) find no bodies there and deny.

## What it holds

| Data | Class | Retention | Sealed |
|---|---|---|---|
| Statutory register entries | `entity-register` | legal | name, address, date of birth, charge terms |
| Agent and address history | `entity-person` | 7y | name, address |
| Entity identifiers | (entity record) | with the entity | registration number, tax id |
| Obligations and filings | `entity-filings` | legal | none (ids, dates, receipt reference) |
| Document refs, share class links | with the entity | | document title |
| Audit trail | `entity-audit` | legal | none |

Structural facts (ids, dates, offices, share counts, opaque party references) are plain; personal and
descriptive detail is sealed per entry with the same keyring and AAD binding as the board module. People are
opaque references: a directory person id or any opaque party id; share classes are opaque `equity` class ids.
Nothing is copied from `equity` or `people`.

## Roles and access

| | secretary | admin | officer | director (member of a body tied to the entity) | audit-viewer | observer, employee |
|---|---|---|---|---|---|---|
| Entity record, documents, links | read, write | read, create (no identifiers) | read | read | read | no (404) |
| Registration and tax numbers | read, write | no | read | read | read | no |
| Registers and as-of views | read, write | no (404) | read | read | read | no |
| Names and register detail | all | no | read | read | read | no |
| Addresses and dates of birth | all | no | no (the person themself: their own) | no (their own) | no | no |
| Attest an entry | no | no | yes, not their own, not the recorder | same | no | no |
| Filings calendar | read, write | no | read, mark filed | read | read | no |

The policy is the same pure layer as the board module (`core/board/policy.go`) and has the same matrix test.
Invisible records are 404, not 403. The admin runs the service and sees no register content.

## Registers

Three registers, each an append-only ledger (a trigger refuses update and delete; only the attestation and the
sealed detail may change, the latter by key rotation or by erasure of non-statutory fields):

- **directors** (directors and officers; also secretary and auditor offices): `appointed` and `ceased`, with
  office, title, effective date and the opaque id of the board resolution that authorised it.
- **members**: `allotted`, `transferred` (a signed quantity, positive to the receiving holder, negative from the
  transferring holder) and `ceased`, with the opaque `equity` share class id. Whole shares as integers. The
  holding never goes negative on any date, and a membership ceases only at zero.
- **charges**: `created` and `satisfied` by charge reference.

Each new entry is checked by replaying the whole register with it: no one holds the same office twice, nothing is
ceased that is not held, balances never go negative **on any date** (a backdated entry is checked against every
later date too). An entry is refused with 409 if it would break a rule. `effective_on` cannot be in the future.

A mistake is corrected by a later entry whose `corrects_id` names the wrong one: views ignore the corrected
entry, the ledger keeps both, and an entry can be corrected once.

**As-of views.** `GET /v1/legal-entities/{id}/registers/{register}?as_of=DATE` computes who held which office,
or who held how many shares of which class, on that date from the ledger. `known_at=DATE` restricts the view to
what had been recorded by then ("what did the register show on that day"), which differs from `as_of` when
entries were backdated. `history=true` adds the ledger lines. `GET .../officers` is the directors register as of a
date. `GET .../registers/{register}/export.csv` exports the state with formula-injection protection and audits
every opened entry before writing a byte.

**Attestation.** The secretary records entries; a director or officer attests them
(`POST /v1/register-entries/{id}/attest`). Nobody attests an entry that names them (the party reference equals
their person id) or that they recorded (one separation-of-duties function, subjects folded, person id compared).
The attestation is write-once (trigger). The module does not stop an unattested entry from appearing in views:
the views mark it (`attested: false`).

## Entities, agent and addresses

An entity has a name, jurisdiction and type labels, status (`active`, `dormant`, `dissolved`; dissolved is
final), incorporation date, fiscal year end (`MM-DD`), the sealed registration number and tax id, and an opaque
`equity_entity_ref`. Registered agent and addresses (`registered_agent`, `registered_office`, `principal_office`,
`postal`) are effective-dated: a new record of the same kind starts on its date and ends the previous one the day
before; the history stays and `?as_of=` returns what applied on a date. A contact tied to a person (`person_ref`)
shows its name and address only to the secretary and that person; others see that it exists, with the details
withheld.

## Filings calendar

An obligation (`POST /v1/legal-entities/{id}/obligations`) has a name, an authority label, a first due date, a
recurrence (`once`, `monthly`, `quarterly`, `annual`) and reminder points in days before the due date (default
30, 7 and 0). It creates the first due date; marking a due date `filed` (with the date and the opaque id of the
receipt) or `waived` creates the next one for a recurring obligation, computed from the first due date so a 31st
returns to the 31st after a short month (31 January, 28 February, 31 March). A recorded filing is immutable.
Overdue is computed (`pending` and past due), not a status.

The due sweep (hourly, and at start) publishes `entity.filing.due.v1` (`filing_id`, `obligation_id`,
`entity_id`, `due_on`, `days_before`) for the reminder point that has been reached and not yet announced; earlier
points are marked sent with it, so a sweep that was down over a due date announces once, not three times; a
second sweep announces nothing. can-core turns the event into a notification and de-duplicates by id. No names,
amounts or document contents are in the event.

## Retention, legal hold and erasure

Registers and filings are legal records. Erasure of a person (`POST /v1/people/{person}/erasure`, admin; see
`docs/board.md`) removes addresses and dates of birth from register detail and the name and address from their
agent record, and keeps the name the register must show, the entry, the dates and the quantities; the response
says what was retained and why. The retention sweep clears agent and address details of records that ended more
than the configured period ago (`entity-person`, default 7y), except for people under legal hold.

## Not implemented, limits and gaps

- **By design:** statutory register content and form, inspection rights, filing lists and deadlines, penalties,
  beneficial-ownership registers, annual-return generation, trademark and domain calendars (SPEC.md mentions
  them for the `entity` module; they can be modelled as obligations but there is no dedicated record), share
  certificates and transfers law. The members register does not read from or write to `equity`: both are kept
  by their owners and linked by opaque ids; nothing reconciles them yet.
- Quantities and registration numbers are only as private as the sealing: share counts, offices and dates are
  plaintext in the database (the replay and the triggers need them). An encrypted volume and encrypted backups
  are required.
- Register entries cannot be edited, removed or have their dates changed: that is the point; corrections are new
  entries.
- Attestation is not required for an entry to take effect in a view.
- No admin UI, no document storage (references only), no import of existing registers, no PDF output.
- The `entity` manifest shares a release with `board`; can-core cannot deploy two manifests as one release yet.
- Not verified on a cluster.

## Scope by body

A director reads an entity and its registers, contacts, documents and filings only through a body whose
`entity_ref` is that entity's id (set when the body is created); a body with no `entity_ref` gives no access to any
entity record. The secretary, officer and audit-viewer see every entity, the admin sees entity structure only.
Lists are filtered per entity. Share counts and offices in the registers are plaintext integers in the database
(the replay and the triggers need them); names, addresses, dates of birth and charge terms are sealed.
