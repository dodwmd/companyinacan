# Implementation Plan: DigitalOcean Provider and Cloudflare DNS (Epic E12c)

**Branch**: `epic/E12c-digitalocean-cloudflare` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Add the `digitalocean` provider (replacing the E06 stub), a `cloudflare` DNS plugin and optional DNS
plugin interfaces (`DNSRemover`, `DNSPlanner`, `DNSChecker`), a `dns` block in `company.yaml` with
semantic validation, and wire them into `can up|down|status`. Research and open questions:
[research.md](research.md); entities: [data-model.md](data-model.md); contracts:
[contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27 (existing module). **Dependencies**: none added (`net/http`).
**Storage**: `.can/state.json` (version stays 1; new optional fields only).
**Testing**: `go test -race`; `httptest` fakes with `testdata/digitalocean/fixtures.json` and
in-test Cloudflare fakes; CLI tests in-process; schema fixtures in `testdata/valid|invalid`.
**Constraints**: no real accounts, servers or tokens; the token never appears in output.

## Constitution Check

| Principle | Assessment |
|---|---|
| I. Spec-first | SPEC.md 3.4/3.5 name the DigitalOcean provider and Cloudflare plugin; this feature restates only what it builds. |
| II. Footprint is a feature | No resident component: both are CLI-side clients. The default droplet is the smallest plan meeting the 2 GB target. |
| III. Test-first | Provider, API client, DNS plugin, validation and CLI tests were written before the implementation; the contract fixtures failed first (8 of them) and then passed. |
| IV. Stable module contracts | No module contract touched. `company.yaml` gains an optional `dns` block (additive, schema version stays 1). |
| V. Secure and private by default | Tag-based ownership before adopt/delete, re-check before delete, 6443/10250 never opened, SSH port fixed to 22, secrets only by reference, redaction end to end, tokens never in state, API `next` URLs never followed. |
| VI. Open-source hygiene | No new dependency; commits signed off. |
| VII. Simplicity and measured claims | Everything needing a real account is listed in [measurements.md](measurements.md); guessed defaults and API shapes are flagged in code and docs. |

**Result**: pass, no violations to justify.

## Project Structure

```text
core/provider/
  digitalocean.go      provider (conf, Plan, Create, Destroy, Status, Address, discover)
  digitalocean_api.go  API client: retries, pagination, ownership helpers
  dns.go               DNS interfaces (+ DNSRemover/DNSPlanner/DNSChecker), DNSConfig, NewDNSWith
  dns_cloudflare.go    Cloudflare plugin
  check.go             semantic checks used by `can validate`
  state.go             + firewall_uuid, dns_records
core/cli/up.go         dns plugin selection, DNS preflight, DNS removal before destroy
core/validate/company.go  calls provider.Check*
core/config/company.go    + DNS map
schemas/company.v1.schema.json  + dns block
testdata/digitalocean fixtures, testdata/valid|invalid company-do*/company-dns*
```

## Design decisions in brief

- State format is unchanged for existing users: `firewall_uuid` (DigitalOcean ids are UUID strings)
  and `dns_records` are optional additions; `server.id` and `ssh_key_id` are the numeric droplet and
  key ids.
- Destroy order: Cloudflare records, then droplet (wait until 404), firewall, key.
- `provider.options` stay a free map in the JSON schema; `can validate` applies per-provider
  semantic rules (a cross-package schema `if/then` per provider would duplicate the Go allow-list).
