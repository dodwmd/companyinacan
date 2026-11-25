# Implementation plan: single sign-on into module UIs (E11c)

Spec: [spec.md](spec.md). Research: [research.md](research.md). Data model: [data-model.md](data-model.md).
Contracts: [contracts/ui-routes.md](contracts/ui-routes.md) and the OpenAPI files
`specs/005-can-core-mvp/contracts/module-api.yaml` (`/v1/tickets/redeem`, `/v1/tickets/revalidate`) and
`admin-api.yaml` (`/api/v1/apps`, `/api/v1/apps/{id}/launch`). Tasks: [tasks.md](tasks.md).

## Shape

- `core/server/ticket` (new, stdlib + the existing store/audit/pat packages): `Service.{Apps, Launch, Redeem,
  Revalidate}`, `Launcher` (module UI address rules), the rate limiters. One place for the rules; the admin UI,
  the admin API and the module API are thin callers.
- `core/server/store`: migration 0009, `tickets.go` (atomic redeem, session handles, purge); `revokeSubjectTokens`
  and `identity.end` also revoke launch sessions.
- `core/server/adminui`: `/apps` and `/apps/{id}/launch` (`apps.go`, `templates/apps.html`, `messages_apps.go`).
- `core/server/adminapi`: `GET /api/v1/apps`, `POST /api/v1/apps/{id}/launch`.
- `core/server/moduleapi`: `POST /v1/tickets/redeem`, `POST /v1/tickets/revalidate`.
- `core/server/runtime`: builds the service (`CAN_COMPANY_DOMAIN`, `CAN_MODULE_UI_URLS`, `CAN_COOKIE_SECURE`),
  hourly purge.
- `core/config` + `schemas/module.v1.schema.json`: `runtime.web_login {base_path, via_subdomain}` (typed `Runtime.WebLogin`), declared by work, people, jobs and finance.
- `core/modclient`: `RedeemTicket`, `RevalidateSession`, `SessionAPI`, `TicketShaped`; `fakecore` learns tickets.
- `core/{work,people,jobs,finance}`: `GET/POST <ui>/login/ticket`, an sso session variant (handle instead of
  token), revalidation on the existing interval, `ui.token_login`. Each module keeps its own session store
  (research R16).

## Constitution check

| Principle | Check |
|---|---|
| I Small and honest | No dependency, no JavaScript, no new listener, no signing key; two small tables and one package. The module UIs are not yet reachable from outside on a stock install and the docs say so. |
| II Open and replaceable | Plain HTTP+JSON module API calls any module (in-house or third party) can make; Apache-2.0; no lock to a particular IdP. |
| III Test first | Tests written before code and seen to fail for each layer (`undefined: Service`, `unknown field Tickets`, unknown route, `UI.TokenLogin undefined`, ...); security tests listed in `tasks.md`. |
| IV Secure by default | Short-lived single-use audience-bound tickets, hashes at rest, GET never consumes, CSRF on mint and redeem, strict return-path allowlist, address from configuration only, per-person and per-module limits, constant answers, audit with ids only, `Referrer-Policy: no-referrer`, fail closed on any doubt. `ui.token_login` stays true by default only for compatibility. |
| V Sensitive data in-house | The module learns a subject, a display name and permissions, never an email or groups; nothing leaves the cluster. |
| VI Honest status | Built and tested offline; not browser- or cluster-verified; module UIs are not routed publicly by the shipped charts. |

## Design decisions in brief

1. The ticket is opaque and means only "ask can-core"; the module session is the module's own.
2. Redeem is one atomic UPDATE keyed by hash, module and freshness; failures are indistinguishable.
3. Permissions are computed live at redeem and at every revalidation with the PAT rule (`pat.HeldBy`, which
   knows `work.admin` and `finance.*` imply their weaker permissions).
4. The module maps permissions with the function its PAT authenticator already uses (restricted-token actor).
5. Not a redirect from the admin POST: an interstitial (research R10).
6. Overrides (`CAN_MODULE_UI_URLS`) are operator configuration and validated exactly like manifest addresses.

## Testing

`go test -race ./core/...` including: `core/server/ticket` (round trip, replay, 8-way concurrent redeem,
audience, expiry, shape, return path table, mint refusals, disabled user, role removal, per-user rate limit,
hash-at-rest and audit-without-secret, failure slowdown vs valid never slowed, identical inactive answers,
revalidate rules, launcher address table), `moduleapi` over HTTP (byte-identical inactive answers, session
cookie value and PAT never tickets, introspection does not recognise or consume a ticket), `adminapi` and
`adminui` (CSRF, refusal as 404, escaping, no secret on the launcher page, interstitial headers, rate limit),
`modclient` (junk never sent, outage is an error), and each module's `ui_sso_test.go` (GET does not consume,
CSRF double-submit, cross-site, replay and foreign tickets, fixation, landing allowlist, permissions follow
can-core, outage fails closed, `token_login` off, no ticket in logs). Contract tests: both OpenAPI files, the
admin UI route table and the four module route tables.
