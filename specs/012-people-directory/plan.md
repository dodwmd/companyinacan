# Implementation Plan: People Directory and Performance, first slice (E13a)

**Branch**: `epic/E13a-people` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

One in-house Go service, `cmd/can-people` over library `core/people`, mirroring the forms gateway (E05): a
SQLite store with embedded migrations, an `Authenticator` seam, a REST JSON API on a ClusterIP-only
listener, an outbox for events, a manifest, a values-only chart and a Dockerfile fragment. Authorisation is
a single pure function (`Decide`/`FieldAccess`); everything else resolves facts and calls it.

## Technical Context

**Language/Version**: Go 1.27 (as the module). **Dependencies**: stdlib, `modernc.org/sqlite`,
`golang.org/x/crypto/chacha20poly1305` and `go.yaml.in/yaml/v3` (all already in `go.mod`; no new ones).
**Storage**: SQLite (WAL, `secure_delete`, one connection). **Testing**: `go test` with `-race`, contract
tests in `test/contract`. **Target**: Linux container, distroless static. **Constraints**: idle RAM 30 MB,
limit 64 MB; no cgo; fail closed.

## Constitution Check (before research)

| Principle | Assessment |
|---|---|
| I Spec-first | Refines SPEC.md 3.7, 4.3 and *Managing people and work*. One divergence (event names `person.created/status_changed/offboarded` instead of `person.hired/left`) is recorded in SPEC.md in the same change. |
| II Footprint | 30 MB / 64 MB declared in manifest and chart; measured locally; no sidecar, no extra always-on dependency. |
| III Test-first | Crypto and policy tests were written and observed failing first; the service, API and contract tests followed the implementation of each slice (see tasks.md for the honest order). Schemas have fixtures; routes are checked against OpenAPI. k3d integration tests are pending. |
| IV Stable contracts | Capabilities `directory.v1`, `performance.v1` and six event schemas with fixtures; no access to other modules' internals. |
| V Secure and private | In-house only; per-field access control; audited reads; per-field encryption; retention, legal hold, erasure tombstones delivered **with** the first module that stores personal data; ClusterIP only; restricted security context; documents the tool-not-advice disclaimer. |
| VI Open-source hygiene | Apache-2.0; no new deps; fake data only. |
| VII Simplicity and measured claims | Boring stack; RAM measured and recorded, not asserted. Complexity in the table below. |

## Project Structure

```text
cmd/can-people/main.go            entry point
core/people/
  policy.go                       the single authorisation layer (pure)
  crypto.go                       keyring, XChaCha20-Poly1305 seal/open, rotation
  service.go                      Service, store open/migrate, actor resolution, relationship facts
  directory.go                    people, org, sensitive fields, lifecycle, history
  checklists.go oneonone.go goals.go retention.go   domain operations
  audit.go events.go              audit hook + hash chain; outbox + signed dispatcher
  api.go auth.go config.go cmd.go REST surface, authenticators, config, CLI
  migrations/001_people.sql
modules/people/                   module.yaml, settings schema, chart values, events copies, README
deploy/people/                    Dockerfile fragment, README
schemas/capabilities/{directory,performance}.v1.json
schemas/events/{person.created,...}.v1.json      + testdata/ fixtures
specs/012-people-directory/       this directory
docs/people.md
test/contract/people_test.go
```

## Design decisions

1. **One policy function.** `Decide(actor, action, fact)` and `FieldAccess(actor, write, field, fact)` are
   pure. The service computes `Fact` (self, direct manager, anywhere in chain, same team, team lead,
   assigned, subject status, goal scope and visibility) from the store. Table tests plus exhaustive
   enumeration over all role subsets x person statuses x facts assert invariants (blocked actors get
   nothing, private notes only for the direct manager, no sensitive field path for managers, ...).
2. **404 before 403.** Existence is revealed only to callers who may see the record.
3. **Terminated-manager safety is data, not token.** The actor's own person record (via `sso_subject`) is
   consulted on every call; `former`/`candidate` block everything, `leave`/`offboarding` drop elevated roles.
   Becoming former also clears the reporting line.
4. **Audit before data.** Sensitive reads call the `Auditor` first and fail if it fails.
5. **Encryption bound to context.** AAD is `<id>:<field>`; the key id travels in the ciphertext so rotation
   re-seals lazily with `rotate-keys`.
6. **Erasure subject = the person's own random id.** It is already opaque and matches the E08 ledger patterns;
   no mapping table, nothing to leak. can-core pulls `GET /v1/erasures`; restore replays with the purge hook.
7. **Outbox in the same transaction** as the change; payloads are ids and machine values only.
8. **One SQLite connection** serialises writes, so the cycle check cannot race, and keeps idle RAM low.

## Complexity Tracking

| Addition | Why | Simpler alternative rejected because |
|---|---|---|
| Hash-chained audit table inside the module | Audit hook must work before can-core's audit accepts module events | A plain log cannot show tampering; the hook interface lets can-core replace it later |
| Introspection client + static tokens | Same seam as forms; users need identities, can-core needs a service account | Static tokens alone cannot carry per-user subjects and roles |
| Exhaustive policy enumeration test | The policy is the security boundary | Hand-picked cases alone miss combinations (status x role x relationship) |

## Constitution Check (after design)

No change: no principle is violated; no new dependency; footprint measured at 13.8 MiB idle. The remaining
risk is Principle III's integration clause (k3d tests), carried in tasks.md Phase 8.
