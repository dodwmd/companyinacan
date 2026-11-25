# Data model: E17b board and entity

SQLite, one file (`/data/board.db`), migrations `core/board/migrations/001_board.sql` (board tables),
`002_entity.sql` (entity tables) and `003_immutability.sql` (triggers). Times are unix seconds (UTC), dates
`YYYY-MM-DD`, counts integers. Ids are `<prefix>_<24 hex>`: `bdy`, `mbr`, `mtg`, `agi`, `pki`, `cfl`, `res`, `vot`,
`min`, `act`, `lge`, `reg`, `ctc`, `obl`, `fil`, `doc`, `scl`, `evt`. People are opaque directory person ids
(`pers_<24 hex>`). Columns `sens_*` hold sealed ciphertext (`v1.<key-id>.<b64>`), AAD = row id + `:` + column name
(`resolution_versions`: `<resolution id>#<version>`).

> Tool, not legal advice; no jurisdiction rules are modelled.

## Data classes

| Class | Module | Sensitivity | Tables and columns | Retention |
|---|---|---|---|---|
| `board-minutes` | board | **sensitive** | `minutes` (`sens_content`, `content_sha256`, versions) | legal |
| `board-resolutions` | board | **sensitive** | `resolutions`, `resolution_versions` (`sens_title`, `sens_text`), `resolution_subjects`, `resolution_voters`, `votes`, `ballots` (`sens_choice`), `vote_receipts`, `bodies`, `body_members`, `meetings`, `agenda_items`, `attendance`, `meeting_invitees`, `action_items` (`sens_text`) | legal |
| `board-packs` | board | **sensitive** | `pack_items.sens_title`, `sens_note` | 7y |
| `board-private` | board | **sensitive** | `conflicts.sens_nature` | 7y |
| `board-audit` | board | standard | `audit_log` (ids and field names only) | legal |
| `entity-register` | entity | **sensitive** | `register_entries` (`sens_detail`) | legal |
| `entity-person` | entity | **sensitive** | `entity_contacts.sens_name`, `sens_address`; the personal keys inside `register_entries.sens_detail` | 7y |
| `entity-filings` | entity | standard | `filing_obligations`, `filings` | legal |
| `entity-audit` | entity | standard | rows of `audit_log` whose resource starts with `legal-entity`, `register-entry`, `filing`, `contact` | legal |

Entity identifiers (`legal_entities.sens_reg_number`, `sens_tax_id`) are sealed and belong to the entity record.
Platform tables with no personal content: `outbox` (ids, dates, results), `erasures` (tombstones), `person_holds`,
`idempotency` (resource ids only), `schema_migrations`.

## Tables (board)

- `bodies` (id, name unique, kind board|committee, parent, entity_ref, quorum_num/den/min, quorum_excludes_recused,
  pass_num/den/inclusive, notice_days, notice_basis). Never deleted.
- `body_members` (body, person, role chair|member|secretary|observer, start_date, end_date). One open term per
  (body, person) and one open chair per body (partial unique indexes); only the end of a term may be written (trigger).
- `meetings` (body, title, starts_on, mode, status scheduled|held|cancelled, notice_given_on, notice_days_required,
  short_notice_waiver_ref, pack_issued_on). `meeting_invitees`, `agenda_items`, `attendance` (frozen once the meeting
  has approved minutes), `pack_items` (document references; supplementary flag after issue).
- `resolutions` (body, meeting, kind meeting|consent, secret_ballot, status draft→proposed→voting→passed|failed|withdrawn,
  current_version, proposer_ref/person, pass thresholds, consent_deadline, external_type/ref, supersedes, eligible
  and quorum figures, tally, close_reason, closer and date). Forward-only and frozen once decided (triggers); the
  thresholds, version and ballot mode are fixed once voting opens.
- `resolution_versions` (append-only, only before voting; sealed title and text; `content_sha256`).
- `resolution_subjects`, `conflicts` (treatment recuse|abstain|disclosed, sealed nature), `resolution_voters`
  (fixed when voting opens).
- `votes` (open votes and consent signatures; insert only while voting; never updated or deleted), `ballots` and
  `vote_receipts` (secret ballot: no link; written at close in a shuffled order from `ballot_staging` and `receipt_staging`), `resolution_withdrawals` (sealed reason when withdrawn mid-vote).
- `minutes` (version, status draft|approved|superseded, sealed content, `content_sha256`, author, editors, approver);
  approved rows are immutable except for a later supersede; never deleted.
- `action_items` (sealed text, assignee person, opaque `work` issue ref, due, status).

## Tables (entity)

- `legal_entities` (name, jurisdiction, type, status active|dormant|dissolved, incorporated_on, fiscal_year_end,
  sealed registration number and tax id, `equity_entity_ref`). Never deleted.
- `entity_contacts` (registered agent and addresses, effective_from/to, sealed name and address, person ref);
  history is append-only, only the end date may be set once.
- `register_entries` (register members|directors|charges, kind, party_ref, office, title, class_ref, quantity,
  effective_on, sealed detail, resolution_ref, corrects_id, recorder, attestation); append-only; only the
  attestation (once) and the sealed detail may change.
- `filing_obligations`, `filings` (due_on, status pending|filed|waived, receipt ref, reminders sent); a recorded
  filing does not change.
- `entity_documents` (document references, sealed title), `share_class_links` (opaque `equity` class ids).

## Events (outbox)

`board.meeting.scheduled.v1`, `board.resolution.passed.v1`, `entity.filing.due.v1`: opaque ids, dates and the
result only (see `contracts/events.md`).

## Derived views

- Quorum: members in term on the meeting day (minus subjects and the recused when the body excludes them) against
  attendance `present|remote`.
- Register state as of a date: effective entries (those not corrected) with `effective_on <= as_of` and, for
  `known_at`, `recorded_at` on or before that day, replayed in (effective date, sequence) order.
