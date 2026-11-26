# Contract: routes added by E11c

Machine-checked by `test/contract/sso_routes_test.go`: every route below must be served, and the module
route tables in `specs/023`, `026`, `027`, `031` list the module rows. The OpenAPI files carry the module and
admin API operations.

## can-core admin UI (`core/server/adminui`, listed in `specs/010-admin-ui/contracts/ui-routes.md`)

| Route | Session | Notes |
|---|---|---|
| `GET /apps` | yes | Modules with a UI in which you hold a permission, each with an Open form. No ticket, no module address in the page. Names escaped. 503 without the service |
| `POST /apps/{id}/launch` | yes + CSRF | Optional `return` (path inside the app). 200 interstitial with a meta refresh and a link to `<module UI>/login/ticket?t=...`; 404 unknown/no UI/no permission (same page); 400 bad return path; 429 over 30 a minute; 403 CSRF |

## can-core admin API (`admin-api.yaml`)

| Route | Notes |
|---|---|
| `GET /api/v1/apps` | `{"items":[{"id","name"}]}` |
| `POST /api/v1/apps/{id}/launch` | `{"return_path"?}` -> `{"url"}`; session + CSRF; 404/400/429/503 as above |

## can-core module API (`module-api.yaml`)

| Route | Notes |
|---|---|
| `POST /v1/tickets/peek` | `{"ticket","binding"}` -> `{"active":true,"subject","name"}` or `{"active":false}`; does not consume |
| `POST /v1/tickets/redeem` | `{"ticket","binding"}` -> `{"active":true,"subject","name","permissions","return_path","session","session_expires_at"}` or `{"active":false}`. The audience is the authenticated caller |
| `POST /v1/tickets/revalidate` | `{"session"}` -> `{"active":true,"subject","name","permissions"}` or `{"active":false}`; only for the module that redeemed |

## Module web UIs (each module's own `contracts/ui-routes.md`)

| Module | Landing route | Home |
|---|---|---|
| work | `GET /ui/login/ticket`, `POST /ui/login/ticket` | `/ui/` |
| people | `GET /login/ticket`, `POST /login/ticket` | `/` |
| jobs | `GET /login/ticket`, `POST /login/ticket` | `/` |
| finance | `GET /login/ticket`, `POST /login/ticket` | `/` |
| files (`specs/050-files-ui`) | `GET /login/ticket`, `POST /login/ticket` | `/` |

Common behaviour: both need the launch nonce cookie (`__Secure-can_launch_<module>`; `can_launch_<module>`
only on a plain-http UI), set by can-core in the launching browser. `GET ?t=` peeks (never consumes) and shows a
Continue form naming the account; if the browser holds a live session of a different account, it says so and
requires a ticked confirmation. `POST` takes `csrf` (double-submit with the login cookie), `ticket` and, to
replace a different account's session, `replace=<new subject>` (409 without it, nothing redeemed); 303 to a clean
URL on success; 401 one generic page for every refusal (another browser included); 403 stale form or cross-site; 429 after repeated failures (valid tickets never throttled); 503 when can-core cannot
answer; 404 when the module is not installed under can-core. `Referrer-Policy: no-referrer`, `Cache-Control:
no-store`, CSP without scripts.
