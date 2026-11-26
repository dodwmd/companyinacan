# Feature 047: CSV import (E17c)

Status: built offline, experimental. Refines `SPEC.md` section 3.7 ("Import / migration") and 6.4.

## Why

A company that already exists has an employee list, a customer and supplier list and a backlog. Until now
the only way in was one record at a time.

## Scope

CSV import for three record types, each through the owning module's API and the `can` CLI:

| Records | Module route | CLI |
|---|---|---|
| directory people | `POST /v1/import/people` (can-people) | `can import people FILE` |
| finance parties (customers, vendors) | `POST /v1/import/parties` (can-finance) | `can import parties FILE` |
| work issues of one project | `POST /v1/projects/{key}/issues/import` (can-work) | `can import issues FILE --project KEY` |

Not in this slice: web UI pages (each would need a larger body limit, new CSRF-checked forms and new
golden pages in three UIs), vendor-specific column presets (HiBob, Xero and so on), the `import` module
hook, invoices, time entries, cap tables, sensitive people fields, SSO subjects, deleting records.

## Request

The body is the CSV file itself (UTF-8, comma separated, first line is the header, an optional byte order
mark is ignored). Query parameters, any other is refused:

- `apply=true` writes. Without it the request is a **preview** (dry run): the default is never to write.
- `map=<field>=<header>` (repeatable): read `<field>` from the column called `<header>`. Fields without a
  mapping are matched to a header of the same name, ignoring case, with spaces and hyphens read as `_`.
  Columns that match nothing are listed in the report as ignored.
- `joiners=true` (people only): see "Consequences to know".

Only comma separation is read; a header that looks semicolon, tab or pipe separated is refused with a hint.
Rows with every cell blank (`,,,`) are skipped.

## Requirements

- **FR-001 Limits.** At most 1 MiB, 2000 data rows, 64 columns and 128000 commas. The file is read once,
  with a hard cap, and parsed in one pass. One import per module runs at a time (`409`), which bounds memory.
- **FR-002 All or nothing.** An apply writes every row or none, in one database transaction, together with
  its audit entries and events. Any row error means nothing is written (`422` with the report).
- **FR-003 Preview is the real thing.** A preview runs the same code in a transaction that is rolled back,
  so it reports what an apply would do. It writes no record and no event. Because the report says which ids
  exist and match, every preview, and every apply refused for row errors, leaves one `import_preview` audit
  entry (file hash and row count); if that cannot be written there is no report.
- **FR-004 Report.** Counts of rows to create, update, leave unchanged and failed; per row its line number,
  external id, action and the **names** of changed fields (never values); errors and warnings with line and
  column. At most 200 errors and 200 warnings are listed; the counts are complete.
- **FR-005 Idempotency.** Every row has a required `external_id`: 1 to 64 characters, starting with a
  letter or digit, then letters, digits and `._:/#-`. So it cannot be an email address or a spreadsheet
  formula. It is unique per record type (per project for issues) and unique within the file. Importing the
  same file again changes nothing. A row whose id exists updates that record. Records made by hand have no
  external id and are never matched. Ids are **case-sensitive**: `e1` and `E1` are two records, so a source
  that changes the case (or renumbers) creates new records, including for erased ones.
- **FR-006 Updates never clear.** An empty cell, or an absent column, leaves the stored value alone. Values
  are cleared in the normal screens or API.
- **FR-007 Authorization.** One check per module, in the store or service, used by every path:
  - people: HR or admin (a restricted token also needs `people.write`);
  - finance: `finance.write`, and a token that is not limited to parties, projects or own records;
  - work: `work.admin` (an import sets the initial status of new issues, which bypasses the workflow), and a
    token not limited to projects.
- **FR-008 No new personal data in events or logs.** Imports publish only the events the same create or
  update would publish. Error messages never go to logs. Audit entries carry ids, field names, counts and a
  hash of the file, never cell values.
- **FR-009 Formula injection.** Values are stored as written. No import path writes CSV, and external ids
  cannot start a formula. Every existing CSV export already defuses text cells.

## Fields

`*` is required in every file. "Create only" values are ignored on update with a warning when they differ.

**People**: `external_id`*, `given_name`*, `family_name`*, `kind` (create only; default `employee`),
`status` (create only; `candidate`, `onboarding` or `active`; default `active`), `preferred_name`,
`work_email`, `work_phone`, `location`, `department`, `team`, `position` (by name; must already exist),
`start_date`, `manager_external_id` (a row of this file or an earlier import; the same rules as setting a
manager, checked against the final reporting lines). A work email already used by another record is an
error, so a first import does not duplicate people entered by hand.

**Parties**: `external_id`*, `name`*, `kind` (default `customer`), `contact_name`, `email`, `phone`,
`address`, `tax_id`, `currency`, `notes`. A new row with the same name as an existing party is a warning.
A party with purchase orders cannot be changed to `customer` (the same rule as editing it).

**Issues**: `external_id`*, `title`*, `type` (create only; default `task`), `status` (create only; a status
of the project's workflow), `description`, `priority`, `assignee`, `due_date`, `estimate`, `labels`
(separated by spaces or `;`), `parent_external_id` (a row of this file or an earlier import; parents may
come after children). The importing identity is the reporter.

## Erased and removed records

An erased person or party keeps its external id (an opaque code), and a row naming it is an error, so a
re-import cannot bring erased data back. When the finance retention purge later deletes an erased party,
the SHA-256 of its external id is kept (`erased_external_ids`) and the import still refuses it. People
records are never deleted. A former employee is not updated. An issue removed by retention is
gone, and importing its row again creates a new issue.

## Consequences to know

**People and joiners.** Provisioning starts a joiner run (accounts, checklists) from `person.created.v1`
with status `onboarding` or `active`. An import is mostly of existing staff, so **by default a new `active`
record publishes no `person.created.v1`**; `onboarding` and `candidate` records publish it as usual.
`joiners=true` (`--joiners`) publishes it for `active` records too. The report's `notices` say, in the
preview, how many rows will start a joiner run and how many are imported as existing staff. Later status
changes publish their events either way, so leaving still works. Any other consumer of
`person.created.v1` also does not hear of staff imported without it.

Importing issues publishes `issue.created.v1` (which carries the title) and `issue.updated.v1`, and the importer
watches the issues. Preview first.
