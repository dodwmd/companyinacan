# Contract: single sign-on routes (E11b)

The normative route table is `specs/010-admin-ui/contracts/ui-routes.md` (extended by this epic; the test
`TestRoutesMatchContract` compares it with the served routes). The admin JSON API is in
`specs/005-can-core-mvp/contracts/admin-api.yaml` (`/api/v1/sso*`), the module API in `module-api.yaml`
(unchanged by this epic); `test/contract` compares both with the handlers.

## Flow

```text
GET /login (button) -> GET /auth/oidc/login[?next=/path]
   sets can_oidc_<flow id> (signed, HttpOnly, Secure, Lax, 10 min, path /auth/oidc; at most 4 per browser) -> 303 IdP /authorize
IdP -> GET /auth/oidc/callback?code&state
   clears that cookie; verifies; sets can_session (Strict); 200 interstitial -> meta refresh -> next
POST /logout (csrf) -> 303 /login            (rp_logout off)
                    -> 200 interstitial -> IdP end_session_endpoint?client_id&post_logout_redirect_uri   (rp_logout on)
```

## Status codes

| Route | Code | When |
|---|---|---|
| `GET /auth/oidc/login` | 303 | to the provider |
| | 404 | SSO not enabled |
| | 503 | configuration invalid, secret unreadable, provider unreachable (login page with a generic message) |
| `GET /auth/oidc/callback` | 200 | signed in (interstitial) |
| | 403 | any refusal; one generic message; reason code in the audit log |
| | 404 | SSO not enabled |
| | 429 | flood of callbacks without a valid transaction (hard-lock mode), `Retry-After` |
| | 503 | busy (4 concurrent exchanges), configuration or provider failure |
| `GET /settings/sso` | 200 / 403 | `rbac.manage` |
| `POST /settings/sso/test` | 200 / 403 / 409 / 503 | 409 when SSO is not configured |
| `POST /settings/sso/identities/{id}/link` | 303 / 403 / 409 / 404 | 403 non-owner; 409 unknown user, user not local, user already has an identity |
| `POST /settings/sso/identities/{id}/unlink` | 303 / 403 / 409 / 404 | 409 would remove the last owner |

## Fields

| Route | Fields |
|---|---|
| `GET /auth/oidc/login` | query `next`: a local path (`/...`, no `//`, no backslash, no control characters, at most 200 characters, not `/login` or `/auth/oidc*`); anything else becomes `/` |
| `GET /auth/oidc/callback` | query `code`, `state`, or `error` (its description is never shown) |
| `POST /settings/sso/identities/{id}/link` | `csrf`, `username` |
| other POSTs | `csrf` |

## Order of checks for the POSTs

Session (303 `/login`), CSRF and same-site (403), permission `rbac.manage` (403), owner (403), handler.
