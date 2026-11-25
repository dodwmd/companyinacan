# Implementation Plan: Jobs and Recruiting, first slice (E16a)

**Branch**: `epic/E16a-jobs` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

One in-house Go service, `cmd/can-jobs` over `core/jobs`, built the way the work tracker (E14a) and people (E13a) were: SQLite with embedded migrations behind one write connection and a read-only pool, an `Authenticator` seam, a JSON API under `/v1` plus public HTML under `/careers`, a signed webhook outbox, a manifest, a values-only chart and a Dockerfile fragment. Rules live in the store so every path gets them; visibility is two small functions (`jobPred`, `appPred`) that every query ANDs on.

## Technical Context

**Language**: Go 1.27. **Dependencies**: stdlib, `modernc.org/sqlite`, `golang.org/x/crypto` (chacha20poly1305), `go.yaml.in/yaml/v3`, and `core/forms` for the timing-token signer and CAPTCHA verifier. No new module dependencies. **Storage**: SQLite WAL with `secure_delete`; resume files on the volume. **Testing**: `go test -race`, golden files for feeds and JSON-LD, contract tests in `test/contract`. **Constraints**: idle RAM 30 MB, limit 64 MB, no cgo, fail closed.

## Constitution Check

| Principle | Assessment |
|---|---|
| I Spec-first | Implements SPEC.md *Recruiting & job-board syndication*. Divergences recorded in research R9: events `job.opened`, `application.stage_changed`, `candidate.hired` added next to the catalogue's `job.published`, `application.received`, `offer.accepted` (kept; offers are deferred). Capability `ats` added beside `jobs`. |
| II Footprint | 30/64 MB declared; measured 16 MiB idle, 23 MiB after load; no sidecar. |
| III Test-first | Feeds, JSON-LD, resume checks and syndicators were written red, then green. For the store and API the tests were written against the designed API first (the package did not compile until the implementation existed), and the security-critical ones were mutation-checked (removing the token-scope clause, or the blind-feedback rule, fails them). See tasks.md for the honest order. |
| IV Stable contracts | `jobs.v1`, `ats.v1`, four event schemas with fixtures, OpenAPI checked against the router; no reach into other modules. |
| V Secure and private | In-house only; sealed fields; audited reads that fail closed; retention, legal hold and erasure tombstones delivered with the first module that stores candidate data; diversity data separated; ClusterIP API; restricted security context; disclaimer in docs. |
| VI Open-source hygiene | Apache-2.0, fake data only, no new dependencies. |
| VII Simplicity and measured claims | Boring stack; RAM measured; the docs say what is not done (no scanner, no board connectors, not run on a cluster). |

## Structure

```text
cmd/can-jobs/main.go
core/jobs/
  model.go jobs.go             job model, validation, lifecycle, approval hook, syndication calls
  candidates.go pipeline.go    intake, candidates, applications, stages, interviewers
  scorecards.go                feedback with blind-until-submitted visibility
  scope.go auth.go tokens.go   role matrix, visibility predicates, field masks, tokens
  store.go retention.go eeo.go sealing, audit, scrub, retention and erasure, diversity aggregates
  feeds.go syndicate.go        JSON-LD, sitemap, XML feeds; Syndicator, fake, partner stub
  resume.go public.go          resume store, sniff, scanner; careers pages and apply
  api.go httputil.go events.go app.go config.go cmd.go
  migrations/                  001 schema, 002 append-only audit triggers
modules/jobs/  deploy/jobs/  schemas/capabilities/{jobs,ats}.v1.json  schemas/events/*
specs/013-jobs-recruiting/   docs/recruiting.md   test/contract/jobs_test.go
```

Shared files touched minimally: `schemas/{names.yaml,capabilities/embed.go,events/embed.go}`, `core/server/events/events.go` (catalogue), `modules/README.md`, `docs/epics/README.md`.

## Complexity tracking

| Choice | Why |
|---|---|
| One listener for public pages and `/v1` | One pod and one port keeps the footprint down; the ingress exposes only the public paths and the chart says so. A second listener is a later change if a network policy needs it. |
| Import `core/forms` | Reuses the QA'd timing-token signer and CAPTCHA verifier instead of copying them. |
| Email dedupe key separate from the keyring | A stolen database must not allow a dictionary check of addresses, and rotating encryption keys must not change the hash. |
| Diversity table without any link | Interviewers and recruiters must not be able to join answers to people, even through the service. |
