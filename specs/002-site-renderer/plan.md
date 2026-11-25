# Implementation Plan: Site Renderer and Static Server (Epic E04)

**Branch**: `epic/E04-site-renderer` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/002-site-renderer/spec.md`

## Summary

A renderer library `core/site` (templates and CSS embedded with `go:embed`, `html/template`
for escaping), a shared hardened handler `core/staticsrv`, the `can site build|serve` commands,
the `cmd/can-static` binary for the site pod, and `deploy/can-static` (Dockerfile and README with
the measured RAM budget). The `site:` schema section grows additively. No new dependency.
Approach in [research.md](research.md); entities in [data-model.md](data-model.md).

## Technical Context

**Language/Version**: Go 1.27 (existing module)

**Primary Dependencies**: none new. Standard library (`html/template`, `embed`, `os.Root`,
`net/http`, `compress/gzip`); existing cobra, yaml and jsonschema packages for the CLI and validation

**Storage**: Files only. Output goes to a directory (a PVC in the cluster)

**Testing**: `go test`; golden files under `testdata/site/golden`; table and `go test -fuzz`
tests for path traversal; in-process CLI and server tests; no network beyond loopback, no containers

**Target Platform**: Linux (CI and cluster); the renderer is portable

**Project Type**: Libraries plus two binaries (`can`, `can-static`)

**Performance Goals**: build under 1 s (measured about 20 ms); can-static idle under 8 MiB

**Constraints**: deterministic output; no JavaScript on the page; the server never writes to disk

**Scale/Scope**: one page, about 12 template files and one stylesheet; ~1.2 k lines of non-test Go

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to SPEC §3.1 (Site), §3.2 (~5 MB static server), §4.1/§4.2 (renderer library, SEO basics, sections driven by `company.yaml`). The `site:` field names beyond `tagline`/`template`/`brand` are new and recorded in data-model.md; SPEC §3.3 should list them (follow-up noted in research R9) |
| II. Footprint | PASS | can-static measured: ~6.7 MiB idle, ~13 MiB peak under 32-way load; budget 16 MiB request, 32 MiB limit (deploy/can-static/README.md, measurements.md). The renderer runs inside can-core/CLI and holds no resident state |
| III. Test-first | PASS | Tests for renderer, writer, server and CLI were written before their implementation (tasks.md orders them); golden files reviewed by eye |
| IV. Module contracts | PASS (n/a) | The site is a built-in component, not a module; no capability or event is added |
| V. Secure/private | PASS | Contextual escaping everywhere, href allow-list, strict CSP and security headers, `os.Root` path confinement, no secrets in the site section, fake data in fixtures |
| VI. OSS hygiene | PASS | No new dependency, so `THIRD_PARTY_LICENSES` is unchanged; `can licences --check` passes; DCO sign-off on commits |
| VII. Simplicity/measured | PASS | One template, one page, no template-engine abstraction beyond `html/template`; the RAM and build-time numbers are measured, not claimed |

Post-design re-check: no violations; Complexity Tracking is empty.

## Project Structure

```text
specs/002-site-renderer/   spec.md plan.md research.md data-model.md tasks.md measurements.md
schemas/company.v1.schema.json     # site: extended additively
core/config/site.go                # typed view of site:
core/site/                         # renderer (site.go), writer (write.go), templates/classic/*
core/staticsrv/                    # hardened static handler, used by can-static and `can site serve`
core/cli/site.go                   # can site build | serve
cmd/can-static/                    # site pod binary
deploy/can-static/                 # Dockerfile, README with RAM budget
testdata/valid/company-site-full.yaml, testdata/invalid/company-site-*.yaml
testdata/site/                     # logo fixture and golden output
test/contract/site_test.go         # every valid fixture and the example render
```

**Structure Decision**: The handler lives in `core/staticsrv`, not in `cmd/can-static`, so the
CLI preview and the production pod run the very same code and tests.

## Complexity Tracking

None.
