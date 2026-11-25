# Data model: single sign-on into module UIs (E11c)

Migrations `core/server/store/migrations/0009_login_tickets.sql` and `0010_launch_auth_time.sql`. Only hashes of
secrets are stored.

## login_tickets

| Column | Meaning |
|---|---|
| `id` | `tkt_` + 16 hex characters. Safe to log and audit; it is not the ticket |
| `token_hash` | SHA-256 (hex) of the ticket (`cant_` + 43 base64url characters); unique |
| `subject` | `user:<username>` of the person who minted it |
| `module_id` | the audience: the one module that may redeem it |
| `return_path` | landing path inside the module's UI, relative to its base; `''` for the home page; validated at mint |
| `binding_hash` | SHA-256 (hex) of the launching browser's nonce (`canb_` + 43 characters; the cookie `__Secure-can_launch_<module>`); peek and redeem require it |
| `session_not_after` | Unix seconds the module session may not outlive (the launching SSO session's IdP revalidation deadline); 0 = none |
| `created_at`, `expires_at` | Unix seconds; `expires_at = created_at + 60` at most (`ticket.MaxTicketTTL`) |
| `used_at` | 0 while unused; set by the atomic redeem |
| `auth_at` | Unix seconds the launching person last authenticated to the admin UI; 0 = unknown |
| `revoked_at` | 0 while valid; set on every ticket of a subject (consumed ones too) by `endLaunchAccess`; no module session is stored for a revoked ticket |

Rows older than an hour past expiry are deleted by the hourly housekeeping (`PurgeLoginTickets`).

## ui_launch_sessions

The server-side handle of a module session that began with a ticket.

| Column | Meaning |
|---|---|
| `handle_hash` | SHA-256 (hex) of the handle (`cans_` + 43 base64url characters); primary key |
| `ticket_id` | the ticket it came from (an id, not the ticket) |
| `subject`, `module_id` | the person and the only module that may revalidate it |
| `created_at`, `expires_at` | Unix seconds; at most 12 hours (`ticket.SessionLifetime`) and never past `session_not_after` |
| `auth_at` | copied from the ticket; returned by redeem and revalidate as `authenticated_at` |
| `revoked_at` | 0 while valid; set (with the person's tickets revoked) by `endLaunchAccess` in the transaction of a password/TOTP change or CLI reset, an admin session revocation, SSO unlink or link replacement, and `identity.end` |

Revalidation does not depend on `revoked_at` alone: it re-reads the account (must exist and not be disabled)
and RBAC (at least one module permission) on every call.

`sessions.auth_at` (admin sessions) is the identity provider's `auth_time` for an SSO session, 0 when it did not
say; a local session authenticated when it was created.

## Manifest (`module.v1`)

New optional `runtime.web_login: {base_path, via_subdomain}` (`config.Module.Runtime.WebLogin`): the module's
web UI accepts launch tickets at `<base>/login/ticket`. `base_path` matches `^(/[a-z0-9-]{1,32}){0,2}$` (work:
`/ui`). `via_subdomain: true` means `https://<runtime.subdomain>.<domain>` serves that UI; otherwise only the
operator's address makes the module launchable. work, people, jobs and finance declare it without
`via_subdomain`.

`runtime.ui` is deliberately **not** used: it describes the routed web app of an upstream module (wiki, CRM,
...), which does not understand tickets. Offering those would send tickets to programs that never asked for them.

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
