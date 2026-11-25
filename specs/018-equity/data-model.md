# Data model: E17a equity

SQLite, one file (`/data/equity.db`), migrations `core/equity/migrations/001_equity.sql` (tables) and
`002_immutability.sql` (triggers). Times are unix seconds (UTC), dates `YYYY-MM-DD`, quantities integers of
1/10000 share, prices integer minor units. Ids are `<prefix>_<24 hex>`: `ent`, `cls`, `stk`, `pln`, `grt`,
`exr`, `led`, `val`, `cvt`, `evt`. Columns `sens_*` hold sealed ciphertext (`v1.<key-id>.<b64>`).

> Tool, not legal, tax or securities advice; no jurisdiction rules are modelled.

## Data classes

| Class | Sensitivity | Tables and columns | Default retention |
|---|---|---|---|
| `equity-ledger` | **sensitive** | `ledger`, `grants` (terms), `exercises`, `grant_milestones`, `grant_accel_events`, `convertibles`, `entities`, `share_classes`, `plans` | legal |
| `equity-stakeholder` | **sensitive** | `stakeholders.sens_name`, `sens_email`, `sens_tax_id`, `person_id` | 7y |
| `equity-grants` | **sensitive** | `grants.sens_notes`, `exercises.sens_payment_ref`, draft grants | 7y |
| `equity-valuation` | **sensitive** | `valuations.sens_fmv`, `sens_notes` | 7y |
| `equity-audit` | standard | `audit_log` (ids and field names only) | legal |

Platform tables with no personal content: `outbox` (ids and dates only), `erasures` (tombstones),
`idempotency` (key hash, resource id), `schema_migrations`.

## Entities

- **Entity**: name, free-text jurisdiction, currency.
- **Share class**: kind common|preferred, par value, `authorised` (can only be raised).
- **Plan**: pool of a class: `authorised`, `adopted_on`, status. Plans of a class cannot reserve more than
  the class authorises.
- **Stakeholder**: kind founder|employee|investor|advisor; `person_id` (opaque directory id, unique per
  entity); sealed identity; `legal_hold`; `erased_at`. Never deleted.
- **Grant**: type ISO|NSO|RSU|SAR (RSU has strike 0); status draft, pending_approval, approved, active,
  cancelled, forfeited, expired; `version` and `supersedes_id`; quantity, currency, `strike_minor`,
  `grant_date`, `vest_start`, `schedule` (JSON), `expiry_date`, `ptep_days`; `created_by`, `edited_by`,
  `submitted_by`, `approved_by` as canonical subjects (separation of duties); approval, activation,
  termination and terminal dates; `exercised`; `vested_notified`; sealed notes.
- **Milestones and acceleration events**: append-only facts that feed the calculator.
- **Exercise**: requested, approved, payment_recorded, issued (or rejected, cancelled); quantity, `due_minor`
  (computed once), paid amount and date, sealed payment reference, ledger id.
- **Ledger**: `seq`, kind issuance|exercise|transfer_out|transfer_in|cancellation|repurchase, effective date,
  signed quantity, optional price, `group_id` (both legs of a transfer), references to grant and exercise.
- **Convertible**: kind safe_pre_money|safe_post_money|convertible_note, issue date, sealed terms (principal,
  cap, discount, interest). Data only: never converted by the service.
- **Valuation**: kind 409a|independent|board|other, effective and expiry dates, sealed price and notes.

## Invariants enforced by triggers

Audit, ledger, milestones, acceleration events: no UPDATE or DELETE. Valuations: only sealed columns change.
Stakeholders and share classes: never deleted; classes can only be raised. Grants: terms frozen outside
draft; legal status transitions only; approval requires an approver different from creator, editors and
submitter; `exercised` never decreases or exceeds the quantity; only drafts are deleted. Exercises: request
frozen; legal transitions only; approver and payer differ from the requester.
