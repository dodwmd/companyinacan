# Events published by the equity module

Envelope (same as every module): `{id, type, time, source: "equity", data}`; `id` is `evt_` plus 24 hex characters, stable across
redeliveries; delivery is at least once, signed with `X-Can-Signature: sha256=HMAC(secret, timestamp + "." + body)`; consumers
de-duplicate on `id`. Schemas: `schemas/events/<type>.json` (copies in `modules/equity/events/`); fixtures: `testdata/events/<type>/`.
Payloads carry opaque ids and dates only, never names, quantities, prices, valuations or notes (they are sensitive fields: a consumer
reads them from the `equity` capability with its own rights). `additionalProperties` is false, so an amount cannot slip in.

| Event | `data` | When |
|---|---|---|
| `equity.grant.approved.v1` | `grant_id`, `stakeholder_id`, `entity_id`, `plan_id`, `approved_on` | the board step approved a grant |
| `equity.grant.vested.v1` | `grant_id`, `stakeholder_id`, `entity_id`, `vested_through` | the periodic sweep saw more vested than it last announced for an active grant |
| `equity.exercise.recorded.v1` | `exercise_id`, `grant_id`, `stakeholder_id`, `entity_id`, `recorded_on` | an exercise was issued into the ledger |

`vested_through` is the sweep date, not an instalment date; several instalments between two sweeps give one event.

SPEC.md 4.3 lists `equity.proposed.v1`, `equity.approved.v1` and `equity.granted.v1`; they stay in the catalogue. This slice
publishes the three events above instead (the activation step that would produce `granted` is recorded but not yet announced).

The capability contract is `schemas/capabilities/equity.v1.json`; the full REST surface is `equity-api.yaml`.
This is a tool, not legal, tax or securities advice.
