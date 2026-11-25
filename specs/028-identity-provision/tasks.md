# Tasks: Identity Provisioning (E13d)

Status: all built offline. `[x]` = done and covered by a test.

## can-core
- [x] T01 offboarded-party facts from the people module only; revocation with subject binding, idempotent, audited in one transaction, rate limited (`core/server/identity`)
- [x] T02 `POST /v1/identity/revoke` on the module API, OpenAPI entry, runtime wiring (`core/server/moduleapi`, `runtime`)
- [x] T03 `person.offboarded.v1` carries `subject_ref` (schema, people service, tests)
- [x] T04 `modclient.RevokeIdentity`
- [x] T05 generated `CAN_PROVISIONING_API_TOKEN`; event catalogue entries

## Module
- [x] T10 SCIM filter parser (bounded, linear, fuzz target)
- [x] T11 SCIM server: discovery, users, groups, PATCH, ETag, scoped tokens, strict content type, errors
- [x] T12 Store, migrations, hash-chained audit, outbox
- [x] T13 Connector interface; `scim-client`, `webhook`; stubs for `google-workspace`, `github`, `slack`; SSRF-safe client
- [x] T14 Planner (joiner, mover, leaver), effective dating, urgent
- [x] T15 Engine: persisted steps, retries and backoff, dead letters, compensation, approvals gate, tasks, kill switch
- [x] T16 Event handling and delivery; trigger verification against the directory
- [x] T17 Provisioning API, authentication (service token, personal access tokens), admin actions
- [x] T18 Config, `Main`, healthcheck, worker loop
- [x] T19 `module.yaml`, chart, settings schema, Dockerfile fragment, `identity.provision.v1`, event schemas and fixtures, release wiring
- [x] T20 Contract tests: manifest, fixtures, OpenAPI vs routes, engine output vs schemas

## Docs and measurement
- [x] T30 `docs/provisioning.md`, module and chart READMEs, `docs/epics/README.md`, catalogue row
- [x] T31 RAM measured locally (research R9)

## Not done (documented gaps)
- [ ] G1 egress NetworkPolicy for the module namespace (people API, connector hosts)
- [ ] G2 a supported way to provide the directory token and connector credentials
- [ ] G3 real connectors for Google Workspace, GitHub, Slack; real IdP trials
- [ ] G4 a "department changed" people event for movers
- [ ] G5 run on a cluster
