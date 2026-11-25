# Data model: Observability (E22)

There is no persistent state. The entities are in-process.

## Registry

- `Registry`: map of name to family. `Default` is the process registry served by the metrics listener.
- `family`: name, help, type (`counter`, `gauge`, `histogram`), label names (at most 4), bucket bounds,
  optional function (function-backed gauges and counters), series map keyed by an array of 4 strings (no
  allocation on lookup), one `overflow` series.
- `series`: label values; `val` (float64 bits, atomic) for counters and gauges; `sum` (float64 bits) and
  per-bucket `uint64` counts for histograms.

Limits: 256 series per family, 128 bytes per label value, 4 labels per family. Registering a name twice
returns the same family; conflicting type or label count panics at start-up (programmer error).

## Health

`Health`: ordered checks `(name, critical, fn(ctx) error)`, a 2 s TTL cache and a 2 s per-check deadline.
`Report{status, components[{name, status}]}`; status values `ok`, `degraded`, `down`. Overall `down` if any
critical check fails; `degraded` if only non-critical checks fail.

## Request id

String of 1 to 64 characters `[A-Za-z0-9._-]`, otherwise 16 lower-case hex digits generated per request.
Carried on the context (`rid.With`), the `X-Request-Id` response header and the `request_id` log attribute.

## Support bundle

Allowed names: `manifest.json` (written by the builder), `versions.json`, `config.yaml`, `doctor.json`,
`modules.json`, `state.json`, `reconcile.json`, `metrics.txt`, `logs.txt`, `backup.json`. Caps: 256 KiB per
file (truncated with a marker), 2 MiB total (an addition that would exceed it is refused). Mode 0600.

`state.json` keys: `db_bytes`, `db_wal_bytes`, `schema_version`, `deliveries_pending`, `deliveries_dead`,
`approvals_pending`, `events_stored`, `modules`, `audit_entries`, `audit_chain_ok`, `audit_violation{kind,seq}`.
`modules.json`: `[{id, version, state, last_error (sanitised)}]`. `reconcile.json`: `{at, ok, actions, failed,
error (sanitised)}`.

## can-core gauges (refreshed every 30 s; verification at most hourly)

`can_approvals_pending` = `COUNT(approval_requests WHERE status='pending')`; `can_webhook_dead_letters`,
`can_webhook_pending` = counts of `deliveries` by status; `can_audit_entries` = head sequence;
`can_audit_verify_ok` = result of the last full `Verify`, left unchanged if the verification itself errored.
