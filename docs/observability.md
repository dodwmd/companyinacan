# Observability: metrics, health, logs and support bundles

Epic E22. Every in-house service (can-core, can-static, can-forms, can-work, can-people, can-jobs,
can-finance, can-equity) shares one small package, `core/obs`, so they all answer the same questions the
same way: is it up, is it ready, how much RAM does it use against what it declared, and what happened. It
is standard library only, so it adds about 100 KB to a binary and nothing measurable to idle RAM (numbers
below).

This is a tool for operators of a small company, not a monitoring product. Nothing here is on by default
that costs RAM or opens a port.

## What each service exposes

| Where | What | Default |
|---|---|---|
| `GET /healthz` on the service port | liveness: the process is up and its database answers (unchanged) | on |
| `GET /readyz` on the service port | readiness, JSON list of components, no error text | on |
| `/metrics` on a **separate** listener | Prometheus text exposition | **off** until `CAN_METRICS_ADDR` is set |
| `-healthcheck` flag / `healthcheck` command | exec probe for distroless images; still probes `/healthz` | unchanged |

### Health model

`/healthz` stays a liveness answer. `/readyz` adds what a service needs in order to do useful work:

| Component | Critical | Meaning |
|---|---|---|
| `database` | yes | the SQLite database answers a ping |
| `migrations` | yes | `schema_migrations` has an applied version |
| `reconcile` (can-core) | no | the last reconcile run did not fail |
| any dependency you add (for example can-core introspection) | no | reachable |

A failing critical component makes `/readyz` answer **503** with `"status":"down"`. A failing non-critical
component answers **200** with `"status":"degraded"`: the service stays in rotation, because restarting it
would not fix the dependency. The body names components and states only, never an error message, address
or credential:

```json
{"status":"degraded","components":[{"name":"database","status":"ok"},{"name":"migrations","status":"ok"},{"name":"reconcile","status":"degraded"}]}
```

Results are cached for 2 seconds and each check has a 2 second deadline, so a probe aimed at the database
cannot become a load. The Helm chart readiness probes and the can-core manifest use `/readyz`; liveness
stays on the binary's own `healthcheck`, so a degraded dependency never restarts a pod.

## Metrics

Enable per service with environment variables (charts and the compose file show examples):

| Variable | Meaning |
|---|---|
| `CAN_METRICS_ADDR` | `host:port` of the dedicated metrics listener, for example `:9190`. Unset = no listener |
| `CAN_METRICS_TOKEN` or `CAN_METRICS_TOKEN_FILE` | bearer token, compared in constant time. Required unless the address is loopback |
| `CAN_RAM_DECLARED_MB` / `CAN_RAM_DECLARED_BYTES` | the RAM the component declared, for the footprint self-check. Charts set it from `resources.requests.memory`; the can-core manifest from the Downward API |

The listener serves `/metrics` and nothing else, GET and HEAD only. If `CAN_METRICS_ADDR` is not
loopback-only and there is no token, the service **refuses to start** rather than publish metrics
unauthenticated. The port is never on the public listener, the ingress route or the module API. A wrong
token gets a bare 401; valid credentials are never throttled (there is no counting of failures to abuse).

**NetworkPolicy.** The base policies are default-deny, so the metrics port is closed until you allow your
Prometheus: `deploy/monitoring/networkpolicy-metrics.example.yaml` allows only pods labelled
`app.kubernetes.io/name: prometheus` in a namespace labelled `can.dev/namespace-kind: monitoring`, to TCP
9190 only. It is an example, not applied by default.

