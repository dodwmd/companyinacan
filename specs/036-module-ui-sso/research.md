# Research: single sign-on into module UIs (E11c)

## R1. Launch ticket vs. handing the module the admin session or a PAT

Rejected: giving the module the admin UI session cookie value (QA lesson of E11b: module introspection of the
`can_session` value was removed because a module could replay it as a cookie; a value given to one module
would work at all); minting a PAT per click (a long-lived bearer in the browser or in module memory, quota
`MaxLivePerIssuer`, lives 1 hour at least); a signed JWT ticket (needs a verification key at every module and
cannot be single-use without state). Chosen: an opaque, short-lived, single-use, audience-bound ticket whose
only meaning is "ask can-core". Everything is server side in can-core's SQLite (already the system of record
for tokens and sessions); no new store, no signing key.

## R2. GET does not consume (prefetch safety)

A ticket in a query string reaches the module as a GET navigation. Email and chat link scanners, browser
prefetch and "open in new tab" previews all issue GETs. If a GET redeemed the ticket, a scanner would spend
it. So GET renders an interstitial with a **POST form** (no JavaScript, no meta refresh: either would let a
scanner that renders pages spend it), and only the POST redeems. The ticket travels to the POST in a hidden
field. Cost: one extra click. The interstitial names the account through a non-consuming `peek` that needs
the launching browser's nonce (R7), so it tells a bare ticket holder nothing.

## R3. Where the ticket is stored and how it is redeemed

`login_tickets(id, token_hash UNIQUE, subject, module_id, return_path, binding_hash, session_not_after, created_at, expires_at, used_at)` and
`ui_launch_sessions(handle_hash, ticket_id, subject, module_id, created_at, expires_at, revoked_at)`
(migration 0009). Redeem is one statement,
`UPDATE login_tickets SET used_at=? WHERE token_hash=? AND module_id=? AND binding_hash=? AND used_at=0 AND expires_at>? RETURNING ...`,
so two concurrent redeems have exactly one winner (tested with 8 goroutines under `-race`). The lookup is by the
hash of the secret, so there is no comparison on the secret to time. Unknown, used, expired and foreign tickets
are the same row-less result.

## R4. Why the ticket does not burn when another module presents it

`module_id` is in the `WHERE`. A hostile or confused module presenting a ticket minted for another module gets
`{"active": false}` and the audience can still redeem. The alternative (burn on any presentation) lets any
module deny service to a person by guessing nothing at all but replaying a ticket it was shown; there is no
way for a module to see another module's ticket except through the browser, so the denial would be real.

## R5. Module identity and audience

The audience is the authenticated caller of `POST /v1/tickets/redeem` (TokenReview of its projected
ServiceAccount token, the same `authed` chain as every module API call). It is never a field of the request
body, so a module cannot claim to be another one.

## R6. Open redirect

Two places carry a URL: the address the browser is sent to, and the landing path inside the module.
- Only modules declaring `runtime.web_login` are launchable (an upstream app with `runtime.ui` is not: it does
  not expect tickets). The address is built by can-core from configuration only: the operator's
  `CAN_MODULE_UI_URLS`, or (with `web_login.via_subdomain`) `runtime.subdomain` + company domain +
  `web_login.base_path`. `Launcher.validate` requires https
  (http only in plain-HTTP development, `CAN_COOKIE_SECURE=false`), a host equal to or under the company domain
  (or localhost in development), no userinfo, query or fragment, a path of at most two lowercase segments,
  a valid port, and refuses the admin host (a module in the admin UI's origin could read its cookie scope).
- The landing path is relative to the module's UI base and must match `^/[A-Za-z0-9/_-]{0,100}$` without `//`:
  no dots (so no `..`), no `?`, `#`, `:` or backslash, no scheme or host. It is checked at mint and again in
  the module, which redirects only to its own `/ui/...` or `/...` paths through its existing `safeNext`-style
  filter.

## R7. Login CSRF and session fixation

Login CSRF: an attacker launches **their own** account and sends the link to a victim, who ends up signed in as
the attacker and types data into the attacker's account. The module's double-submit cookie does **not** stop
this (an earlier version of this document said it did; QA showed it was false): the interstitial GET sets that
cookie in whatever browser opens the link. It only stops a forged cross-site POST.

What stops it is binding the ticket to the browser that launched. At launch can-core also makes a random nonce
(`canb_` + 43 characters), stores its hash with the ticket and sets it in the launching browser as a cookie:
`__Secure-can_launch_<module>`, `HttpOnly`, `Secure`, `SameSite=Strict`, 60 seconds, path
`<UI base>/login/ticket`, `Domain=<company domain>` so the module's host receives it (admin and module are
same-site, so Strict is sent on the navigation). The module forwards it with the ticket; `peek` and `redeem`
require it (`binding_hash` in the `WHERE`), fail closed when it is missing or different, and do not spend the
ticket. A link opened in any other browser shows only "not accepted". Tested with two browsers in all four
modules.

On top of that the interstitial names the account (a non-consuming `peek`, which needs the nonce, so a ticket
alone is no oracle), and a browser that already holds a session of a **different** account is told so and must
tick a confirmation (409 and nothing redeemed without it).

Cookie scope cases, decided at launch (`BindingCookie`):
- admin UI and module on the same host (plain-http development on `localhost`; ports do not separate cookies):
  host-only cookie, named `can_launch_<module>` without the `__Secure-` prefix when the module is on http;
- different hosts, both under a company domain with a dot: `Domain=<company domain>`;
- otherwise (admin UI reached by IP or port-forward, `Domain=localhost`): refused with a page that says to use
  the admin UI's configured address. A module whose UI is secure never reads the unprefixed name.

