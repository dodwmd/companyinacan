# Feature specification: Observability, health and diagnostics (Epic E22)

**Branch**: `epic/E22-observability` | **Status**: built offline, verified against the docker compose stack
only (no cluster) | **Refines**: `SPEC.md` §3.2 (footprint), §6.4 (CLI), §6.5 (operations); Constitution II and V.

## Why

Constitution II says a component above 120% of its declared RAM fails, but only CI on k3d could tell: a
running company had no way to see it. `/healthz` was a bare "the process answers", there was no readiness,
the logs were not redacted and carried no request id, and a support request meant asking the operator to
paste things. This epic gives every in-house service the same, very small, observability surface without
adding a dependency or a default cost.

## User stories

1. **US1 Operator sees the footprint.** As an operator I can scrape every service and compare
   `can_ram_rss_bytes` with `can_ram_declared_bytes`, and an alert fires above 120%.
2. **US2 Orchestrator routes on readiness.** As the platform I take a pod out of rotation when its database
   or migrations are broken (`/readyz` 503), but I keep it in rotation, and do not restart it, when only a
   dependency is down (`degraded`).
3. **US3 Operator is told about integrity and delivery problems.** Alerts for audit chain verification
   failing, reconcile failing, webhook dead letters growing, disk over 85%, backup older than 36 h.
4. **US4 Maintainer can help without a screen share.** `can diagnose` and `can-core admin diagnose` produce a
   bundle with secrets and email addresses removed, to be skimmed and then attached to an issue (names and phone numbers in free text can survive).
5. **US5 Security reviewer can trust the telemetry.** Metrics, logs and bundles cannot leak secrets or
   personal data, and the metrics endpoint cannot be reached anonymously from outside.

## Functional requirements

- **FR-01** A shared package `core/obs` (standard library only) provides counters, gauges and fixed-bucket
  histograms and the Prometheus text exposition 0.0.4, with label escaping per the format.
- **FR-02** Metric families hold at most 256 label combinations and 128-byte label values; excess folds into
  one `overflow` series. Updates are lock-free atomics.
- **FR-03** Route labels are the registered mux pattern, never the raw path; the method label is a fixed set.
- **FR-04** Process metrics: build info, start time, uptime, RSS (`/proc/self/statm`), goroutines, GC pause
  total, open fds; `can_ram_declared_bytes` only when `CAN_RAM_DECLARED_MB` or `CAN_RAM_DECLARED_BYTES` is set.
- **FR-05** HTTP middleware records count, duration and in-flight by method, route pattern and status class,
  and serves `/readyz`. Database metrics: pool gauges for every SQLite service; statement timings and busy or
  locked counters for can-core's store.
- **FR-06** Business counters, no labels or only fixed enumerations: leads created (forms), issues created
  (work), webhook deliveries by result, dead letters, pending approvals, audit chain length and the result of
  an hourly full chain verification (can-core).
- **FR-07** The metrics endpoint is a separate listener, off unless `CAN_METRICS_ADDR` is set, serving
  `/metrics` only. Off loopback it requires `CAN_METRICS_TOKEN` (or `_FILE`), compared in constant time, and
  the service refuses to start without one. Valid credentials are never throttled.
- **FR-08** `/readyz` returns the JSON component list without error text: 200 for `ok` and `degraded`, 503
  for `down`. `/healthz` and the `-healthcheck` flag and `healthcheck` command are unchanged. Chart and
  can-core readiness probes use `/readyz`; liveness probes are unchanged.
- **FR-09** A shared logger (`core/obs/log`): JSON (text on request), service and version fields,
  server-validated request ids (`X-Request-Id` kept only if 1 to 64 characters of `A-Za-z0-9._-`), request id
  on records, redaction of secret-named keys and secret-shaped values, no query strings or bodies in access
  logs.
- **FR-10** `can diagnose` and `can-core admin diagnose` write a tar.gz from a file-name allowlist with size
  caps, scrubbing every text for secrets and email addresses; seeded secrets and emails must not appear. The state database is opened
  read-only and secret columns are never selected.
- **FR-11** `can doctor` reports reachability of Docker, the `companyinacan` compose stack and the Kubernetes
  API when those tools exist, without ever failing the command.
- **FR-12** `deploy/monitoring/` holds a Prometheus scrape example, alert rules, a small Grafana dashboard, an
  example NetworkPolicy allowing scrapes from the monitoring namespace only, and the compose `monitoring`
  profile (Prometheus and Grafana, pinned tags, localhost-bound, not part of the default `up`).

## Non-functional requirements

- **NFR-01 Footprint.** can-core idle RSS delta at most 0.5 MiB; binary delta at most about 150 KiB; no new
  module dependency; no sidecar; nothing on by default that opens a port.
- **NFR-02 Hot path.** Counter and histogram updates allocate nothing; middleware overhead stays well under a
  microsecond per request. Benchmarks are in the repository.
- **NFR-03 Test first.** Every behaviour above has a test that was seen to fail first.

## Out of scope

Tracing, log shipping, a bundled alert receiver, a Prometheus or Grafana deployment in the cluster, native
histograms, OpenMetrics, per-tenant metrics, and instrumenting wrapped third-party modules (Uptime Kuma,
DokuWiki, EspoCRM and so on keep their own endpoints).

## Honest gaps

Verified only offline and against compose; Prometheus and Grafana images were not started; the cluster
readiness probes and NetworkPolicy example were not applied to a real cluster; disk and backup alerts rely on
host tooling (node_exporter) that is not part of the project; statement timings exist only for can-core.
