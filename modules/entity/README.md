# entity

In-house company-records service: legal entities, the registered agent and addresses with history, officers and
directors with effective dates, statutory registers (members, directors, charges) as append-only ledgers with
as-of views, a filings calendar with reminder events, document references, links to `equity` share classes.
Library `core/board`, binary `cmd/can-board` (shared with the `board` module), image `deploy/board`. It provides
the `entity` capability and publishes `entity.filing.due.v1` (ids and dates only).

**This is a tool, not legal advice.** Which registers a company must keep, what they must show and what must be
filed by when are not implemented: obligations and due dates are data the operator enters. Compliance with
company law and data-protection law is the operator's responsibility.

**Status: experimental.** Tested offline with the `board` module; not yet run on a cluster, no Helm templates, no
admin UI. One process serves both modules, so enabling both does not double the footprint (measured idle RSS
14.4 MiB for the process).

| | |
|---|---|
| Listener | `:8082` (shared with `board`), ClusterIP only |
| Data | `/data/board.db`, shared with `board`; registration numbers, personal details and register detail sealed |
| Data classes | `entity-register` (legal), `entity-person` (7y), `entity-filings` (legal), `entity-audit` (legal) |

Read `docs/entity.md` for the register rules, as-of views, effective dating, attestation, filings reminders,
erasure versus legal retention, events and the gaps. The contract is `specs/029-board-entity/contracts/board-api.yaml`.