Residual risk: a `Domain` cookie can also be set by **any other host under the company domain** (cookie
tossing). Someone who controls such a host (a compromised or hostile upstream app on a subdomain) can plant
their own nonce in a victim's browser and send their own link; the account name on the interstitial and the
confirmation are then the only defence. The same host receives the nonce on requests to that exact path, but
the nonce is useless without the ticket. Host subdomains you do not trust on a different domain.

Session fixation: a session cookie sent with the login is never adopted; a new random id is issued and the old
session is deleted.

With `Referrer-Policy: no-referrer` a browser posts a form with `Origin: null` (Fetch standard). The work UI
refused every `null` origin, which would have refused its own forms in a real browser; it now accepts `null`
only when the browser-controlled `Sec-Fetch-Site` is `same-origin`. The other module UIs check
`Sec-Fetch-Site` only. This rests on the standard, not on a browser run.

## R8. XSS in the launcher

Module names come from manifests (installable from the catalogue). `html/template` escapes them; a test with
`<script>` in a name asserts the escaped form. The page has no script and CSP `script-src 'none'`. The
launcher page never contains a ticket or a module address; the address appears only in the POST response.

## R9. Leakage: Referer, logs, history

The ticket is in the URL for exactly one navigation. Mitigations: `Referrer-Policy: no-referrer` and
`Cache-Control: no-store` on the admin interstitial and on every module UI page; no third-party resources;
access logs record the route pattern (`GET /login/ticket`), never the path or query (tested: no `cant_` in
logs); after the POST the module redirects (303) to a clean URL, so the ticket is not left in the address
bar or history of the page the person keeps using (the interstitial entry remains but the ticket is spent
after one use or 60 seconds). The ticket is never put in a cookie, header or audit entry.

## R10. The admin interstitial and `form-action 'self'`

The admin UI CSP has `form-action 'self'`. Chrome applies it to redirects that follow a form submission, so a
POST answered with a 303 to the module's origin would be blocked. As with the OIDC callback (E11b), the
POST answers 200 with an interstitial that moves the browser on by a meta refresh and a link (a navigation
started by the page, not a form redirect).

## R11. Cross-module ticket reuse and replay

Covered by R3/R4. A ticket works for the module it names, once. The session handle likewise belongs to one
module (`ui_launch_sessions.module_id`); revalidation by another module is inactive.

## R12. Clock skew

All expiry decisions use can-core's own clock (`ticket.Service.Now` / the store clock) against timestamps
can-core wrote. A module's clock is never consulted for a ticket or a handle; the module compares its own
clock only with its own `checked` timestamps (monotonic enough: a skewed module clock changes how often it
asks, never what it is told). 60 seconds is long enough for a redirect and a click on a slow link and short
enough that a ticket found in a log is nearly always dead; it is not configurable upwards (`MaxTicketTTL`).

## R13. Rate limits

Mint: 30 per person per minute (in memory, bounded to 10,000 tracked subjects), 429 beyond it. Redeem: invalid
redeems per module past 60 per minute wait 200 ms each (the pattern of `pat.penalise`); a valid redeem is never
delayed, so a flood of junk cannot lock anyone out. The module's login handler additionally counts its own
failed ticket logins per client address with its existing failed-login limiter.

## R14. Disabled or demoted users

Mint checks the account exists and is not disabled and holds a permission. Redeem rechecks all three (a user
disabled in the 60-second window gets nothing). Revalidate rechecks them every time.

Ending sign-ins ends module access too, in the same transaction: a password change, TOTP rotation or CLI
credential reset, revoking one admin session (conservatively: all of that person's module sessions), SSO unlink
and link replacement, and `identity.end` revoke the person's launch sessions and spend their unused tickets.
Ordinary admin UI logout and normal session expiry do not.

A module session started from an SSO sign-in expires no later than that admin session's IdP revalidation
deadline (`sso.revalidate_interval`), and a ticket cannot be minted past it. Local sign-ins are capped at 12
hours.

## R15. Why a restricted-token style actor in the module

The module session carries exactly the module permissions returned at the last revalidation, mapped by the
same function the module's PAT authenticator uses (work: the three permissions; people and jobs: the role
mapping with the permission-restricted set; finance: scopes). So an SSO session can never do more than a PAT
with the same permissions, and a permission removed in can-core is gone at the next revalidation.

## R16. Why not extract a shared `core/uikit/session`

The four copies are not copies of one thing: work keeps `uiSessions` with a per-request re-authentication
against a 30-second token cache, the other three keep `sessionStore` with recheck intervals, different caps
(20/5 per subject), cookie names, privileged-interval rules and `Principal` shapes. Unifying them without
changing behaviour means rewriting four session stores whose tests pin the differences, in the same change as
a security feature. Decision: keep the copies, add the ticket login to each, with the same shape and the same
tests. Recorded as a follow-up in `tasks.md`.

## R17. Module address in development

`CAN_INSTALLER=none` (the compose stack) installs no module chart, so there is no module in `modules` state and
the Apps page is empty; compose keeps the paste-a-token login. `CAN_MODULE_UI_URLS` plus
`CAN_COOKIE_SECURE=false` allow `http://localhost:<port>/...` for a developer who runs a module by hand; open
the admin UI on `localhost` too, so the launch cookie is shared (R7). `admin.localhost` with `work.localhost`
cannot share a cookie. can-core warns at start-up about a missing or single-label `CAN_COMPANY_DOMAIN` and
about unacceptable `CAN_MODULE_UI_URLS` entries (module id or position only, never the value).

## R18. Step-up in a ticket session

finance's party erasure asks a token session for its token again. A ticket session has none, so the step-up is
a launch at most 5 minutes old plus a revalidation at that moment that still holds `finance.admin`. It proves
the admin UI session was usable minutes ago; it does not ask for a password. people, jobs and work have no
token step-up.
