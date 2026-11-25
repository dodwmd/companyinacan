# Data model: E13a people

SQLite, one file (`/data/people.db`), migration `core/people/migrations/001_people.sql`. Times are unix
seconds (UTC), dates are `YYYY-MM-DD`. Ids are `<prefix>_<24 hex>` (96 random bits): `pers`, `dep`, `team`,
`pos`, `tpl`, `chk`, `task`, `ser`, `mtg`, `agd`, `act`, `goal`, `kr`, `upd`, `evt`.

## Data classes

| Class | Sensitivity | Tables and columns | Default retention |
|---|---|---|---|
| `directory-basic` | standard | `people` (non-`sens_*` columns), `departments`, `teams`, `positions`, `status_history` | 7y after leaving |
| `people-sensitive` | **sensitive** | `people.sens_dob`, `sens_national_id`, `sens_address`, `sens_phone`, `sens_salary` (sealed) | 7y after leaving |
| `people-checklists` | standard | `checklist_templates`, `checklists`, `checklist_tasks` | 2y |
| `performance-oneonone` | **sensitive** | `oneonone_series`, `oneonone_meetings` (notes sealed), `agenda_items`, `action_items` | 2y (by age and after leaving) |
| `performance-goals` | **sensitive** | `goals`, `key_results`, `goal_updates` | 2y (closed goals by age; owner after leaving) |

Platform tables with no personal content: `outbox` (ids only), `erasures` (tombstones), `audit_log`
(ids and field names), `schema_migrations`.

## Entities

- **Person** (`people`): `kind` employee|contractor; `status` candidate|onboarding|active|leave|offboarding|
  former; `sso_subject` (unique, links a login to the record); names, work email/phone, location;
  `department_id`, `team_id`, `position_id`, `manager_id` (self-reference); `start_date`, `end_date`;
  sealed sensitive columns; `legal_hold`, `legal_hold_reason`; `ended_at` (retention clock);
  `erased_at` (tombstone marker).
- **Status history** (`status_history`): from, to, `effective_date`, `applied` (0 = pending future change,
  at most one per person via a partial unique index), recorded by (actor id), note.
- **Organisation**: `departments`, `teams` (optional department and lead), `positions` (title, optional department).
- **Checklist template**: kind onboarding|offboarding, `auto_status`, JSON items
  `{title, assignee_role self|manager|hr|admin, due_offset_days, action}`. **Checklist**: person, template,
  anchor date. **Task**: title, assignee role and resolved assignee id, due date, `action`, done.
- **1:1 series**: manager, report, cadence weekly|biweekly|monthly, unique per pair. **Meeting**:
  scheduled time, status, `shared_notes_sealed`, `private_notes_sealed`. **Agenda item**, **Action item**
  (assignee, due date, status open|done, `work_issue_id`).
- **Goal**: scope person|team|company, owner or team, title, description, period, visibility
  private|manager|team|company, status draft|active|achieved|abandoned, `parent_id` (alignment).
  **Key result**: start, target, current. **Goal update**: author, value, comment.

## Invariants

- Manager chain is acyclic and at most 64 deep; a manager is active or on leave.
- Former is terminal; at most one pending status change per person; effective dates never move backwards.
- A series is unique per (manager, report); access follows the *current* reporting line.
- Sealed values are `v1.<key-id>.<b64url(nonce||ct)>` with AAD `<person id>:<field>` or
  `<meeting id>:<shared_notes|private_notes>`.
- A tombstone row is kept for an erased person (`erased_at` set, names `[erased]`); status history dates are
  retained, notes cleared; five `erasures` rows per erased subject.

## State: employment status

```text
candidate -> onboarding -> active <-> leave
    \            \           \         \
     \            \           +--> offboarding -> former (terminal)
      +------------+-----------------^     (offboarding -> active: rescinded)
```

`candidate -> former` and `onboarding -> former` cover withdrawn and rejected hires.
