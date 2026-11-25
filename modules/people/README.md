# people

In-house people service: the `directory` and `performance` capabilities. Library `core/people`,
binary `cmd/can-people`, image `deploy/people`. A module like any other (`module.yaml`); it provides
`directory` and `performance` and publishes `person.created.v1`, `person.status_changed.v1`,
`person.offboarded.v1`, `oneonone.scheduled.v1`, `goal.updated.v1` and `checklist.task_completed.v1`.

**This is a tool, not legal, HR or payroll advice.** It never computes pay or tax; payroll is
integrated, not built. Compliance with employment and data-protection law is the operator's
responsibility.

**Status: experimental.** Tested offline (unit, race, contract); the chart installs through can-core's
installer in unit tests; not yet run on a cluster, no admin UI. `can validate` warns accordingly.

**Installed under can-core** (see [`docs/module-wiring.md`](../../docs/module-wiring.md)): can-core generates `CAN_PEOPLE_API_TOKEN` and the
sealed-field keyring once and rotates them on request (`can-core admin secrets rotate people keyring`
adds a key and keeps the old ones; run `can-people rotate-keys` afterwards to re-seal). Personal access
tokens are introspected through can-core and become roles from their permissions (`people.read` is
employee, `performance.write` without `people.sensitive-write` adds manager, `people.write` is hr,
`people.admin` is admin); a token's subject is `user:<admin username>`, so a person is linked to it by
setting their `sso_subject` to that value. Events go to can-core. **`candidate.hired.v1` from jobs
starts onboarding:** a person in status `onboarding` (named "New hire" and the candidate id; the hand-off
carries no personal data, HR fills in the real details) and the onboarding checklist, once per
candidate. If no template starts on entering onboarding, a default one is created.

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
