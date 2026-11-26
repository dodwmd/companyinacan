# Feature specification: Users and roles in the admin UI (Epic E11e)

**Feature branch**: `epic/E11e-users-roles` | **Created**: 2026-11-25 | **Status**: built and tested offline against
the real can-core services and fakes (mail, identity provider); no browser, relay, provider or cluster run.

**Refines**: `SPEC.md` section 3.7 (shared platform services: RBAC); builds on `specs/015-approvals-notifications`
(RBAC, owner rules), `specs/021-oidc-sso`, `specs/028-identity-provision` (revoke and disable),
`specs/036-module-ui-sso` (launch tickets), `specs/016-module-wiring` (personal access tokens) and
`specs/039-email-send`.

## Problem

A business owner cannot manage people's access from the admin UI. Local accounts are made with
`can-core admin bootstrap` inside the pod; disabling is an API call; role overrides have no UI.

## What changes

1. **Users pages** (`/users`, `/users/{name}`, `rbac.manage`) and the matching admin API: add a local user,
   list, disable, enable, delete, assign and remove roles (showing what each grants, from module manifests),
   reset the authentication code or the whole sign-in, view and revoke sessions and personal access tokens,
   link and unlink single sign-on identities. `/access` gains the role-permission override form.
2. **One-time set-up link** (`/set-password`). A new account has no password, no code secret and no role. The
   link (256 random bits, stored hashed) opens a page with the authenticator secret and a form for a new
   password and a code from the app, so TOTP is mandatory from the first sign-in. Using it does not sign in.
   Invitations last 72 hours, resets 24 hours; one link per account; opening consumes nothing.
   It is mailed through `email.send` when a relay is configured and the recipient is allowed (company domain,
   `email.notification_recipients` or a verified SSO address); the administrator then never sees it.
   Otherwise it is shown once.
3. **Step-up.** Sensitive changes need a sign-in or a confirmation (`/reauth`, `POST /api/v1/reauth`) within
   10 minutes. Single sign-on users authenticate again at the provider (`prompt=login`, `max_age=0`; the
   returned `auth_time` is what counts).
4. **One authorization path.** `core/server/users` makes every decision; UI and API handlers only read input
   and render. The older paths (`/access`, `/api/v1/users/{name}/roles`, `/api/v1/identity/*`, SSO link and
   unlink, overrides) now go through it and gain the same rules.

## Behaviour changes to existing features

| Before | Now |
|---|---|
| A holder of `rbac.manage` could assign any role except `owner` | Only roles whose every permission they hold themselves (on that scope) |
| A non-owner could remove other roles from an owner | Only an owner changes an owner, including their tokens and sessions |
| Removing a role changed answers only | It also ends the person's admin sessions, module sessions, launch tickets and personal access tokens (also for the SSO group sync, role overrides that take a permission away, and the CLI) |
| An `owner` assignment counted toward "last owner" whatever the account | Only an enabled account that can sign in counts |
| Role, override, disable, enable and SSO link calls worked with any live session | They need a sign-in or step-up within 10 minutes (API: 403 "recent authentication required") |
| `can-core admin reset-password` on any account | Refused for SSO users; `--rotate-totp` required when the account has no code secret |

## Rules

- **Who.** Reading and changing need `rbac.manage`. Owners only: change anything about an owner, grant or remove
  `owner`, enable, override, link and unlink identities.
- **No escalation.** Grant: every permission of the role must be held. Set-up link for an existing account
  (a way to take it over): the maker must hold everything the account holds; a role of a disabled module is
  held only by holding that same role. The maker's authority is checked again when the link is used.
- **Self.** Nobody disables, deletes, resets or assigns roles to themselves here (own password, code and
  sessions are on `/settings`).
- **Last owner.** Disable, delete, role removal, reset, SSO unlink and the SSO group sync refuse to leave no
  owner who can sign in.
- **Delete** removes the account, roles, sessions, set-up link, delegations, notifications and their settings,
  push devices and the SSO identity; revokes tokens and tickets; keeps the audit log, approval history and
  token records. The name is never given to a new account (modules know people as `user:<name>`).
- **Audit** entries commit in the same transaction as the change; none holds a password, secret, token or
  email address.
- **Throttling.** Sign-in is unchanged: valid credentials are never throttled by anything here. Wrong step-up
  attempts are counted per session and end that session after five. Wrong current passwords on a
  code-reset link delete that link after five.

## Data

