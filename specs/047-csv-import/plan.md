# Plan: 047 CSV import

- `core/csvimport`: standard library only. Bounded read, header mapping, row cells, `external_id` rules,
  query options and the report. No database knowledge.
- Each module: `import.go` (store or service, including the authorization check), one route, one migration
  adding a nullable `external_id` column with a partial unique index (people 005, finance 005, work 003;
  checked against main and the open pull requests, none adds a migration).
- Existing create and update code is reused through transaction-level functions (people: insert extracted
  from `createPerson`; work: `createInTx` and `updateTx`), so rules, events and activity cannot drift.
- Preview is the apply transaction, rolled back.
- `core/cli/import.go`: `can import people|parties|issues`, configured by `CAN_<MODULE>_URL` and
  `CAN_<MODULE>_TOKEN`; no redirects are followed.

## Footprint

No new process, dependency or idle memory. An import holds at most 1 MiB of file plus its parsed rows and
report, one at a time per module. The write transaction holds the module's single writer connection for the
length of the import; the size limits keep that short.

Measured once (linux/amd64, `go test` binary, `GOMEMLIMIT=48MiB`, not the container): a 1.0 MB, 2000-row
issue file with events on, previewed, applied and re-run, raised the process's peak RSS from 25.6 MiB to
44.1 MiB, under work's 64 MiB limit; each run took well under a second. People and finance were not
measured separately, and no import has been measured in a running pod. Tests use 500 to 1000 rows because
the race detector makes the pure-Go SQLite about forty times slower.

## Constitution check

Spec first; tests with the code; no new contracts between modules (no new event types); authorization in
one place per module; no personal data in logs. No complexity to justify.
