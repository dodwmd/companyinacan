# Feature specification: Single sign-on from the admin UI into module web UIs (E11c)

**Epic**: E11c. **Depends on**: E11b (OIDC sign-in, `specs/021-oidc-sso`), E03c/E10 (module wiring,
`specs/016-module-wiring`), the module web UIs (`specs/023`, `026`, `027`, `031`), RBAC (`specs/015`).
**Status**: built and tested offline against fakes and the real can-core services; no browser or cluster run.

## Problem

Signing in to a module's web page (work, people, jobs, finance) means pasting a personal access token (PAT)
that the person first created in the admin UI. That is clumsy, the token is a long-lived bearer secret that
ends up in a password manager or a chat, and people who signed in to the admin UI with single sign-on (E11b)
have to do it all over again for every module.

## What changes

1. **Apps page.** The admin UI gets an *Apps* page listing the enabled modules that have a web UI and in
   which the signed-in person holds at least one permission. *Open* is a POST (CSRF-protected).
2. **Launch ticket.** The POST mints a random 256-bit ticket, bound to that one module, valid for at most 60
   seconds, redeemable once, stored only as a hash, and sends the browser to the module's own sign-in
   address, which can-core builds from configuration (never from the request).
3. **Module side.** `GET <module UI>/login/ticket?t=...` only shows a *Continue* form (a link scanner or a
   browser prefetch must not spend the ticket). The form POST redeems the ticket at can-core over the module
   API (the caller's projected token names the audience) and the module starts a **session of its own**, with
   the permissions can-core computed from RBAC at that moment. The browser gets nothing but the module's own
   session cookie; the module gets no long-lived credential, only an opaque session handle that can-core
   answers "is this person still allowed, and with what?" for.
4. **Revalidation.** The module asks again on the intervals it already used for PAT sessions (1 minute for
   privileged sessions, 5 minutes for others; work 30 seconds, its PAT cache lifetime). A disabled account, a
   removed role, an unlink, a revocation or an expired handle ends the module session at the next check.
5. **PAT login stays**, behind `ui.token_login` (default `true`, backwards compatible). Documentation
   recommends `false` once SSO is in use.

## Out of scope / non-goals

- Back-channel logout from can-core into modules (revalidation covers it within one interval).
- Logging out of the admin UI does not end module sessions (they end at the next revalidation only if the
  account is gone; logging out of the module ends the module session only).
- A module that is not in this repository (third-party modules with a UI) can adopt the same two module-API
  calls; nothing here is specific to in-house modules.
- Hosting: today no in-house module is routed to a public host by its chart (work, people, finance: none;
  jobs: only `/careers`). The launcher lists a module only when its UI address is configured
  (`runtime.ui` plus `runtime.subdomain` in the manifest, or the operator's `CAN_MODULE_UI_URLS`), so on a stock
  install the Apps page is empty. Exposing the module UIs through the ingress is a separate decision (see
  `docs/sso.md`).

## User stories

**US1 (P1)**: As a person signed in to the admin UI with SSO, I open *Apps*, press *Open* next to a module and
land in it, already signed in as me, without ever seeing a token.

- A module I hold no permission in, a disabled module, a module without a UI, and a module whose address is
  not acceptable are not listed, and launching them is refused (404, the same page).
- The module shows what my roles allow, no more (a person with only `work.read` cannot write).

**US2 (P1)**: As an operator, a stolen, replayed, prefetched or misdirected ticket gives an attacker nothing.

- Second redeem fails; another module's redeem fails (and does not spend the ticket); an expired ticket fails;
  a GET never spends it; every failure is the same `{"active": false}`; failed redeems are slowed per module
  and a valid redeem never is.

**US3 (P1)**: As an operator, when someone leaves, their module sessions end within one revalidation interval,
without me touching each module.

**US4 (P2)**: As an operator I can switch the paste-a-token login off per module (`ui.token_login: false`) and
keep tokens for scripts only.

## Requirements

- FR-1 Minting needs an authenticated admin session, a CSRF token, an enabled non-disabled account, an
  enabled module with a UI and at least one held permission of that module; 30 mints per person per minute.
- FR-2 A ticket: 256 random bits (`cant_` + 43 base64url characters), SHA-256 at rest, `expires_at <= created
  + 60 s`, one atomic `UPDATE ... WHERE used_at=0 AND expires_at>now AND module_id=caller` to redeem.
- FR-3 The landing path is stored with the ticket, validated at mint (`^/[A-Za-z0-9/_-]{0,100}$`, no `//`,
  relative to the module's UI base) and again by the module; anything else lands on the home page.
- FR-4 Redeem returns subject, a display name (no email, no groups), the module permissions the subject holds
  now, the landing path, and a session handle (`cans_` + 43 characters, hashed at rest, at most 12 hours).
- FR-5 Revalidate answers only to the module that redeemed, recomputes permissions, and is inactive for a
  disabled or removed account, a revoked or expired handle, or no remaining permission.
- FR-6 Mint and redeem are audited with ids only (never a ticket or handle). Failed redeems are counted and
  slowed, not audited one by one.
- FR-7 The module session cookie is the module's own random id; the session fixation rules of the existing
  login apply (a presented id is never adopted).
- FR-8 The admin UI session value is never a module credential and is not accepted anywhere in this flow.

## Success criteria

- `go test -race ./core/...` and the contract tests pass; the new route tables match the contract files.
- No new third-party dependency, no JavaScript, no new listener.
