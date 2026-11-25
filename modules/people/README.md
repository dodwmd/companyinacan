# people

In-house people service: the `directory` and `performance` capabilities. Library `core/people`,
binary `cmd/can-people`, image `deploy/people`. A module like any other (`module.yaml`); it provides
`directory` and `performance` and publishes `person.created.v1`, `person.status_changed.v1`,
`person.offboarded.v1`, `oneonone.scheduled.v1`, `goal.updated.v1` and `checklist.task_completed.v1`.

**This is a tool, not legal, HR or payroll advice.** It never computes pay or tax; payroll is
integrated, not built. Compliance with employment and data-protection law is the operator's
responsibility.

**Status: experimental.** Tested offline (unit, race, contract); not yet run on a cluster, no Helm
templates, no admin UI. `can validate` warns accordingly.

Why in-house: people records, salary and ID data, and performance notes are sensitive data classes.
Wrapped third-party apps cannot enforce per-field access or audit reads, so `can validate` rejects a
non-in-house module that declares a sensitive class (Constitution V, SPEC.md section 3.7).

| | |
|---|---|
| Listener | `:8081`, ClusterIP only (the capabilities); no public route |
| Data | `/data/people.db` (SQLite, WAL), one persistent volume; sensitive columns and 1:1 notes encrypted |
| Secrets | `CAN_PEOPLE_API_TOKEN`, `CAN_PEOPLE_WEBHOOK_SECRET`, `CAN_PEOPLE_INTROSPECT_TOKEN`; encryption keyring file from a mounted Secret |
| Footprint | 30 MB requested, 64 MB limit; measured 13.8 MiB idle, 22.3 MiB after load |
| Data classes | `directory-basic` (7y), `people-sensitive` (7y, sensitive), `people-checklists` (2y), `performance-oneonone` (2y, sensitive), `performance-goals` (2y, sensitive) |

Read `docs/people.md` for the access matrix, encryption and key rotation, audit, retention, legal hold,
erasure, events and the gaps. The contract is `specs/012-people-directory/contracts/people-api.yaml`.
