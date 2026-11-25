---

description: "Task list for Epic E12c: DigitalOcean provider and Cloudflare DNS"
---

# Tasks: DigitalOcean and Cloudflare (Epic E12c)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: tests precede implementation. `[V]` marks work that needs a real account and is **not** done.

## Phase 1: DigitalOcean provider (US1, US2)

- [x] T001 Fixtures `core/provider/testdata/digitalocean/fixtures.json` (hand-written, unverified shapes)
- [x] T002 Tests in `digitalocean_test.go`: full flow, operator_ip firewall, idempotent re-run, crash-resume by tag, foreign droplet/firewall refused, key reuse, destroy order and wait-until-gone, 404 tolerance, tag re-check, empty-state discovery, pagination, 429 / 5xx retry rules, token echo redaction, option validation, action errored/timeout/cancel, plan/status/address, tag characters, `TargetFor`
- [x] T003 `digitalocean_api.go`, `digitalocean.go`; `APIError.Service`, `Deps.DOBase`, state fields, `provider.New`
- [x] T004 `can validate` rules for DigitalOcean options (`check.go`, fixtures `company-do-*`)

## Phase 2: DNS plugin interfaces and Cloudflare (US3)

- [x] T005 Tests in `dns_cloudflare_test.go`: create and remember, idempotent and in-place update, zone walk-up, zone missing, foreign never touched, adopt rules, CNAME conflict, crash resume, remove only own, remove tolerates missing, proxied/TTL, pagination, rate limits, redaction, token must be a reference, plan offline, preflight, factory, config parsing
- [x] T006 `dns.go` interfaces and `DNSConfigFrom`; `dns_cloudflare.go`
- [x] T007 Schema `dns` block, `config.Company.DNS`, fixtures `company-dns-*`, `company-digitalocean-cloudflare.yaml`

## Phase 3: CLI

- [x] T008 Tests in `core/cli/digitalocean_test.go`: plan touches nothing (text/JSON), up/status/down end to end with both fakes, DNS problems stop before any server, `--dns manual` override, token never in output, down plan text, validate output
- [x] T009 `up.go`: DNS plugin selection, planner lines, preflight, removal before destroy, warnings
- [x] T010 `ResolveSecret` no longer echoes an invalid reference (research D12)

## Phase 4: Docs

- [x] T011 `docs/providers.md`, `docs/make-up.md`, `docs/epics/README.md`, [measurements.md](measurements.md)
- [x] T012 `make test` green, race green, rebase, PR, CI

## Blocked (human-gated, real accounts) `[V]`

See [measurements.md](measurements.md): V001-V008.
