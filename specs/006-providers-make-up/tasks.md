---

description: "Task list for Epic E06: Server Providers and make up"
---

# Tasks: Server Providers and `make up` (Epic E06)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Test tasks precede their implementation tasks. All verification is
offline. `[V]` marks work that needs a real host and is **not** done (see measurements.md).

## Phase 1: Foundations

- [x] T001 Tests for `Redactor` (literals, patterns, error chain, split writes, secret references) in `core/provider/redact_test.go`
- [x] T002 `redact.go`, `options.go` (secret references)
- [x] T003 Tests for `Store`: round trip, 0600, atomic failure, 25 concurrent mutations, `TryMutate`, newer/corrupt files in `core/provider/state_test.go`
- [x] T004 `state.go` with flock and atomic rename
- [x] T005 `Provider` interface, `Deps`, factory, `local`/`static`/`digitalocean` stubs in `provider.go`; stub tests in `ssh_test.go`

## Phase 2: Bootstrap and SSH provider (US1)

- [x] T006 Tests: bootstrap golden files (default, full), determinism, structural assertions (pinned version, swap flag, firewall order, no 6443, pre-seed overlap), unsafe-value rejection, `bash -n`, shellcheck, cloud-init decode and size in `bootstrap_test.go`
- [x] T007 `bootstrap.go` (script, cloud-init, uninstall)
- [x] T008 Tests: preflight failures and re-run tolerance in `preflight_test.go`; `preflight.go`
- [x] T009 Tests: `SSHArgs` validation, helper-process `SSHRunner` run/copy (stdin transport, sudo only for non-root) in `runner_test.go`; `runner.go`
- [x] T010 Tests: ssh provider create/idempotent/failed preflight/failed bootstrap/destroy/status/plan/address/option validation in `ssh_test.go`; `ssh.go`
- [x] T011 Tests and implementation: kubeconfig rewrite and fetch, tunnel argv, free port (`dns_tunnel_test.go`, `tunnel.go`)

## Phase 3: Hetzner (US2)

- [x] T012 Fixtures `testdata/hetzner/fixtures.json` (recorded-style responses and errors)
- [x] T013 Tests: full flow, idempotent re-run, resume after interruption, key reuse and no foreign delete, destroy and re-destroy, firewall-in-use retry, 429 retry and give-up, POST not retried on 5xx, token redaction (own and echoed), status, plan is offline, timeouts, option validation in `hetzner_test.go`
- [x] T014 `hetzner_api.go` and `hetzner.go`

## Phase 4: Instant URL and DNS (US3)

- [x] T015 Tests: dashing, hosts, https plan, every fallback reason, history windows, host-set order, pruning in `sslip_test.go`; `sslip.go`
- [x] T016 Tests and implementation: record builder, `manual` plugin output, plugin factory in `dns_tunnel_test.go`, `dns.go`

## Phase 5: CLI and Makefile (US4)

- [x] T017 Tests in `core/cli/up_test.go`: `--plan` has no side effects (text, JSON), end-to-end up with fake host, second up idempotent, HTTP fallback output, preflight failure exit 1, exit codes 1/2/3, `down` needs `--yes`, up/status/down lifecycle, degraded status, Hetzner token never in stdout/stderr (text and JSON, API echoing the token), remote output secrets redacted
- [x] T018 `core/cli/up.go`; register in `root.go` (stub for `up` removed, `dev` stub untouched); update the stub tests in `commands_test.go` and `test/contract/make_test.go`
- [x] T019 Makefile `up|down|status` with `ARGS`

## Phase 6: Docs and records

- [x] T020 `docs/providers.md`, `docs/make-up.md` (including the human-gated timing procedure)
- [x] T021 `measurements.md`: what was verified offline, the blocked list
- [x] T022 `make test` green, rebase, PR, CI

## Blocked (human-gated, real server or account) `[V]`

- [ ] V001 Run the bootstrap on a fresh Ubuntu 24.04 and Debian 12 host; confirm the pinned k3s version, node Ready, firewall rules do not break CoreDNS/API under default deny (E00)
- [ ] V002 Hetzner: run `can up` with a real token, confirm server type default, cloud-init completion, `can down` cleanup
- [ ] V003 sslip.io: confirm resolution and Let's Encrypt issuance for the dashed names (E03 deploys Traefik first)
- [ ] V004 The five-minute timing procedure in `docs/make-up.md` (E00)
- [ ] V005 Idle RAM of the trimmed k3s (E00)
