# Research: Observability (E22)

## R1. Own exposition vs prometheus/client_golang

**Decision**: a stdlib implementation in `core/obs`.

- `client_golang` is not in `go.mod`; no installs were allowed in this epic, so its cost was **not
  measured here**. What was measured is the cost of the replacement: +104 KiB (can-core), +100 KiB
  (can-forms), +132 KiB (can-static) of binary; +0.15 MiB idle RSS for can-core (medians of five runs,
  ranges overlapping). The client library is widely reported to add on the order of a megabyte of binary and
  non-trivial resident memory through its default Go and process collectors and its protobuf and expfmt
  dependencies; that is a claim from outside this repository and is labelled as such everywhere.
- Needed surface: counter, gauge, fixed-bucket histogram, three function-backed gauges, text format 0.0.4.
  No summaries, no exemplars, no native histograms, no push gateway.
- Cost of owning it: the text format. Mitigated by golden tests for HELP and label escaping, bucket
  cumulation, `+Inf`, ordering, overflow series and invalid UTF-8.
- Revisit trigger: native histograms, OpenMetrics, or more than a handful of new metric kinds.

## R2. Binary size is dominated by imports, not code

The first cut (regexp for name validation, `encoding/json` for `/readyz`, slog in the listener, `database/sql`
in the same package) grew can-static by 987 KB (+15%). Removing those from the shared path (hand-written name
validation, hand-built JSON, a two-method `Logger` interface that `*slog.Logger` satisfies, `obssql` and
`rid` sub-packages) brought it to +132 KiB. can-core and the modules already link all of them, so for them
the delta was +100 KiB all along.

## R3. Idle RSS measurement method

`bin/{base,new}-can-core serve` built with `CGO_ENABLED=0 -trimpath -ldflags "-s -w"` from `origin/main` and
from the branch; `CAN_TOKEN_REVIEW=fake`, `CAN_INSTALLER=none`, empty database on tmpfs-backed scratch,
`VmRSS` read from `/proc/<pid>/status` 8 s after start, five runs each:

| | runs (kB) | median |
|---|---|---|
| before | 46964 46644 47124 46804 46904 | 46904 |
| after | 46940 47056 46856 47404 47320 | 47056 |

Delta +152 kB = +0.15 MiB. This is RSS on a workstation, not the cgroup working set measured by the footprint
harness (`docs/measurements/phase0-ci.md`), so it is evidence about the delta, not about the 48 MiB gate.
Metrics enabled adds one listener goroutine and one `http.Server`; its RSS was not separately measured.

## R4. Where to put metrics: separate listener vs token on the module port

A separate listener (`CAN_METRICS_ADDR`) keeps `/metrics` off every route Traefik or the module API serves,
so a routing mistake cannot publish it. A token is still required off loopback, and the process refuses to
start without one, so a typo in the bind address is also safe. Chosen over "token on the module port" because
the module port is reachable by every module namespace.

## R5. Route label cardinality

Go 1.22+ sets `Request.Pattern` on the request the mux served. The middleware reads it after the handler
returns, so it must wrap the mux directly or an outer layer that does not clone the request; can-core's
`httpx.Middleware` clones, so the instrumentation sits inside it. Patterns are registered by code (bounded);
unmatched requests are one `unmatched` series; UI catch-all patterns such as `/` are one series each.

## R6. Readiness semantics

Kubernetes removes a not-ready pod from Service endpoints and does not restart it. A failing dependency
(can-core introspection, notification webhook, the reconcile loop) cannot be fixed by removing this pod, and
removing it would turn a partial outage into a total one, so those are `degraded` with HTTP 200. Critical
components (database, migrations) are 503. Liveness probes stay on the binary's own `healthcheck` so nothing
here can cause a restart loop.

## R7. Why readiness is cached for 2 seconds

A readiness probe every 5 to 10 seconds from the kubelet plus any number of external callers would each run a
database ping and a `SELECT` on a one-connection SQLite pool. A short cache bounds that to one check per two
seconds per process whatever calls `/readyz`.

## R8. Why can-core gauges are polled, not computed at scrape time

can-core's pool is one connection (SQLite). A scrape that ran `COUNT(*)` queries would queue behind real work
and a slow scrape could block admin requests. A background loop refreshes counts every 30 s; the audit chain
is fully verified at most once an hour.

## R9. Scrubbing strategy for bundles

Allowlist of names, caps, scrub every text, never read personal-data tables, never select secret columns:
defence in depth, because each layer can fail alone. Long token-like runs (32 or more of `A-Za-z0-9_+=-`) are
redacted, which also hides sha256 hex digests and long identifiers; accepted as the cheaper error. `config.yaml`
additionally blanks the value of any key whose name contains password, secret, token, api key, private key,
credential or bearer.

## R10. Downward API for declared RAM

The module installer forbids `fieldRef` and `resourceFieldRef` env in module charts (allowlist,
`core/server/installer/pod.go`), so module charts render `CAN_RAM_DECLARED_MB` from `resources.requests.memory`
(`{{ trimSuffix "Mi" }}`), and a chart test pins it to the manifest's `ram_mb`. can-core's own manifest is not
installed through that path and uses the Downward API, which stays correct if the request is edited.
