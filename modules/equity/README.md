# equity

In-house equity service: cap table and employee stock options (not inventory). Library `core/equity`,
binary `cmd/can-equity`, image `deploy/equity`. A module like any other (`module.yaml`); it provides the
`equity` capability and publishes `equity.grant.approved.v1`, `equity.grant.vested.v1` and
`equity.exercise.recorded.v1` (ids and dates only).

**This is a tool, not legal, tax or securities advice.** It records and computes what the data says.
409A and other valuation rules, ISO/NSO limits, tax treatment, 83(b) elections and ESOP scheme rules are
not implemented; they are data fields and documented limits. Compliance with securities, tax and
data-protection law is the operator's responsibility.

**Status: experimental.** Tested offline (unit, race, fuzz, contract); not yet run on a cluster, no Helm
templates, no admin UI or portal. `can validate` warns accordingly.

Why in-house: equity is a sensitive data class (shareholder identities, holdings, valuations, strike
prices). Wrapped third-party apps cannot enforce per-field access or audit reads, so `can validate`
rejects a non-in-house module that declares a sensitive class (Constitution V, SPEC.md section 3.7).

| | |
|---|---|
| Listener | `:8081`, ClusterIP only (the capability); no public route |
| Data | `/data/equity.db` (SQLite, WAL), one persistent volume; sensitive columns sealed |
| Secrets | `CAN_EQUITY_API_TOKEN`, `CAN_EQUITY_WEBHOOK_SECRET`, `CAN_EQUITY_INTROSPECT_TOKEN`; encryption keyring file from a mounted Secret |
| Footprint | 16 MB requested, 64 MB limit; measured idle RSS: see `specs/018-equity/measurements.md` |
| Data classes | `equity-ledger` (legal), `equity-stakeholder` (7y, sensitive), `equity-grants` (7y, sensitive), `equity-valuation` (7y, sensitive), `equity-audit` (legal) |

Read `docs/equity.md` for the access matrix, vesting rules, encryption and key rotation, audit,
retention, legal hold, erasure, the round-model formula, events and the gaps. The contract is
`specs/018-equity/contracts/equity-api.yaml`.
