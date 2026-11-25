# Implementation Plan: Forms Gateway and `leads` Capability (Epic E05)

**Branch**: `003-forms-gateway` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/003-forms-gateway/spec.md`

## Summary

An in-house Go service (`cmd/can-forms`, library `core/forms`) that receives public form posts at
the same-origin `/_forms/<id>` route, validates and spam-filters them, stores them in SQLite,
notifies by email and signed webhook, and exposes the `leads` capability on a separate
authenticated listener. Ships with `modules/forms/module.yaml`, the `lead.created.v1` event schema
with contract tests, and a distroless image fragment. Choices are in [research.md](research.md),
entities in [data-model.md](data-model.md), wire contracts in [contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27 (existing module)

**Primary Dependencies**: standard library `net/http`, `database/sql`, `log/slog`; new:
`modernc.org/sqlite` v1.63.0 (pure Go). Already present: `go.yaml.in/yaml/v3`,
`santhosh-tekuri/jsonschema/v6` (tests).

**Storage**: SQLite file with embedded migrations

**Testing**: `go test`; handlers driven in-process with `httptest`; fakes for mailer, captcha,
clock; loopback `httptest` servers for webhook, mail and captcha clients; no external network, no
container runtime

**Target Platform**: linux/amd64 and arm64 static binary in distroless

**Project Type**: single service plus library

**Performance Goals**: not throughput bound (human-speed traffic); idle RSS at most 15 MB

**Constraints**: bounded memory everywhere (rate limiter, queue, request bodies, export size);
no cgo; secrets only from environment

**Scale/Scope**: about 2.5 k lines of Go, 1.9 k lines of tests, 2 migrations, 1 event schema

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to SPEC.md sections 3 (forms gateway), 4.3 (capabilities, events, module contract), 6.1 (footprint). Added design details (token endpoint, status set) are recorded here and in research.md |
| II. Footprint | PASS | Idle RSS measured at 13.9 MiB against a 15 MB target; every buffer is bounded; one SQLite connection |
| III. Test-first | PASS | Tests exist for each unit before the code (tasks.md orders them); contract tests for schema and manifest; no network or container runtime |
| IV. Stable module contracts | PASS | Manifest validates under module.v1; capability `leads`, event `lead.created.v1`, permissions and roles declared; API behind an `Authenticator` and mail behind a `Mailer` so E03 plugs in without wire changes |
| V. Secure and private | PASS | Secrets env-only; constant-time token compare; CSV injection neutralised; CRLF and control characters rejected; no redirects followed by webhook; no IP/UA stored, no personal data in logs; retention declared and enforced; same-origin by default, credentials never allowed; security headers; fail-closed captcha |
| VI. OSS hygiene | PASS | Only one new dependency family (modernc, BSD-3-Clause); `can licences --check` passes; DCO commits |
| VII. Simplicity and measured claims | PASS | stdlib router and middleware; footprint numbers measured and recorded in research.md R7; no queue broker or ORM |

Post-design re-check: no violations. Complexity Tracking is empty.

## Project Structure

```text
cmd/can-forms/main.go            # thin entry point
core/forms/
  config.go fields.go            # config + validation
  spam.go                        # timing token, rate limiter, captcha verifier
  store.go migrations/*.sql      # SQLite store and migrations
  public.go api.go auth.go       # the two HTTP surfaces
  notify.go event.go             # email, signed webhook, dispatcher, event envelope
  csv.go httputil.go app.go cmd.go
schemas/events/lead.created.v1.json (+ embed.go)
modules/forms/{module.yaml,README.md,settings.schema.json,chart/,events/}
deploy/forms/{Dockerfile,README.md}
testdata/events/lead.created.v1/{valid,invalid}-*.json
test/contract/forms_test.go
specs/003-forms-gateway/
```

## Design notes

- Two listeners: public `:8080` (Traefik routes `/_forms` here, prefix stripped) and API `:8081`
  (ClusterIP only). The leads API can therefore never be reached through the public route.
- Request pipeline on submit: origin check, rate limit, parse and size-limit, strip reserved
  inputs, honeypot, timing token, captcha, validate, insert, enqueue, respond. Cheap checks run
  first; the lead is committed before any notification.
- Notifications run off the request path on a bounded queue; shutdown drains it.
- Time, mailer, captcha and authenticator are injected, so tests are deterministic.
