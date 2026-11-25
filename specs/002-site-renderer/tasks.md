---

description: "Task list for Epic E04 - Site renderer and static server"
---

# Tasks: Site Renderer and Static Server (Epic E04)

**Input**: Design documents from `/specs/002-site-renderer/` (plan.md, spec.md, research.md, data-model.md)

**Tests**: Requested (Constitution III). In every story the test tasks come first and are observed
failing (no implementation, so the package does not compile) before the implementation tasks.

## Format: `[ID] [P?] [Story] Description`

## Phase 1: Foundation (schema and config)

- [x] T001 Extend `site:` in `schemas/company.v1.schema.json` additively (hero, features, pricing, faq, cta, footer, description, lang, og_image, noindex, same_as; shared `$defs` href and button)
- [x] T002 [P] Add the typed `Site` view in `core/config/site.go` and the `Site` field on `Company`
- [x] T003 [P] Add fixtures: `testdata/valid/company-site-full.yaml`, `testdata/invalid/company-site-bad-href.yaml`, `company-site-bad-sections.yaml` (with sidecar expectations), `testdata/site/logo.svg`; the existing fixture contract tests pick them up

## Phase 2: User Story 1 and 2 - Renderer (P1)

- [x] T004 [US1] Write failing tests in `core/site/site_test.go`: golden output, determinism, expected files, minimal site, rejected inputs (table), all-problems-together, href allow-list, contrast
- [x] T005 [US2] Failing tests in the same file: SEO tags, JSON-LD, noindex, base URL override, accessibility structure, CSS properties, HTML escaping of hostile content in every field
- [x] T006 [US1] Failing tests in `core/site/write_test.go`: tree creation, stale removal, refusal of foreign directories, symlink safety, gzip siblings, deterministic gzip
- [x] T007 [US1] Implement `core/site/site.go`, `write.go` and `templates/classic/*` (layout, sections, `site.css`)
- [x] T008 [US1] Generate and review golden files in `testdata/site/golden` (`go test ./core/site -update`)
- [x] T009 [P] [US1] Contract test `test/contract/site_test.go`: every valid company fixture and the example renders

## Phase 3: User Story 3 - Static server (P1)

- [x] T010 [US3] Failing tests in `core/staticsrv/server_test.go`: pages, redirects, content types, methods, security headers, healthz, 404, caching, conditional and range, precompressed, hidden files, traversal table, fuzz target, timeouts, access log, directory swap
- [x] T011 [US3] Implement `core/staticsrv/server.go`
- [x] T012 [US3] Failing tests in `cmd/can-static/main_test.go`, then implement `cmd/can-static/main.go` (flags, env defaults, graceful shutdown, `-healthcheck`)
- [x] T013 [P] [US3] `deploy/can-static/Dockerfile` (distroless, non-root, digest placeholders) and README with the RAM budget
- [x] T014 [US3] Measure idle and loaded RSS and record in `measurements.md`; run the fuzz target

## Phase 4: CLI and preview (P1/P2)

- [x] T015 [US1] Failing tests in `core/cli/site_test.go` (build, default out, flags, validation problems write nothing, foreign dir, JSON, usage, example) then implement `can site build`
- [x] T016 [US4] Tests and implementation for `can site serve` (base URL from the listen address, rebuild on change, last good build kept, `--public`)
- [x] T017 [P] Update `examples/acme-co` with a full site and mention the commands in its README; add `bin/can-static` to `make build`

## Phase 5: Polish

- [x] T018 Run `make test` and `go run ./cmd/can licences --check`; fix lint findings
- [x] T019 Spec artifacts: spec, plan (constitution check), research, data-model, measurements
- [x] T020 Update the E04 row in `docs/epics/README.md` (last commit)

## Known gaps (not in this epic)

- `static` provider deploy to Cloudflare Pages / GitHub Pages (epic stretch goal)
- Writing the build to the PVC from can-core (E03 calls `Output.Write`); pod manifests (E02/E06)
- Brotli precompression; multi-page sites and localised section headings; a second template
- Image size measurement; RAM measured outside the cluster only
