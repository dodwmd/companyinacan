# Implementation plan: Observability, health and diagnostics (E22)

Spec: [spec.md](spec.md). Research: [research.md](research.md). Data model: [data-model.md](data-model.md).
Contracts: [contracts/](contracts/). Tasks: [tasks.md](tasks.md).

## Shape

| Package | Role |
|---|---|
| `core/obs` | registry, counter/gauge/histogram, exposition, process metrics, HTTP middleware, `Health`, metrics handler and listener (`StartMetrics`, `Setup`), `DBObserver`. Standard library only, no `database/sql`, `regexp`, `encoding/json` or `log/slog` imports, so it costs about 100 KB even in can-static. |
| `core/obs/rid` | request id (generate, validate, context). Split out so the id logic is not tied to slog. |
| `core/obs/log` | `New` logger with redaction, request id in records, `Sanitize`, `RedactString`. |
| `core/obs/obssql` | `WatchSQL`: readiness checks and pool gauges for a `*sql.DB` (keeps `database/sql` out of can-static). |
| `core/diagnose` | bundle builder, `Scrub`, `RedactConfig`, `TailLines`. |
| `core/cli/diagnose.go`, `core/doctor` (`Reach`) | `can diagnose`, `can doctor` reachability. |
| `core/server/corecli/diagnose.go` | `can-core admin diagnose` (read-only DB). |
| `core/server/runtime/obsmetrics.go` | can-core business gauges, reconcile metrics and readiness. |
| `core/server/store/observe.go` | statement timing hook on `store.DB`. |

Per-service wiring is deliberately three lines: `obs.Setup(...)` after the signal context in `Main`,
`obssql.WatchDefault(db)` after migrations, and `obs.Instrument(handler)` on the outermost handler. A
wiring test (`test/wiring/obs_wiring_test.go`) fails the build if a service forgets one.

## Constitution check (before research, again after design)

| Principle | Before | After design |
|---|---|---|
| I Spec-first | SPEC §6.4/§6.5 gain `can diagnose`, `/readyz` and the observability bullet in this PR; no contradiction | same; contracts under `contracts/` |
| II Footprint is a feature | The risk: can-core is at ~46 of 48 MiB. Budget set: <= 0.5 MiB RSS delta, no new dependency, off by default, no sidecar | Measured: +0.15 MiB RSS (noise-level), +104 KiB binary; recorded in `docs/observability.md`. The epic adds the means to *alert* on the 120% rule. Prometheus and Grafana are an optional compose profile, not default |
| III Test-first | Tests precede code | Done: each package's tests were written and run red first (compile failure or assertion) before its implementation |
| IV Stable module contracts | Modules must not see each other's internals | Metrics are per process; no capability or event schema changes; `/readyz` is an addition, `/healthz` unchanged |
| V Secure and private | Metrics and logs are the classic leak path | Dedicated listener, token off loopback and fail-closed, no PII labels, bounded cardinality, redacting logger, scrubbed read-only bundle, NetworkPolicy example; every claim has a seeded-secret or hostile-input test |
| VI Open-source hygiene | Apache-2.0, no new third-party code | No dependency added; Grafana and Prometheus are pulled by tag only in an optional dev profile (Apache-2.0 and AGPL-3.0 respectively, never linked or redistributed; noted in the docs) |
| VII Simplicity and measured claims | Do not over-build | A 5-kind metrics subset instead of a client library, justified by measurement and by what was not measured (`research.md`); every figure in the docs is from a run recorded there |

## Complexity tracking

| Addition | Why it is not avoidable |
|---|---|
| Own exposition code instead of `client_golang` | Constitution II and the no-new-dependency rule; about 330 lines plus golden tests |
| Two small split packages (`rid`, `obssql`) | Keeps the import graph of can-static free of `database/sql` and `slog` (measured: +132 KiB instead of about +1 MiB for the first, unsplit cut) |
| Global `obs.Default` and `obs.DefaultHealth` | One process is one service; it removes plumbing from six module constructors. Tests that need isolation use `NewRegistry` and `NewHealth` |

## Risks

- can-core idle RSS is near its request; a later feature could consume the headroom. The new RAM alert
  makes that visible.
- Readiness on the public forms listener exposes component names (not errors) to the internet: judged
  acceptable, the same listener already serves `/healthz`.
- The scrubber is regex based and can miss unusual formats; documented, with a review-before-sharing note.

## Testing

`go test -race ./core/obs/... ./core/diagnose ./core/cli ./core/doctor ./core/server/... ./test/wiring/...`
plus benchmarks (`go test -bench . -benchmem ./core/obs/...`). Seeded-secret tests: `core/diagnose`,
`core/cli`, `core/server/corecli`, `core/obs/log`. Not tested: Prometheus and Grafana loading the shipped
files, cluster probes.
