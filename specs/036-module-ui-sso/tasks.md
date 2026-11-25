---
description: "Task list for Epic E11c: single sign-on from the admin UI into module web UIs"
---

# Tasks: Module UI single sign-on (Epic E11c)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Test tasks precede their implementation and were seen failing first. `[V]` marks
work that needs a browser or cluster and is **not** done.

## Phase 1: can-core (US1, US2, US3)

- [x] T001 Migration 0009 (`login_tickets`, `ui_launch_sessions`), `store/tickets.go` (atomic redeem with `UPDATE ... RETURNING`), revocation hooks in `revokeSubjectTokens` and `identity.end`, hourly purge
- [x] T002 Tests then `core/server/ticket`: round trip, replay, 8-way concurrent redeem (one winner), audience and non-burning foreign attempt, expiry and clock, shape, return-path allowlist table, mint refusals (no permission, unknown/disabled user or module, no UI, bad ids), disabled-after-mint, role-removed-after-mint, per-user mint limit, hashed at rest, audit without secrets, failure slowdown but valid never slowed, identical inactive answers, revalidate rules, Apps list, launcher address table
- [x] T003 Tests then `moduleapi` `POST /v1/tickets/redeem|revalidate`: byte-identical inactive answers, a session-cookie-shaped value and a PAT are never tickets, a ticket is not introspectable and introspection does not consume it, 401 without a module token, 503 without the service
- [x] T004 Tests then admin API `GET /api/v1/apps`, `POST /api/v1/apps/{id}/launch` (refusals as 404, CSRF, 400, 429)
- [x] T005 Tests then admin UI `/apps`, `/apps/{id}/launch` (escaped names, no ticket or address on the page, POST-only, interstitial with `no-store` and `no-referrer`, ticket not in logs or audit, rate limit, 503)
- [x] T006 `runtime`: service wiring, `CAN_MODULE_UI_URLS` parser (test), `config.Module.Runtime.WebLogin`, schema `runtime.web_login`, the four in-house manifests
- [x] T007 OpenAPI: `module-api.yaml`, `admin-api.yaml`; `specs/010` route table; contract test `test/contract/sso_routes_test.go`

## Phase 2: modules (US1, US2, US3, US4)

- [x] T008 `modclient`: `RedeemTicket`, `RevalidateSession`, `SessionAPI`, `TicketShaped`; `fakecore` tickets (tests: junk never sent, outage is an error, ticket survives an outage)
- [x] T009 work: `GET/POST /ui/login/ticket`, sso sessions, revalidation every 30 s, `ui.token_login`
- [x] T010 people: `GET/POST /login/ticket`, sso sessions, revalidation 1 min (hr/admin) / `RecheckEvery`, `ui.token_login`
- [x] T011 jobs: same, intervals `strongRecheck` / `defaultRecheck`
- [x] T012 finance: same, intervals `uiRecheckPrivil` / `uiRecheckDefault`
- [x] T013 Each module: GET never consumes, double-submit CSRF and fetch metadata, replay and foreign tickets, fixation, landing allowlist, permissions follow can-core, outage fails closed, token login off, ticket not in logs

- [x] T015 `test/wiring/sso_test.go`: real ticket service, module API, `modclient`, real manifests and the real tracker end to end (launch, interstitial, redeem, signed in, replay refused)

## Phase 2b: QA findings on the first review

- [x] T016 Login CSRF: browser nonce cookie, `binding_hash`, `POST /v1/tickets/peek`, account on the interstitial, confirmation before replacing another account's session; two-browser tests in all four modules
- [x] T017 finance party erasure for ticket sessions: recent launch plus live check (`stepUpSSO`); people, jobs and work have no token step-up
- [x] T018 Credential change/reset, session revocation and unlink end launch sessions and unused tickets; SSO deadline cap
- [x] T019 Start-up warnings for module UI configuration

## Phase 3: docs

- [x] T014 `docs/sso.md`, `docs/module-wiring.md`, per-module UI docs, README note, compose note (stays on token login)

## Not done

- [V] Browser run: the interstitial and the meta-refresh hop on a real Chrome/Firefox/Safari (`form-action 'self'`, SameSite=Strict cookie on the module side after a cross-site navigation: the module's cookie is set by a same-site POST response, so it should hold; unverified).
- [V] Cluster run: a module UI routed through the ingress with TokenReview-authenticated `/v1/tickets/*`.
- [ ] The shipped charts do not route work, people, finance (or jobs beyond `/careers`) to a public host, so on a stock install the Apps page is empty until the operator sets `CAN_MODULE_UI_URLS`. A module installed before this change gets `web_login` in can-core's record only at its next upgrade or re-enable (the reconciler compares version and settings, not the manifest).
- [ ] Extract one tested session package for the four copies (research R16): a refactor with its own risk, better done on its own.
- [ ] Another host under the company domain can plant a nonce cookie (cookie tossing, research R7): the account name and the confirmation are then the only defence.
- [ ] Back-channel logout from can-core: modules learn at the next revalidation.