Migration `0014_users_roles.sql`: `admin_users.email`, `sessions.reauth_at`, `user_invites` (one row per
account, token hash, pending secret, purpose, maker, expiry), `deleted_users` (name, time).

## Threat model

Assets: accounts, roles, sessions, tokens, set-up links, the owner's control of the installation.

| Threat | Mitigation | Remaining risk |
|---|---|---|
| A delegated administrator raises their own or an accomplice's access | Grant subset rule in `rbac` (also under the API), no self-grant, overrides and identity links owner only | They can create accounts and give them what they themselves hold, which includes acting as a second person in approvals (two-person control assumes administrators are trusted for it). By default only owners hold `rbac.manage` |
| A delegated administrator takes over a stronger account by resetting it | Links need the maker to cover the target, owners only for owners, checked again at use; code reset keeps the password as a factor; delegations received are revoked | Authority that is not a role (an approval policy naming `user:<name>`, access recorded inside a module) is not seen by the check |
| The maker of a show-once link uses it themselves | Mail the link instead (then it is never shown); the setup is audited with the address it came from; the link dies if the account later holds more than the maker | Without a relay the administrator who creates an account can set it up. That is inherent in handing a link over |
| The link leaks (mail box, proxy log, history, screen) | Single use, short life, replaced by the next one, `Referrer-Policy: no-referrer`, `no-store`, can-core logs route patterns only | Whoever opens a live link controls the account. The token is in the query string: an ingress access log would record it. A queued or dead-lettered message holds it until sent, or up to 7 days (the link itself expires first or soon after) |
| A mail scanner or prefetcher spends the link | `GET` consumes nothing | None known |
| Guessing links or codes | 256-bit token; the code is checked before any password hashing; hashing shares the sign-in concurrency bound | The holder of a live link can cause hashing work until it is used |
| Forged requests | CSRF token and same-site check on every change; double-submit cookie on the public page; CSP with no scripts and `form-action 'self'`; `SameSite=Strict` session cookie | None known |
| A stolen or unattended session changes access | Step-up for sensitive changes; five wrong confirmations end the session | Within 10 minutes of a sign-in the session can act. Revoking one session or token needs no step-up, being protective |
| Account enumeration | Invited and reset accounts fail sign-in exactly like unknown ones; one answer for every unusable link; user pages need `rbac.manage` | Administrators who may manage users learn which names exist, by design |
| Locking the owner out | Last-owner rule on every path inside the transaction (two owners cannot reset each other away); no throttle on valid sign-in; `can-core admin` break-glass | An owner whose only credential is a lost device and who is the only owner needs the CLI |
| Access outlives a disable, deletion or role removal | Sessions, tickets, module sessions and tokens end in the same transaction | A module session ends at its next revalidation (at most 5 minutes, `specs/036`); module-issued service tokens are the module's own and are not revoked |
| A deleted person's data passes to a new person with the same name | Names of deleted accounts are refused, case-insensitively | Single sign-on usernames are derived from (issuer, subject): deleting an SSO user does not block their next sign-in. Disable them, or remove them at the provider |
| Change without a trace | Same-transaction audit; a failing audit rolls the change back | Issuing a token (specs/016, unchanged here) still audits after the insert |

## Not done

- With `sso.local_login: owner_only` or `disabled`, an invited non-owner can set up a password but cannot sign
  in with it; the pages do not warn about this.
- A can-core with no `company.yaml` file at all counts the SSO configuration as unreadable, so only owners can
  sign in with a password (existing `specs/021` behaviour, unchanged). The compose stack and `can up` provide
  the file; a hand-made deployment must too before invited non-owners can sign in.
- No self-service "forgot password"; an administrator makes a reset link.
- No CLI command to invite users (`can-core admin bootstrap` and `reset-password` remain).
- The list shows the first 500 accounts; no search or paging.
- The link address is `https://admin.<domain>`; without `CAN_COMPANY_DOMAIN` only the path is shown and nothing
  is mailed.
- Not checked with a real browser, screen reader, SMTP relay or identity provider (including whether a given
  provider honours `max_age=0`; if it does not, SSO administrators cannot step up and the change stays refused).

## Success criteria

- `make test` and `go test -race ./core/server/...` pass; route and OpenAPI contract tests cover every new route.
- Tests in `core/server/adminui/users_test.go` fail when a guard is removed (grant subset, owner guard,
  revocation, step-up, permission, link cover and maker checks, pending-account sign-in, last-owner counting,
  name tombstone, token owner guard, link attempt limit).
