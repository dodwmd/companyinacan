# Implementation Plan: Wiki, CRM and Status Page (Epic E10a)

**Branch**: `epic/E10a-wiki-crm-status` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Three wrapper modules built to the E07 pattern (specs/007-starter-modules), plus three capability
contracts, a lead-to-CRM adapter design and a catalogue document. Choices: [research.md](research.md);
entities: [data-model.md](data-model.md); contracts: [contracts/](contracts/).

## Technical Context

Go 1.27 (tests only), Helm templates, POSIX sh hooks, JSON Schema 2020-12. No new dependency.
Testing: `test/contract` (extends the `starters` table in `starter_modules_test.go`; new
`wrapped_modules_test.go`), `core/server/installer` (`TestStarterModuleChartsStillInstall` list).
No cluster, container runtime or network for builds; helm is used when on PATH.

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to SPEC.md 4.3, 5, 6.1, 6.5. Deviation: wiki is DokuWiki, not BookStack/Wiki.js (SPEC.md row updated; reasons in R-wiki) |
| II. Footprint | PASS with caveat | Declared RAM and limits per module, chart sums asserted against manifests; all figures are unmeasured estimates (R-footprint); the flat-file wiki and SQLite status page avoid two extra database servers |
| III. Test-first | PASS | `starters` entries, capability fixtures, installer list and `wrapped_modules_test.go` were added before any module file and observed failing (compile error: missing capability schemas, then missing modules, READMEs, specs) |
| IV. Stable module contracts | PASS | Capabilities versioned `v1` with flagged write verbs and fixtures; modules couple only through capabilities and events; adapters `planned` |
| V. Secure and private | PASS | Restricted pods, named Secrets only, no sensitive classes, admin UI of the status page not routed, CRM erasure and retention documented, lead mapping minimises fields |
| VI. OSS hygiene | PASS with open points | GPL-2.0, AGPL-3.0, MIT are on the accepted list; sources and uncertainties in R-licences; no source-available module; `THIRD_PARTY_LICENSES` regenerated; DCO sign-off |
| VII. Simplicity, measured claims | PASS | Unmeasured numbers are labelled so; unverifiable tags and digests are placeholders |

No violations; Complexity Tracking not needed.

## Structure

```text
modules/{wiki,crm,status}/ module.yaml README.md settings.schema.json chart/ hooks/{backup,restore}.sh
schemas/capabilities/{wiki,crm,status}.v1.json (+ embed.go), schemas/names.yaml
testdata/capabilities/{wiki,crm,status}.v1/*.json
test/contract/{starter_modules_test.go (table), wrapped_modules_test.go, starter_charts_sync_test.go (status route exemption)}
core/server/installer/hostile_test.go (install list)
docs/modules-catalogue.md, modules/README.md, SPEC.md rows, docs/epics/README.md, THIRD_PARTY_LICENSES
```

## Design decisions

1. **Reuse the starter chart templates** (service, pvc, networkpolicy, adapter, helpers identical;
   a drift test covers them). Status has its own `ingressroute.yaml` (public paths only). CRM adds
   a daemon container in the same pod and a `_crm.tpl` for env and mounts.
2. **One pod per module**, `Recreate` strategy: RWO volumes, SQLite and flat files have one writer.
3. **Adapters are chart slots, off by default**, matching `provides[].status: planned` (the existing
   drift test ties the two).
4. **Status markers are honest:** crm `not-deployable` (root-looking image on port 80 and unresolved
   tag), wiki and status `experimental`.
5. **Admin restriction by omission,** not by middleware: the installer allowlist has no Middleware.

## Verification without a cluster

| Claim | Evidence |
|---|---|
| Manifests valid, licences accepted | contract tests, `can licences --check` |
| Charts admissible and within footprint | helm render assertions, values assertions, installer test with fakes |
| Contracts well formed | schema compile plus fixtures |
| Hooks syntactically sound | `sh -n` and static assertions |
| Status admin not routed | helm render assertion on the rules |
| Pods run, probes pass, dumps restore, mail and leads flow, RAM figures | NOT verified (tasks Phase 7) |
