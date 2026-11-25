---

description: "Task list for Epic E11b: OpenID Connect single sign-on"
---

# Tasks: OpenID Connect single sign-on (Epic E11b)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. `[V]` marks work that needs a real identity provider, browser or cluster and is **not** done.

## Phase 1: Configuration

- [x] T001 `core/config/sso.go`: type, defaults, `ValidateSSO`, SSRF helpers (`config_test`: private ranges, URL rules, every rule)
- [x] T002 Schema `sso` block, `can validate` hook, fixtures (1 valid, 7 invalid; `test/contract` green)
- [x] T003 `config.ValidSecretRef` shared with the module settings validator

## Phase 2: Store, auth, RBAC

- [x] T004 Migration 0004; `store/sso.go` (identities, JIT user, link, unlink, owner count)
- [x] T005 `auth`: session source and revalidation, `CreateSSOSession`, SSO users cannot use the password path, `LocalPolicy`
- [x] T006 `rbac.SyncSSORoles` (own assignments only, unknown roles skipped, owner confirmation, last owner kept, audit before/after) and tests

## Phase 3: `core/server/sso`

- [x] T007 JWS (RS256, ES256), JWKS parsing limits, single-flight rate-limited key set
- [x] T008 SSRF-guarded client (connect-time check, no redirects, no proxy, caps, timeouts)
- [x] T009 Provider: discovery validation, auth URL (PKCE S256), code exchange, ID token validation, userinfo fill-in
- [x] T010 Signed single-use transaction cookie, `SafeNext`
- [x] T011 Service: begin/complete, policy, provisioning, role sync, session, audit (fail closed), logout URL, local policy, status, test discovery, link/unlink, introspection
- [x] T012 `ssotest` fake provider and the security test suite (replay, state, nonce, aud/iss/azp, none, HS256 confusion, crit, foreign key, expiry/nbf/iat, rotation, JWKS failure, unverified email takeover, group removal, fixation, throttling, SSRF, redirects, size caps, PII in logs)

## Phase 4: Surfaces

- [x] T013 Admin UI: routes, login button, interstitial, settings note, SSO page, link/unlink, messages; `ui-routes.md`; handler tests (flow, open redirect, fixation, logout CSRF, pages)
- [x] T014 Admin API `/api/v1/sso*`, session `source`; OpenAPI; contract tests
- [x] T015 Module API introspection of session tokens; OpenAPI; tests
- [x] T016 Runtime wiring, `CAN_SSO_SECRET_DIR`, optional Secret volume in `deploy/can-core`

## Phase 5: Docs and verification

- [x] T017 `docs/sso.md`, `docs/admin-ui.md`, `docs/approvals.md`, epics table
- [x] T018 Mutation run: each guard below removed in turn, `go test ./core/server/sso ./core/server/auth` had to fail. Result: all failed once two tests were strengthened (alg allowlist: a refusal must happen before any key lookup; redirects: the target must not be contacted). Guards: nonce, aud, iss, exp, alg allowlist, replay set, email_verified, session fixation, JWKS refetch limit, revalidation, cookie signature, connect-time address check, no-redirect policy
- [x] T019 Footprint: binary 19,001,607 to 19,255,559 bytes (+254 KB, `-trimpath -s -w`); idle RSS after 20 requests 46.7 to 47.6 MiB (SSO off, 64 MiB soft limit)
- [ ] T020 [V] Real provider (Keycloak, Authentik, Google, Entra): claim quirks, trailing-slash issuers, groups size limits, missing `email_verified`
- [ ] T021 [V] Browser: SameSite handling and the interstitial, provider logout, cookies across the redirect
- [ ] T022 [V] Cluster: mounted Secret `sso-client`, ConfigMap refresh within 30 s, RSS with SSO enabled
- [ ] T023 Back-channel logout (`logout_token` validation, `sid` to session map) and `id_token_hint`
- [ ] T024 `identity.provision` / SCIM (E13)