### Metric catalogue

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `can_build_info` | gauge | `version`, `commit`, `go` | always 1 |
| `can_process_start_time_seconds`, `can_uptime_seconds` | gauge | | |
| `can_ram_rss_bytes` | gauge | | resident set, `/proc/self/statm` |
| `can_ram_declared_bytes` | gauge | | only present when a declaration is set |
| `can_goroutines`, `can_open_fds` | gauge | | |
| `can_gc_pause_seconds_total` | counter | | |
| `can_http_requests_total` | counter | `method`, `route`, `class` | `route` is the registered pattern (`/v1/leads/{id}`), `class` is `2xx`..`5xx` |
| `can_http_request_duration_seconds` | histogram | `method`, `route` | |
| `can_http_requests_in_flight` | gauge | | |
| `can_db_connections_open`, `can_db_connections_in_use` | gauge | | from `sql.DB.Stats` |
| `can_db_wait_total`, `can_db_wait_seconds_total` | counter | | waits for a pooled connection |
| `can_db_query_duration_seconds` | histogram | `op` (`query`, `exec`, `tx`, `other`) | can-core statements |
| `can_db_errors_total`, `can_db_busy_total` | counter | `op` / none | busy = SQLite busy or locked |
| `can_reconcile_runs_total` | counter | `result` | can-core |
| `can_reconcile_last_success_timestamp_seconds`, `can_reconcile_consecutive_failures` | gauge | | can-core |
| `can_webhook_deliveries_total` | counter | `result` (`delivered`, `retry`, `dead`) | can-core |
| `can_webhook_dead_letters`, `can_webhook_pending` | gauge | | can-core, refreshed every 30 s |
| `can_approvals_pending` | gauge | | can-core |
| `can_audit_entries` | gauge | | length of the audit chain |
| `can_audit_verify_ok`, `can_audit_verify_last_run_timestamp_seconds` | gauge | | full chain verification, hourly |
| `can_forms_leads_created_total`, `can_work_issues_created_total` | counter | | no labels |
| `can_backup_last_success_timestamp_seconds` | gauge | | **not exported by a service**: written by `deploy/monitoring/backup-textfile.sh` for the node_exporter textfile collector |

### What can never be a label

- **No personal data.** There is no user id, email, subject, lead, issue key, project key, IP address or
  token anywhere in a metric. Business counters have no labels at all.
- **No raw paths or query strings.** The route label is the pattern the mux matched; an unmatched request
  is `unmatched`. An id in a URL can never become a series.
- **Bounded cardinality.** Each metric family holds at most 256 label combinations and each label value is
  cut at 128 bytes. Further combinations fold into one series whose values are all `overflow`, so memory
  stays bounded however hostile the input. The method label is a fixed set (anything else is `OTHER`).
