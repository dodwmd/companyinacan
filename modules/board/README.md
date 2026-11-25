# board

In-house board and governance service: boards and committees, meetings, agendas, board packs, quorum and
recusal, resolutions (roll call, written consent, secret ballot), minutes with versions, action items. Library
`core/board`, binary `cmd/can-board` (shared with the `entity` module), image `deploy/board`. A module like any
other (`module.yaml`); it provides the `board` and `resolutions` capabilities and publishes
`board.meeting.scheduled.v1` and `board.resolution.passed.v1` (ids, dates and the result only).

**This is a tool, not legal advice.** It records and computes what the data says. Quorum, notice, voting,
signing and minute-keeping law are not implemented: each body carries its own quorum fraction, notice days and
pass threshold as data, and the operator is responsible for entering what their constitution and local law
require. Compliance with company law and data-protection law is the operator's responsibility.

**Status: experimental.** Tested offline (unit, race, fuzz, contract); not yet run on a cluster, no Helm
templates, no admin UI. `can validate` warns accordingly.

Why in-house: board materials are confidential (Constitution V, SPEC.md section 3.7). Wrapped third-party apps
cannot enforce per-body visibility, recusal or audited reads, so `can validate` rejects a non-in-house module
that declares a sensitive class.

| | |
|---|---|
| Listener | `:8082`, ClusterIP only (the capabilities); no public route |
| Data | `/data/board.db` (SQLite, WAL), one persistent volume; sensitive columns sealed |
| Secrets | `CAN_BOARD_API_TOKEN`, `CAN_BOARD_WEBHOOK_SECRET`, `CAN_BOARD_INTROSPECT_TOKEN`; encryption keyring file from a mounted Secret |
| Footprint | 20 MB requested, 64 MB limit; measured idle RSS 14.4 MiB: see `specs/029-board-entity/measurements.md` |
| Data classes | `board-minutes` (legal), `board-resolutions` (legal), `board-packs` (7y), `board-private` (7y), `board-audit` (legal) |

Read `docs/board.md` for the access matrix, the voting and quorum rules, the equity approval seam, encryption,
audit, retention, legal hold, erasure, events and the gaps. The contract is
`specs/029-board-entity/contracts/board-api.yaml`.
