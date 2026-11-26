# 058: one session kit for the module web UIs

**Problem.** The work, people, jobs, finance and files UIs each carried a copy of session storage,
cookies, CSRF, launch-ticket handling, re-check intervals, step-up and security headers. QA kept
finding the copies drifting apart.

## Requirements

1. The security mechanics of a module UI session live in `core/uikit/session`. A module says who may
   sign in, with which permissions, how often that is asked again, its timeouts and bounds, and draws
   its own pages.
2. One conformance suite (`core/uikit/session/conformance`) runs against every module UI through its
   HTTP handler: CSRF, session fixation, cross-site posts, cookie flags, headers, idle and absolute
   expiry, the can-core deadline, re-check on revocation, step-up with a revoked credential, body
   limits, a demotion that takes effect and stays, and a login throttle that never blocks a valid credential. A module states its values
   there, so changing one fails a test.
3. Routes, templates and configuration do not change. Behaviour is preserved except where one module
   was weaker (below).

## What changed

| # | Behaviour | Before | After |
|---|---|---|---|
| 1 | `Origin` header on a post | only work checked it | all five: it must name this host; `Origin: null` only with `Sec-Fetch-Site: same-origin` (work's rule) |
| 2 | `Cross-Origin-Resource-Policy: same-origin` on pages | files, finance, work | all five |
| 3 | `Content-Security-Policy` | work had no `script-src` (covered by `default-src 'none'`) | one policy, with `script-src 'none'` |
| 4 | work session cookie | `Max-Age` 24 h: it survived closing the browser | ends with the browser session, as in the other four; server timeouts unchanged |
| 5 | people sessions of a `manager` (approves time off and reviews) | re-checked every 5 min | every 1 min, as approvers elsewhere |
| 6 | Looking at whose session a browser holds (ticket pages) | work extended the idle timeout by it | no module does |
| 7 | A launch ticket redeemed for an account that is not `user:...` | people, finance and jobs relied on their own subject or permission checks | also refused by the kit |
| 8 | A token re-check that names a different subject | work did not compare | ends the session in all five |
| 9 | When a re-check is due | at an age over the interval (work tickets: at the interval) | at the interval |
| 10 | Expired sessions | work swept them at each sign-in; the others kept them until used or evicted | all swept at each sign-in |

One consequence of row 1: a browser sends `Sec-Fetch-Site` only to `https` and loopback addresses
(`localhost`, `*.localhost`, `127.0.0.1`). A UI served with `insecure_cookies` over plain HTTP on
any other name now refuses its own form posts (`Origin: null` with nothing to vouch for it), as
work already did. Cookies without `Secure` are for local development only.

Another consequence: a browser that does not send `Sec-Fetch-Site` at all (Safari before 16.4, for
one) posts `Origin: null` from these pages, with nothing to vouch for it, so it cannot sign in or
submit any form in a module UI.

## Behaviour matrix (after)

| | work | people | jobs | finance | files |
|---|---|---|---|---|---|
| Idle / absolute | 8 h / 24 h, fixed | 30 min / 8 h, configurable | same | same | same |
| Sessions: total / per person | 5000 / 20 | 5000 / 5 | 5000 / 5 | 5000 / 5 | 2000 / 5 |
| Token re-check | every request | 5 min; 1 min manager, hr, admin | 5 min; 1 min if it can write, read candidates or reports | 5 min; 1 min write, approve, admin, ledger write, approve or admin | 5 min; 1 min write or admin |
| Ticket re-check | 30 s | as token | as token | as token | as token |
| Introspection cache (token) | 30 s | 30 s | 30 s | 30 s | 30 s |
| Session cookie | browser session | browser session | browser session | browser session | browser session |
| Sign-in cookie | 1 h, new token per form | 15 min, kept while valid | same | same | same |
| Cookie flags | `HttpOnly`, `SameSite=Strict`, `Path=/`, `Secure` and `__Host-` when secure | same | same | same | same |
| `Secure` decided | per request (TLS, trusted proxy, or forced) | configuration | configuration | configuration | configuration |
| Public form bound | 4 KiB, 16 fields | same | same | same | same |
| Signed-in form bound | 512 KiB (or `max_body_bytes`) | 64 KiB | 128 KiB | 256 KiB | 64 KiB; uploads streamed |
| Over the signed-in bound | 413 | 403 (form check) | 403 | 403 | 403 |
| Failed sign-ins per address | `auth_failures_per_minute` (30), shared with the API, 300 ms delay | bucket of 10, one back per 5 s | 10 per minute | 10 per minute | 10 per minute |
| First failure answered 429 | 31st | 11th | 11th | 11th | 10th |
| Valid credential while limited | accepted | accepted | accepted | accepted | accepted |
| Step-up | none | none | none | party erasure | subject erasure |
| Person re-checked in the module | - | active staff, every request | - | - | groups, every request |

Everything in this table that is not listed under "What changed" behaved the same before.

## Left as they were, on purpose

These differ between modules but none is clearly a defect; each is a product decision.

- **work's timeouts** (8 h idle, 24 h absolute, against 30 min and 8 h elsewhere) are specified in
  [023](../023-work-ui/spec.md). work re-authenticates every request, but an unattended browser
  stays signed in much longer. Changing them means changing that specification.
- **Throttles** keep their own limiters and thresholds; only work delays a limited failure. Tokens
  carry 256 bits, so no limit here is what stops guessing.
- **The admin UI** (can-core) has sessions in its database, shared with the admin API, with
  passwords, TOTP, account lock-out and its own step-up. It is a different design, not a copy, and
  is not touched.