- Label values are escaped on output (`\`, `"`, newline) and made valid UTF-8, per the exposition format.

## Alerts and dashboard as code

`deploy/monitoring/`:

| File | What |
|---|---|
| `prometheus.yml` | scrape config for the compose stack, with a commented Kubernetes example |
| `alerts.yml` | service down, 5xx ratio, **RAM over 120% of declared (Constitution II)**, reconcile failing or stale, **audit chain verification failing**, webhook dead letters growing, SQLite busy, disk over 85%, backup older than 36 h |
| `grafana/dashboards/can-overview.json` | one overview dashboard (10 KB) |
| `networkpolicy-metrics.example.yaml` | allow metrics scrapes from the monitoring namespace only |
| `backup-textfile.sh` | writes the backup timestamp metric from `can backup status` |

Disk and backup alerts need `node_exporter` on the host (not part of this project). `promtool check
rules deploy/monitoring/alerts.yml` validates the rules.

### Try it locally (compose)

```sh
docker compose up -d                              # every service has its metrics listener on the private network
docker compose --profile monitoring up -d --build # adds Prometheus (127.0.0.1:9090) and Grafana (127.0.0.1:3000)
```

Prometheus and Grafana are optional, use pinned image tags, and bind to `127.0.0.1` only. The Grafana login
is the public dev credential `admin` / `dev-only-grafana-password`; the stack is dev only (see
`docs/local-compose.md`). To look at one service by hand, `docker compose exec` is not available in
distroless images; scrape from the Prometheus container or add a published port locally.

## Logs

All services use `core/obs/log`:

- JSON records on stderr (`CAN_LOG_FORMAT=text` for local reading, `CAN_LOG_LEVEL` for the level), with
  `service` and `version`.
- **Request ids.** A request carries `X-Request-Id`. A client value is kept only if it is 1 to 64
  characters of `A-Z a-z 0-9 . _ -`; anything else is replaced by a fresh server-generated id. The id is on
  the response and on every log record written with the request's context.
- **Redaction.** Attributes whose key looks like a secret (password, secret, token, authorization, cookie,
  api key, private key, credential, bearer, totp, csrf, signature, session id) are replaced by
  `[redacted]`; string values and errors are scrubbed for bearer tokens, `user:password@` URLs and
  `password=...` pairs. Tests seed secrets into every path.
- **Access logs** carry method, matched route pattern, status and duration: no query string, no raw path, no
  body, no client address.
- **Injection.** JSON encoding escapes newlines and quotes in every value; hand-built lines use `%q`.

## Support bundle: `can diagnose` and `can-core admin diagnose`

```sh
can diagnose --company-dir . --log-file ./can-core.log --metrics-url http://127.0.0.1:9190/metrics   # token from $CAN_METRICS_TOKEN
kubectl -n can-system exec deploy/can-core -- can-core admin diagnose --out /tmp/diag.tar.gz && kubectl cp ...
```

The bundle is a tar.gz of mode-0600 files from a fixed allowlist: `manifest.json`, `versions.json`,
`config.yaml` (company.yaml, secrets and email addresses removed), `doctor.json`, `modules.json`,
`state.json` (database sizes, schema version, counts of pending and dead deliveries and pending approvals,
audit chain length and verification result), `reconcile.json`, `metrics.txt` (only `can_`, `go_`,
`process_` series), `logs.txt` (last lines), `backup.json`. Nothing else can be added; each file is capped at
256 KiB and the bundle at 2 MiB; an existing output file is never overwritten.

Everything passes a scrubber (bearer tokens, URL credentials, `key=value` secrets, PEM blocks, JWTs,
well-known key prefixes, long random-looking tokens, email addresses) plus the provider redactor seeded with
the values of every secret-looking environment variable. The database is opened read-only; `hmac_secret`,
password hashes, TOTP secrets and session tokens are never selected, and no people, finance or lead rows are
read. Scrubbing is a safety net, not a guarantee: skim the bundle before you attach it to a public issue.

`can doctor` additionally reports whether the Docker daemon, the `companyinacan` compose stack and the
Kubernetes API answer (`ok`, `unreachable`, `not-running`). These never change its exit code.

## Footprint cost (measured, not asserted)

Measured on this repository's CI-class workstation (Go 1.27, `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`),
base = `origin/main` before the epic:

| Binary | Before | After | Delta |
|---|---|---|---|
| can-core | 19,411,104 B | 19,517,600 B | +104 KiB |
| can-forms | 12,247,200 B | 12,349,600 B | +100 KiB |
| can-static | 6,750,368 B | 6,885,536 B | +132 KiB |

Idle RSS of can-core (`serve`, fake token review, empty database, 8 s after start, five runs each, median):
46,904 kB before, 47,056 kB after, **+0.15 MiB** (the ranges overlap: 46.6 to 47.1 MB before, 46.9 to 47.4 MB
after), inside the 0.5 MiB budget but close to the noise floor. can-core's declared request is 48 MiB; its
idle RSS on this machine is about 46 MiB, so the headroom is thin whatever this epic does.

Why not `prometheus/client_golang`: it is not in `go.mod`, and no installs were allowed, so it was **not
measured here**. Its default collectors and protobuf/expfmt dependencies are known to cost on the order of a
megabyte of binary and some resident memory per process; for a project whose promise is "small", a small
stdlib package covering the five metric kinds actually used is the cheaper option, at the price of owning
the text format (golden tests cover escaping, buckets and ordering). Revisit if native histograms or the
OpenMetrics format are ever needed.

Hot-path cost (`go test -bench . ./core/obs/...`): counter increment about 44 ns, 0 allocations; histogram
observation about 43 ns, 0 allocations; the HTTP middleware adds about 470 ns and 9 allocations
(480 B) per request over a bare mux handler, mostly the request clone for the request id and the deferred recorder. A request that
touches SQLite costs hundreds of times that.

## Limits, honestly

- Verified against the compose stack only; nothing here has run on a cluster, and Prometheus and Grafana
  were not started in CI.
- `can_backup_last_success_timestamp_seconds` and the disk alert depend on host tooling outside this repo.
- can-core's `can_db_query_duration_seconds` covers `ExecContext`, `QueryContext` and transactions on its own
  `store.DB`; statements issued through a `*sql.Tx` inside a transaction are covered by the `tx` timing, not
  individually. Modules export connection-pool gauges, not per-statement timings.
- Metrics are per process and per scrape; there is no tracing and no log shipping.
- The scrubbers can miss a secret in an unusual format and can over-redact long identifiers; see above.
