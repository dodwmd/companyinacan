# Implementation Plan: Identity Provisioning (Epic E13d)

**Branch**: `epic/E13d-identity-provision` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

An in-house Go module with a SCIM 2.0 server and a workflow engine over SQLite, plus a minimal
can-core addition: `POST /v1/identity/revoke` on the module API. Decisions: [research.md](research.md);
tables: [data-model.md](data-model.md); contract: [contracts/openapi.yaml](contracts/openapi.yaml)
(checked against the registered routes by `test/contract/provision_test.go`).

## Technical Context

**Language/Version**: Go 1.27. **Dependencies**: none added (`modernc.org/sqlite`, `go.yaml.in/yaml/v3`
and `golang.org/x/text` are already in `go.mod`).
**Storage**: SQLite (`core/provision/migrations/001_provision.sql`), one writer connection. can-core
gets migration `0007_identity_revoke.sql` (one small table).
**Testing**: `go test -race`; fakes for the directory, connectors, approvals, can-core and the people
checklist API; `httptest` servers for the SCIM-client and webhook adapters and for SSRF and redirect
behaviour; fixtures for events and the capability. No test contacts a real service.
**Constraints**: idle RSS at most 20 MiB; no goroutine per run (one worker); every call bounded.

## Constitution Check

| Principle | Assessment |
|---|---|
| I. Spec-first | SPEC.md already defines `identity.provision`, the hire and leave flows and the checklist hooks; this feature restates only what it builds and records the deviation that the capability is provided by an in-house module rather than by can-core's SSO user store (research R1). |
| II. Footprint is a feature | One static binary of 12.4 MB, 14 MiB idle, 19 MiB after 600 requests (research R9). One worker goroutine, one SQLite connection, no cache of directory data, scans streamed and bounded. No dependency added. |
| III. Test-first | The can-core revocation (`core/server/identity`) and the SCIM filter parser had their tests written first and failed to compile until the code existed. The engine, connectors, SCIM handlers and API were developed with their tests alongside rather than strictly before, and their first runs failed on real defects (a kill switch step blocking later accounts, group `remove` filter paths, a cached introspection hiding an outage); this plan does not claim more. |
| IV. Stable module contracts | The capability schema `identity.provision.v1`, two event schemas and an optional `subject_ref` in `person.offboarded.v1` (additive; `additionalProperties: false` consumers must accept it) are versioned and fixture-tested. The module API gains one route (additive). |
| V. Secure and private by default | See the spec's SR list and research R3 to R8: SSRF-safe outbound, fail closed, ids only in events and audit, hashed tokens, second person for privileged access, a revocation that can touch only an offboarded person's own account. |
| VI. Open-source hygiene | No new dependency; Apache-2.0; commits signed off. |
| VII. Simplicity and measured claims | Connectors are four methods per operation, workflows are plans of plain steps, no DSL. Everything not run against a real service or cluster is listed in `docs/provisioning.md`. |

**Result**: pass. One justified deviation (R1).

## Project Structure

```text
core/provision/                  config, store, planner, engine, events, SCIM (filter, server, tokens), API, connectors
core/provision/migrations/       001_provision.sql
cmd/can-provision/               main
core/server/identity/            can-core: offboarding facts and the revocation (+ migration 0007)
core/server/moduleapi/           POST /v1/identity/revoke
core/modclient/                  RevokeIdentity
core/people/                     person.offboarded.v1 carries subject_ref
modules/provisioning/            module.yaml, chart, events, settings schema
schemas/capabilities/identity.provision.v1.json, schemas/events/provision.*.json
deploy/provisioning/             Dockerfile fragment
docs/provisioning.md
```

## Not verified

Listed in `docs/provisioning.md` (no real connector, no real IdP, no cluster, NetworkPolicy and secret
provisioning gaps, kill switch preconditions).
