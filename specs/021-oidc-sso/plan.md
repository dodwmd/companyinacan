# Implementation Plan: OpenID Connect single sign-on (Epic E11b)

**Branch**: `epic/E11b-oidc-sso` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

A new package `core/server/sso` (flow, provider, JWS/JWKS, SSRF-guarded client, service) with no new
dependency; `core/server/auth` learns session sources, revalidation and a local-login policy hook;
`core/server/rbac` gets one transactional sync method; `core/server/store` gets migration 0005;
`adminui`, `adminapi` and `moduleapi` get the routes. Choices: [research.md](research.md); entities:
[data-model.md](data-model.md); contracts: [contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27. **New dependencies**: none. `go-oidc/v3` (+ `go-jose`) and `x/oauth2` were
considered and rejected (research R1).

**Storage**: SQLite migration `0005_oidc_sso.sql` (two columns on `sessions`, one on `admin_users`, table `sso_identities`).

**Testing**: `go test -race`. A fake provider (`core/server/sso/ssotest`, `httptest`) implements discovery,
JWKS (RSA and EC), authorization with PKCE, token (single-use codes, PKCE check, client authentication) and
userinfo, with switches to attack the client. `sso` (flow, claims, algorithms, keys, policy, sync, linking,
introspection, SSRF, size caps), `adminui` (HTTP flow, cookies, interstitial, open redirect, fixation,
logout CSRF, pages), `adminapi`, `moduleapi`, `rbac`, `config`, fixtures under `testdata/`.

**Constraints**: no real IdP, no browser, no cluster, no installs.

## Constitution Check

| Principle | Before research | After design |
|---|---|---|
| I. Spec-first | SPEC.md names SSO as Phase 2 with roles as SSO groups; this spec, contracts and `docs/sso.md` land with the code. SPEC.md names Authelia as the default IdP module (forward-auth); can-core's own OIDC client is independent of which IdP runs | Same. No SPEC.md contradiction; `identity.provision`/SCIM stays E13 |
| II. Footprint | Stdlib only; client code is ~1 kLOC. Measured against current main: binary +221 KB (19.14 to 19.36 MB, stripped), idle RSS 47.7 MiB (main) to 48.1 MiB (SSO off) and 50.6 MiB with SSO enabled after 20 sign-ins (can-core budget 48 request / 96 limit) | Provider state (discovery, a handful of keys, a used-state set capped at 4096 entries) is a few KiB. Concurrent exchanges capped at 4 |
| III. Test-first | The fake provider and the security tests were written with the implementation, not before it (honest note). After the fact, 13 guards were removed one at a time and the suite had to fail for each; two survived and tests were strengthened until they failed too | Config, rbac, moduleapi tests likewise written beside the code. Route-contract and message-catalogue tests ran red before the documentation and messages |
| IV. Stable module contracts | Introspection gains optional response fields (additive); no capability or event changes | Same |
| V. Secure and private | Fail closed everywhere, no email match, SSRF guard, no PII in logs or audit, secret by reference only, CSRF on all POSTs | Tested (see spec FR list) |
| VI. Open-source hygiene | Apache-2.0, no third-party code added | Same |
| VII. Simplicity and measured claims | One flow, two algorithms, one provider; size and RSS measured, nothing else claimed; "not verified" list | Same |

**Result**: pass. One honest deviation (III) recorded above.

## Project structure

```text
core/config/sso.go                  type, defaults, semantic validation, SSRF helpers (shared with can validate)
core/server/sso/                    flow.go provider.go jws.go jwks.go httpc.go service.go admin.go ssotest/
core/server/auth/                   session source + revalidation, CreateSSOSession, LocalPolicy, no password for SSO users
core/server/rbac/                   SyncSSORoles (granted_by = sso)
core/server/store/                  migrations/0005_oidc_sso.sql, sso.go
core/server/adminui/                sso.go, templates/{sso,interstitial}.html, login and settings changes, messages_sso.go
core/server/adminapi/               sso.go
core/server/moduleapi/              introspection of session tokens
core/server/runtime/                wiring, CAN_SSO_SECRET_DIR
deploy/can-core/                    optional Secret volume sso-client
schemas/company.v1.schema.json      sso block
docs/sso.md, docs/admin-ui.md, docs/approvals.md
```
