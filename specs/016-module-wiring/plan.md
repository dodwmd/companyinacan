# Implementation Plan: Module wiring (Epic E03c)

**Branch**: `epic/E03c-module-wiring` | **Spec**: [spec.md](spec.md)

## Summary

can-core gains personal access tokens, token introspection for them, generated module secrets with
rotation and a rollout when they change, and idempotent event publishing. A new shared library,
`core/modclient`, is what a module uses to talk to can-core (introspection with a bounded cache, event
publishing, approvals, signed webhook receiver). Each in-house module gets an authenticator, an event
path through can-core and, where needed, an inbound event handler. Charts are added per module.
Choices in [research.md](research.md), tables in [data-model.md](data-model.md), wire contracts in
[contracts/](contracts/).

## Technical context

Go (existing module), standard library plus the existing `modernc.org/sqlite`. No new dependency.
Tests: `go test`, `httptest`, fake clock, the real can-core services in-process (`test/wiring`), a fake
can-core for module unit tests (`core/modclient/fakecore`), the installer's fake Kubernetes API. No
cluster, no network. The only cluster run is the advisory dev-cluster workflow.

## Constitution check

| Principle | Check |
|---|---|
| I. Spec-first | SPEC.md 3.1, 3.7 and 4.3 are the source; the introspection answer, `source_id` and the token routes are additive to `specs/005-can-core-mvp/contracts`, which were updated in place. |
| II. Footprint | can-core idle RSS 41.9 MiB (base 41.9 MiB) and 60.9 MiB after 4,000 requests (base 60.2 MiB); work idle 13.6 MiB (base 13.5 MiB). Caches are bounded (1,024 introspection answers per module, 4,096 seen event ids, 50 live tokens per module). |
| III. Test-first | Tests came first (seen failing) for the can-core side, `modclient`, finance approvals, the hire hand-off and the keyring loaders; for the module authenticators and event paths the code came first and the tests after (see tasks.md, which says so per task). The end-to-end suite runs the real can-core services with the real modules. |
| IV. Stable contracts | Additive: new routes, new optional request/response members, new manifest `subscribes`/`approvals`/`notifications` entries. The module event schemas are unchanged. |
| V. Secure and private by default | Tokens hashed at rest and shown once; a module learns about its own tokens only; permissions limited to what the issuer holds; fail closed on any doubt; secrets never logged, printed or placed in events; no personal data in the events can-core keeps (forms sends ids only; people's hand-off is a placeholder). |
| VI. OSS hygiene | No new dependency. |
| VII. Simplicity and measured claims | One shared client instead of five copies; RAM measured and reported; what ran on a cluster and what did not is stated in `docs/module-wiring.md`. |

## Project structure

```
core/server/pat/                 issue, revoke, list, introspect personal access tokens
core/server/modsecrets/          which secrets are generated per module, how to rotate
core/server/store/wiring.go      tokens, publish de-duplication, rotation requests (migration 0004)
core/server/{moduleapi,adminapi,adminui}   introspection of tokens, /api/v1/tokens, /tokens page
core/server/{installer,reconcile,corecli}  generated secrets, rollout on change, `admin secrets rotate`
core/modclient/                  module-side client, introspector, receiver; fakecore/ for tests
core/<module>/canauth.go, canwiring_test.go ...   authenticators and event paths
core/{jobs,finance}/...approv... requisition and invoice approvals; core/people/hire.go
modules/<module>/chart/          Helm templates
test/wiring/                     end-to-end tests with the real can-core services
test/integration/work_test.go    advisory k3d run
```
