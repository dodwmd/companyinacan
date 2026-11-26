# equity

In-house equity service: cap table and employee stock options (not inventory). Library `core/equity`,
binary `cmd/can-equity`, image `deploy/equity`. A module like any other (`module.yaml`); it provides the
`equity` capability and publishes `equity.grant.approved.v1`, `equity.grant.vested.v1` and
`equity.exercise.recorded.v1` (ids and dates only).

**This is a tool, not legal, tax or securities advice.** It records and computes what the data says.
409A and other valuation rules, ISO/NSO limits, tax treatment, 83(b) elections and ESOP scheme rules are
not implemented; they are data fields and documented limits. Compliance with securities, tax and
data-protection law is the operator's responsibility.

**Status: experimental.** Tested offline (unit, race, fuzz, contract); can-core installs the chart and CI waits
for Ready on k3d. A web UI for holders and for administration and finance
([`specs/066-equity-ui`](../../specs/066-equity-ui/spec.md), [`docs/equity.md`](../../docs/equity.md#web-ui)) is off unless
`ui.enabled` is set or `modules.equity.expose.ui` routes it. It runs in the Docker Compose stack and its end-to-end suite (grant
approval with can-core's own roles and, separately, with a fake introspection service,
[`docs/local-compose.md`](../../docs/local-compose.md)). `can validate` warns accordingly.

**Grant approval** needs the `equity:board` role, assigned by an owner, and directory identity mode; one person
alone cannot approve a grant ([who is the board](../../docs/equity.md#who-is-the-board)).

Why in-house: equity is a sensitive data class (shareholder identities, holdings, valuations, strike
prices). Wrapped third-party apps cannot enforce per-field access or audit reads, so `can validate`
rejects a non-in-house module that declares a sensitive class (Constitution V, SPEC.md section 3.7).

| | |
|---|---|
| Listener | `:8081`, ClusterIP (the capability); the web UI's paths are routed only with `expose.ui` |
| Data | `/data/equity.db` (SQLite, WAL), one persistent volume; sensitive columns sealed |
| Secrets | `CAN_EQUITY_API_TOKEN`, `CAN_EQUITY_WEBHOOK_SECRET`, `CAN_EQUITY_INTROSPECT_TOKEN`; encryption keyring file from a mounted Secret |
| Footprint | 16 MB requested, 64 MB limit; measured idle RSS: see `specs/018-equity/measurements.md` |
| Data classes | `equity-ledger` (legal), `equity-stakeholder` (7y, sensitive), `equity-grants` (7y, sensitive), `equity-valuation` (7y, sensitive), `equity-audit` (legal) |

**Holder identity:** grants, exercises and ledger entries need a holder confirmed by a second actor
(`docs/equity.md#holder-identity`).

Read `docs/equity.md` for the access matrix, vesting rules, encryption and key rotation, audit,
retention, legal hold, erasure, the round-model formula, events and the gaps. The contract is
`specs/018-equity/contracts/equity-api.yaml`.
