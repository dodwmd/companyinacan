---
description: "Task list for Epic E22: Observability, health and diagnostics"
---

# Tasks: Observability (Epic E22)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Each test task was written and seen failing before the code that satisfies it.
`[V]` marks work that needs a cluster or the Prometheus and Grafana images and is **not** done.

## Phase 1: `core/obs` (US1, US5)

- [x] T001 Tests (`registry_test.go`: golden exposition, escaping, cardinality cap, race, duplicates, benchmarks) then `registry.go`
- [x] T002 Tests (`process_test.go`) then `process.go`: RSS from statm, declared RAM from env, build info, uptime, goroutines, GC, fds
- [x] T003 Tests (`obs/log/log_test.go`) then `log.go`, `rid`: request id policy, redaction of keys and values, injection, level
- [x] T004 Tests (`http_test.go`) then `http.go`, `health.go`, `server.go`: route patterns not paths, method set, readyz, health model, token handler, listener policy, valid credentials never throttled
- [x] T005 Tests (`sql_test.go`, `obssql_test.go`) then `sql.go`, `obssql`: readiness and pool gauges, busy classification
- [x] T006 Binary-size pass: remove regexp, encoding/json, slog and database/sql from the shared path (can-static +987 KB to +132 KiB)

## Phase 2: Wire the services (US1, US2)

- [x] T007 can-core: `httpx.Middleware` request-id policy and instrumentation, store statement hook, webhook delivery counter, platform gauges, reconcile metrics and readiness, `Config.Env`, version and commit, logger
- [x] T008 can-static, can-forms, can-work, can-people, can-jobs, can-finance, can-equity: `obs.Setup`, `obssql.WatchDefault`, `obs.Instrument`, shared logger, route instead of raw path in access logs (`test/wiring/obs_wiring_test.go` first)
- [x] T009 Business counters: leads created, issues created (tests first)
- [x] T010 Charts: readiness `/readyz` for the six module charts and the can-core manifest, `CAN_RAM_DECLARED_MB` in chart env, `CAN_RAM_DECLARED_BYTES` from the Downward API for can-core; installer test updated
- [ ] T011 [V] Probes on a real cluster; `/readyz` through Traefik

## Phase 3: Diagnostics (US4)

- [x] T012 Tests then `core/diagnose`: allowlist, caps, scrub, config key redaction, tail
- [x] T013 Tests then `can diagnose` (seeded secrets, token from env only, no overwrite, URL credentials refused)
- [x] T014 Tests then `can-core admin diagnose` (read-only, secret columns never selected, no creation)
- [x] T015 Tests then `can doctor` reachability (never fails, never echoes command output)

## Phase 4: Monitoring as code (US1, US3)

- [x] T016 `deploy/monitoring`: scrape example, alert rules, dashboard, NetworkPolicy example, backup textfile script
- [x] T017 Compose: metrics env on every service (private network only), `monitoring` profile with Prometheus and Grafana, pinned tags, localhost-bound, not in the default `up`
- [ ] T018 [V] `promtool check rules`, Prometheus loading the config, Grafana loading the dashboard (images not pulled in this run)

## Phase 4b: QA follow-ups

- [x] T022 Backup alerts: `backup-textfile.sh` keeps the last good timestamp and writes `can_backup_last_run_ok`; `CanBackupMissing` (`absent`, 1h) and `CanBackupLastRunFailed`; alert rules parsed and checked by `test/contract/monitoring_test.go` (promtool is not available offline); script behaviour tested with a stub `can`
- [x] T023 `can_audit_verify_ok` and its timestamp exist only after a completed verification; retry after 5 minutes on an error; alert gated on the timestamp
- [x] T024 Audit `Verify` is keyset-paged (5000 rows) and releases the single SQLite connection between pages; test: a concurrent query waits under 100 ms on a 30,000-record log; damage across page boundaries still found
- [x] T025 Diagnose: `RedactConfig` removes nested values under secret, URL, email and contact keys and all block-scalar text; `--compose-project` validated; claims reworded to secrets and email addresses
- [x] T026 `/readyz` checks run detached from the first caller's context and are shared by concurrent probes (no lock held while checking)
- [x] T027 Log redaction scans structured values (maps, structs, http.Header, byte slices, Stringers, pointers) and messages
- [x] T028 Warn when `CAN_METRICS_TOKEN_FILE` is group or world readable
- [x] T029 `CanNotReady` renamed `CanHighServerErrorRatio` with a minimum request rate
- [x] T030 Finance merge: route pattern in the access log; `logPath` kept for its tests

## Phase 5: Docs and measurement

- [x] T019 `docs/observability.md`, SPEC cross-references, `docs/local-compose.md`, epics table
- [x] T020 Footprint: binary and idle RSS deltas, hot-path benchmarks, recorded in `research.md` R3 and the docs
- [ ] T021 [V] Footprint delta on k3d with the CI harness
