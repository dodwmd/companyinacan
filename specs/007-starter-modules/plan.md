# Implementation Plan: Starter Modules (Epic E07)

**Branch**: `epic/E07-starter-modules` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Three wrapper modules (support = FreeScout, newsletter = Listmonk, analytics = GoatCounter), each
a manifest, a small Helm chart, backup/restore hooks, a settings schema and a README, plus the
capability and event contracts they publish, the email policy, and the licence plumbing
(EUPL-1.2 accepted, `THIRD_PARTY_LICENSES`). Choices in [research.md](research.md), entities in
[data-model.md](data-model.md), contracts in [contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27 (tests only); Helm chart templates; POSIX sh hooks; JSON Schema 2020-12

**Primary Dependencies**: none new. `santhosh-tekuri/jsonschema/v6` and `go.yaml.in/yaml/v3` already present

**Storage**: per module: MariaDB (support), Postgres (newsletter), SQLite on a PVC (analytics)

**Testing**: `go test ./test/contract/...`; `helm template` when helm is on PATH (CI ubuntu
runners have it; local dev may not), otherwise structural checks on `values.yaml`; `sh -n` on hooks

**Target Platform**: k3s single node, module namespaces `mod-<id>` (pod security `restricted`)

**Constraints**: 1 GB-class starter server (SPEC 3.4); footprint declared per module; no cluster,
network or container runtime during development

**Scale/Scope**: 3 manifests, 3 charts (6 shared templates + 1 workload each), 6 hook scripts,
3 capability and 2 event schemas, about 60 fixtures

## Constitution Check

Constitution: `.specify/memory/constitution.md` (read, not modified).

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to SPEC.md 4.3 (capabilities, events), 5 (catalogue, email policy), 6.5 (backup), 6.1 (footprints). Deviations (capability name `tickets`, hooks image, provisional dump path) recorded in research.md |
| II. Footprint | PASS, with caveat | Declared RAM and limits per module, sum of pod requests asserted against the manifest; GoatCounter chosen over Umami for 5x lower idle RAM and no Postgres. Figures are estimates, not measurements |
| III. Test-first | PASS | `test/contract/starter_modules_test.go` and the fixtures were written first and observed failing (compile error: missing schema packages) before any module file existed |
| IV. Stable module contracts | PASS | Manifests validate under module.v1; capabilities are versioned schemas with flagged write verbs and fixtures; modules couple only through capabilities and events |
| V. Secure and private | PASS | Restricted pod security, no secrets in values/manifests (named Secrets, optional SMTP keys), no sensitive data classes, consent required on subscribe, signature-verified public webhook design, retention declared |
| VI. OSS hygiene | PASS | Licences recorded and accepted (AGPL-3.0, EUPL-1.2 added with rationale); network copyleft documented; `THIRD_PARTY_LICENSES` regenerated; DCO sign-off; no new Go dependency |

No violations; Complexity Tracking not needed.

## Project Structure

```text
modules/{support,newsletter,analytics}/
  module.yaml  README.md  settings.schema.json
  chart/{Chart.yaml,values.yaml,templates/}   # shared templates are byte-identical across charts
  hooks/{backup.sh,restore.sh}
  events/<event>.json                          # support, newsletter: copy of schemas/events
schemas/capabilities/{tickets,newsletter,analytics}.v1.json  (+ embed.go)
schemas/events/{ticket.created,subscriber.added}.v1.json     (+ embed.go entries)
schemas/{names,accepted-licences}.yaml                       (extended)
deploy/hooks/{Dockerfile,README.md}
docs/email-policy.md
testdata/{capabilities,events}/...
test/contract/starter_modules_test.go
scripts/check-image-pins.sh                                  (also scans module charts)
```

## Design decisions

1. **Plain Helm, no templater.** helm is the project's declared chart engine (SPEC 4.2). Tests
   render with helm when present; a Go templater would re-implement Helm to test something Helm
   already does. Without helm the structural checks on `values.yaml` and manifests still run.
2. **Shared templates.** service, ingressroute, pvc, networkpolicy, adapter and `_helpers.tpl`
   are identical in the three charts (embedded in the can-core image, so no library chart
   dependency); only the workload template differs.
3. **Adapter slot.** The capability, event and public-webhook logic lives in an in-house adapter
   (a Go service, later). The chart carries it as an optional second Deployment, off by default,
   so manifests can declare `provides` now and the contract is testable.
4. **Network.** The base module namespace already allows Traefik and can-core in and DNS, can-data
   databases and the internet out. The charts add only an intra-namespace allow (both directions,
   needed because the base egress excludes the pod CIDR) and only when the adapter runs.
5. **Hooks via a shared tools image** (`deploy/hooks`), because the upstream images lack curl and
   DB clients and can-core's image has no shell (SPEC 4.1).

## Verification without a cluster

| Claim | Evidence |
|---|---|
| Manifests valid, licences accepted | `can validate` code path in contract tests; `can licences --check` |
| Chart objects admissible and within footprint | helm render assertions; values assertions |
| Contracts well formed | schema compile plus fixtures |
| Hooks syntactically sound | `sh -n`, static assertions |
| Everything else (pods run, probes pass, dumps restore, mail flows) | NOT verified; listed in research.md and tasks.md Phase 7 |
