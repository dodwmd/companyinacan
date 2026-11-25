# Data model (migration 004_reviews_timeoff.sql)

Timestamps are unix seconds (UTC), dates are `YYYY-MM-DD`, `*_sealed` columns hold XChaCha20-Poly1305
ciphertext (`v1.<key-id>.<b64>`). Applies on top of 001/002; nothing existing is altered.

## Reviews (class `performance-reviews`, sensitive, default 2y)

| Table | Purpose |
|---|---|
| `review_templates(id rtp_, name, questions JSON, created_by, created_at)` | Generic questions (`id`, `text`, `type` rating or text, `audience` all, self, manager, peer, upward). No personal data. |
| `review_cycles(id cyc_, name, state, template_id, questions snapshot, anonymity, min_responses >= 3, rating_max 3..10, due_date, ...)` | `state`: draft, open, calibration, closed. Questions are copied at creation. |
| `review_participants(id rev_, cycle_id, person_id, ...)` | The review. Sealed: `self_sealed` (AAD `review:<id>:self`), `mgr_sealed` (`:mgr`; answers, proposed rating, summary), `calib_sealed` (`:calib`), `final_sealed` (`:final`; frozen outcome), `ack_sealed` (`:ack`), `release_note_sealed` (`:relnote`). `release_state` none, requested, released. `touched_by` JSON of SHA-256 hashes of canonical identities (separation of duties history). Unique `(cycle_id, person_id)`. Cascade from the cycle. |
| `review_requests(id frq_, participant_id, reviewer_id, kind peer or upward, status proposed, approved, declined, submitted, proposed_by, decided_by, created_at)` | Nominations and feedback requests. `proposed_by` is never shown to the nominee. Unique `(participant_id, reviewer_id, kind)`. |
| `review_responses(id frs_, participant_id, kind, reviewer_id NULL when anonymous, answers_sealed)` | Feedback. No timestamp. AAD `review_response:<id>`. |

## Time off (class `people-timeoff`, sensitive, default 7y)

| Table | Purpose |
|---|---|
| `leave_types(id lvt_, key, name, paid, sensitive, accrual_mode none or annual or monthly, grant_halves, carryover_cap_halves (-1 unlimited), allow_negative, negative_floor_halves, ignores_blackouts, sets_leave_status, leave_status_min_halves, active)` | Policy as data. `accrual_mode none` means untracked (no balance). Units are half days. |
| `holiday_calendars(id cal_, location (case-folded, unique), name, work_days bitmask)` and `holidays(calendar_id, date, name)` | Public holidays and work week per location. |
| `blackouts(id blk_, name, start_date, end_date, team_id NULL, ...)` | Company-wide or per team. |
| `leave_requests(id lvr_, person_id, type_id, status, start_date, end_date, start_part full or pm, end_part full or am, start_slot, end_slot, halves, note_sealed, approval_ref, submitted_by, decided_by, cancelled_by, status_applied, ...)` | `note_sealed` AAD `leave:<id>:note`. Slots are `day*2 + 0|1`. `status_applied`: 1 this request moved the person to leave, 2 and back. |
| `leave_ledger(seq, person_id, type_id, entry_date, kind grant, accrual, expire, use, reversal, adjust, halves signed, ref, note_sealed, created_by, created_at)` | Append-only: a trigger refuses UPDATE of everything except `created_by`, and DELETE unless `ledger_guard.open = 1`. Unique `(person_id, type_id, kind, ref)`: reruns of accruals and double use or reversal write nothing. `note_sealed` (adjustment reason) AAD `leave_ledger:<ref>:note`. |
| `ledger_guard(id = 1, open)` | Opened only inside an erasure or purge transaction. |

## State machines

Cycle: `draft -> open -> calibration -> closed` (forward only; opening needs a participant; closing needs no
release waiting for a decision).

Review release: `none -> requested -> released`; a rejection returns to `none` (history kept).

Nomination: `proposed -> approved | declined`; `approved -> submitted` when answered. Manager and HR additions
start `approved`.

Leave request: `draft -> pending -> approved | rejected`; `draft | pending | approved -> cancelled`. A rejected
request is final.
