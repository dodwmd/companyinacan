# Feature Specification: OpenID Connect single sign-on for the admin UI (Epic E11b)

**Feature Branch**: `epic/E11b-oidc-sso` | **Created**: 2026-11-25 | **Status**: built offline against an in-process fake provider; no real identity provider or browser verified
**Refines**: `SPEC.md` (SSO, section 4 identity and RBAC, `identity.provision`, section 3.7 shared platform services), `docs/epics/README.md` E11; builds on `specs/005-can-core-mvp` (sessions, CSRF, throttling), `specs/010-admin-ui`, `specs/015-approvals-notifications` (module-namespaced RBAC, owner rules, break-glass `can-core:cli`) and `specs/017-module-ui-wizard`.

## Summary

Staff sign in to the admin UI with the company's OpenID Connect provider. First sign-in creates a user
with no role; the provider's groups are mapped to roles (built-in and module-namespaced) on every
sign-in. The local password + TOTP login remains as the break-glass path. Staff reach modules with personal access tokens.

Out of scope (documented, not built): back-channel logout, `id_token_hint` logout, SCIM and
`identity.provision` (E13), refresh tokens, several providers per installation, editing `sso:` from the UI.

## User scenarios

### US1. Sign in with the company identity provider (P1)

- **Given** `sso.enabled` and a working provider, **then** the login page shows a sign-in button above the password form, the flow uses authorization code + PKCE S256 with `state` and `nonce`, and a successful callback creates a session with the same cookie flags as a password login.
- **Given** a first-time subject, **then** a user `sso-<issuer>-<subject hash>` is provisioned with no password, no TOTP secret and no role, and the audit log gets `sso.provision`.
- **Given** a provider failure of any kind (bad state, token, claims, keys unreachable), **then** no session is created and the browser sees one generic message; the audit log has the reason code.

### US2. Roles follow groups (P1)

- **Given** `group_roles` maps `ops` to `operator`, **then** a user in `ops` is an operator after sign-in; leaving the group removes the role at the next sign-in, with an `rbac.sso_sync` audit entry holding before and after.
- Roles an owner assigned by hand, scoped assignments, and the last owner are never removed by the sync.
- `owner` can only be mapped with `confirm_owner_mapping: true`; unknown roles are skipped and reported.
- **Given** a user removed from a group at the provider while signed in, **then** their role lives on until `revalidate_interval` (default 1 h) and not longer (documented lag).

### US3. No account takeover (P1)

- A user is found by (issuer, `sub`). Email never selects an account: an unverified email is refused (default), a verified lookalike becomes a new, role-less user.
- Linking an identity to an existing local account is an explicit, audited, owner-only action.

### US4. Break-glass stays (P1)

- Local login remains; `local_login: owner_only` restricts it to owners; `disabled` needs `confirm_disable_local_login`, a group mapped to `owner`, and is ignored while no SSO owner exists. `can-core admin` is unaffected.
- SSO users cannot sign in locally, and local sign-in with valid credentials is never throttled.

### US5. Operate it (P2)

- An admin page shows SSO status without secrets and a **Test discovery** button that fetches discovery and keys with the production restrictions; identities are listed with link/unlink.
- Logout is a CSRF-protected POST; with `rp_logout` it continues to the provider's end-session endpoint.

### US6. Modules (P2)

- SSO users use modules with personal access tokens (E03c); nothing SSO-specific reaches the modules.
- The admin UI session value is never accepted by `POST /v1/tokens/introspect` (it would let a module act as the user in the admin UI).
- Removing an SSO user revokes the personal access tokens they issued.

## Requirements

- **FR-001** Authorization code flow with PKCE S256, `state`, `nonce`; transaction in a signed, HttpOnly, Secure, SameSite=Lax cookie, 10 minutes, single use.
- **FR-002** ID token: RS256/ES256 only; verify signature, `iss` (exact), `aud`, `azp`, `exp`, `nbf`, `iat` (30 s skew, max age 10 min), `nonce`, `at_hash` when present; constant-time comparison of state and nonce.
- **FR-003** JWKS cached; unknown `kid` triggers at most one refetch per 30 s, single flight; fetch failure refuses the sign-in.
- **FR-004** Every outbound request: https (unless `allow_insecure_dev`), non-public addresses refused at connect time, no redirects, no proxy, 10 s timeout, size caps.
- **FR-005** The client secret is a Secret reference, never in `company.yaml`, never shown, never logged.
- **FR-006** Identity key (issuer hash, `sub`); never email. Link/unlink owner-only and audited in the same transaction as the change.
- **FR-007** Role sync as in US2; audit before and after; fail closed (a sync error refuses the sign-in).
- **FR-008** A new random session token per sign-in; the presented session is destroyed; SSO sessions expire at `revalidate_interval`.
- **FR-009** CSRF and same-site checks on every state-changing route (existing `authed`); `next` only local paths.
- **FR-010** No personal data (email, name, subject) in logs or audit entries; the audit log carries reason codes, hashes and role names.
- **FR-011** `company.yaml` `sso:` has a JSON Schema, semantic validation (`can validate`) and fixtures; secrets in values are rejected as unknown fields.
- **FR-012** Contracts: admin OpenAPI, `ui-routes.md`; contract tests green.

## Success criteria

- `make test` and `go test -race ./core/server/...` pass; the security tests in `core/server/sso` and `adminui` fail when each guard is removed (mutation run recorded in `tasks.md`).
- Binary grows by less than 0.5 MiB; idle can-core RSS with SSO enabled grows by about 3 MiB over main (measured, see `docs/sso.md`).

## Known gaps

See `docs/sso.md` section "Not verified" and the PR description.
