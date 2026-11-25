# Implementation Plan: can-core MVP (Epic E03, part 1)

**Branch**: `epic/E03a-can-core` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Build the can-core control plane as a single small Go binary (`cmd/can-core`) over packages in
`core/server/...`: SQLite state and repositories, admin authentication, module authentication through a
TokenReview interface, the capability registry, a hash-chained audit log, HMAC-signed webhook delivery from a
SQLite outbox, an in-process event bus, two HTTP APIs with OpenAPI contracts, and `can apply`
reconciliation through a `ModuleInstaller` interface. Everything that needs Kubernetes is behind an
interface with an in-memory fake; the real Helm installer and cluster integration tests are part 2.
Decisions are in [research.md](research.md), entities in [data-model.md](data-model.md), HTTP in
[contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27

**Primary Dependencies** (added): `modernc.org/sqlite` (pure-Go SQLite), `golang.org/x/crypto` (argon2).
Existing: cobra, `go.yaml.in/yaml/v3`. Everything else is the standard library (`net/http`, `log/slog`,
`crypto/*`). No client-go, no web framework, no ORM.

**Storage**: SQLite file on a PVC in `can-system` (WAL, one connection).

**Testing**: `go test` with in-memory SQLite, `httptest`, fake TokenReview and fake installer; contract tests under
`test/contract`; no network, cluster or container runtime.

**Target Platform**: Linux container (distroless/static later); static binary, CGO off.

**Performance / footprint goals**: **idle RAM target: 48 MB request, 96 MB limit** (declared `ram_mb` /
`ram_limit_mb` for the future chart), inside the SPEC section 3.2 budget of about 100 MB for can-core + site +
forms. Measured on the stripped binary: 38 MB RSS right after start, 55 MB after Argon2 logins (see
[measurements.md](measurements.md)). A soft Go memory limit (default 64 MiB, `CAN_MEMLIMIT_MB`) keeps the heap
from growing past that. Argon2 uses the OWASP minimum memory (19 MiB) for this reason.

**Constraints**: two listeners on separate ports; modules reach only the module-API port; admin API only
through Traefik. Secrets never logged.

**Scale/Scope**: one admin, tens of modules, tens of events per minute.

## Constitution Check (before research)

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Behaviour traces to SPEC 3.1, 3.3, 3.7, 4.2-4.5; spec.md written first and states what this PR delivers |
| II. Footprint | PASS | RAM request/limit declared above and measured; no sidecars; client-go avoided to save binary size and RAM |
| III. Test-first | PASS | Tests written first per package and observed failing (compile or red runs); Kubernetes behind fakes; k3d integration test deferred to part 2 and listed in tasks |
| IV. Module contracts | PASS | Registry and bus consume only manifest fields (`provides`, `requires`, `consumes`, `publishes`, `subscribes`) |
| V. Secure/private | PASS | Mandatory TOTP, argon2id, CSRF, lockout, hashed session tokens, signed webhooks, audit log holds ids only, secrets excluded from API views and logs |
| VI. OSS hygiene | PASS | Dependencies are BSD-3 (x/crypto, modernc.org/sqlite and its deps); no secrets committed |
| VII. Simplicity | PASS | SQLite, stdlib HTTP, hand-written TOTP (about 60 lines, RFC vectors) instead of a dependency; justified entries in Complexity Tracking |

## Project Structure

```text
cmd/can-core/main.go                 # thin: corecli.Execute
core/server/
├── store/        # SQLite open, embedded migrations, repositories (admin, sessions, modules, events, deliveries)
├── audit/        # hash-chained log, Verify, Head, Export
├── auth/         # argon2id, TOTP, sessions, CSRF, throttling/lockout
├── tokenreview/  # Reviewer interface, Fake, net/http implementation
├── modauth/      # token -> installed module
├── registry/     # capability registry and resolution
├── events/       # catalogue, envelope, in-process bus + outbox fan-out
├── webhook/      # signing, verification, dispatcher, delivery log
├── reconcile/    # Desired/Plan/Apply, ModuleInstaller, FakeInstaller, RecordOnly
├── httpx/        # shared HTTP helpers
├── adminapi/     # admin HTTP API
├── moduleapi/    # module HTTP API
├── runtime/      # config, listeners, workers, graceful shutdown
├── corecli/      # serve, audit, admin bootstrap, version
└── lifecycle_test.go   # stub-module lifecycle (epic "done when", in-process)
core/cli/apply.go                    # `can apply` (surgical change)
specs/005-can-core-mvp/contracts/    # admin-api.yaml, module-api.yaml (OpenAPI 3.1)
test/contract/cancore_api_test.go    # contract <-> handler parity
```

**Structure Decision**: one package per concern with one-directional imports (store at the bottom; adminapi,
moduleapi and runtime at the top). Domain packages depend on `store`, never on each other's HTTP.

## Complexity Tracking

| Item | Why | Simpler alternative rejected because |
|---|---|---|
| Own TOTP implementation | 60 lines, verified with RFC 6238 vectors for SHA1/256/512, avoids a dependency in the auth path | `pquerna/otp` is unmaintained |
| TokenReview over `net/http` instead of client-go | client-go adds roughly 20 MB to the binary and noticeable RSS; one POST needs none of it | client-go also needed in part 2 for the installer, then reassess |
| Single SQLite connection | removes locking classes of bugs at this scale | pool of readers adds complexity without need |

## Constitution Check (after design)

No violations. Open item carried to part 2: Principle III requires k3d integration tests for token review, Helm
upgrades and NetworkPolicies; they are explicitly out of this PR because no cluster exists in this environment.

## Follow-ups (part 2 and later)

Done in part 2 (tasks.md): real TokenReview proven on k3d, installer (R16: no Helm SDK), DB creation and credential
injection (Postgres), `company.yaml` ConfigMap and `can apply`, TokenReview caching and rate limit, admission
policy (R17), can-core manifests and the stub-module lifecycle on k3d.

Still open:
1. `email.send` and SMTP relay credential injection.
2. Admin API enable/disable endpoints and the minimal admin UI.
3. Uninstall with keep/purge; module data export.
4. IP allowlist middleware option; session list/revoke endpoints.
5. MariaDB provisioner; the full Helm engine for upstream charts (R16); `can apply --dry-run` against cluster state.
6. Transport security for the module API (see research R15).
