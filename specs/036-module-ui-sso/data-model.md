# Data model: single sign-on into module UIs (E11c)

Migration `core/server/store/migrations/0009_login_tickets.sql`. Only hashes of secrets are stored.

## login_tickets

| Column | Meaning |
|---|---|
| `id` | `tkt_` + 16 hex characters. Safe to log and audit; it is not the ticket |
| `token_hash` | SHA-256 (hex) of the ticket (`cant_` + 43 base64url characters); unique |
| `subject` | `user:<username>` of the person who minted it |
| `module_id` | the audience: the one module that may redeem it |
| `return_path` | landing path inside the module's UI, relative to its base; `''` for the home page; validated at mint |
| `created_at`, `expires_at` | Unix seconds; `expires_at = created_at + 60` at most (`ticket.MaxTicketTTL`) |
| `used_at` | 0 while unused; set by the atomic redeem |

Rows older than an hour past expiry are deleted by the hourly housekeeping (`PurgeLoginTickets`).

## ui_launch_sessions

The server-side handle of a module session that began with a ticket.

| Column | Meaning |
|---|---|
| `handle_hash` | SHA-256 (hex) of the handle (`cans_` + 43 base64url characters); primary key |
| `ticket_id` | the ticket it came from (an id, not the ticket) |
| `subject`, `module_id` | the person and the only module that may revalidate it |
| `created_at`, `expires_at` | Unix seconds; at most 12 hours (`ticket.SessionLifetime`) |
| `revoked_at` | 0 while valid; set when the account's access is ended (`identity.end`, SSO unlink via `revokeSubjectTokens`) |

Revalidation does not depend on `revoked_at` alone: it re-reads the account (must exist and not be disabled)
and RBAC (at least one module permission) on every call.

## Manifest (`module.v1`)

`runtime.ui` gains an optional `base_path` (`^(/[a-z0-9-]{1,32}){0,2}$`, default none: the UI is at the root
of the host; work's UI is under `/ui`). `runtime.ui` still requires `runtime.subdomain`. `config.Module`
carries `Runtime.UI.{Service, Port, BasePath}`.

## Configuration

| Setting | Where | Meaning |
|---|---|---|
| `CAN_MODULE_UI_URLS` | can-core env | `id=base URL,...`: the operator's address for a module's UI; beats the manifest; validated like the manifest address |
| `CAN_COMPANY_DOMAIN` | can-core env (existing) | module UI hosts must be this domain or below |
| `CAN_COOKIE_SECURE=false` | can-core env (existing) | development: http and localhost addresses are accepted |
| `ui.token_login` | each module's `ui:` block | default `true`; `false` removes the paste-a-token form and refuses `POST /login` |

## In-memory state

- can-core: per-person mint counters (30/min) and per-module failed-redeem counters (bounded maps).
- module: per session, the can-core session handle, display name, permissions and the time of the last
  check. Never written to disk, never sent to the browser.
