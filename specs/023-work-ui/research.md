# Research: Work Tracker Web UI (E14c)

## R1. Where the UI lives
- **Decision**: Inside `can-work`, package `core/work`, under `/ui/`, embedded assets.
- **Rationale**: The UI must use the tracker's store, scope rules, limiter and authenticators. A
  separate service would need the API plus a second token store; a separate Go package would create
  an import cycle with `NewApp`. `/ui/` does not collide with `/v1/` or `/healthz`, so the edge
  needs no new route in the compose stack (`work.localhost` already proxies the whole host).
- **Alternatives**: the admin UI (E12) hosting work pages (wrong dependency direction, and the
  admin UI session secret would be shared); a SPA (needs a JavaScript toolchain, breaks the
  footprint and the strict CSP).

## R2. Authentication without the admin session secret
- **Decision**: Sign in by presenting a personal access token once. The server authenticates it
  with the module's `Authenticator` chain (so can-core personal access tokens, the module's own
  stored tokens and the bootstrap token all work), then keeps it in an in-memory session behind a
  random cookie id, and authenticates it again on every request.
- **Rationale**: Re-authentication gives immediate revocation, expiry and permission changes (for
  can-core tokens within the introspection cache's TTL). Memory-only storage means no token at rest
  and nothing in the browser except an opaque id. The cookie holds 256 bits of randomness; the
  server indexes sessions by SHA-256 of the id.
- **Alternatives**: storing a hash in SQLite (a hash cannot be re-authenticated, so revocation
  would not work); signed cookies carrying the token (token in the browser, no logout); sharing the
  admin UI's session secret (couples modules, forbidden by the task).
- **Follow-up**: SSO-based sign-in to module UIs (the can-core side is PR #30). When it lands, the
  login page gains a "Sign in with Company in a Can" button and the session holds the SSO subject
  instead of a token; the session table and CSRF code stay.

## R3. CSRF and cross-site posts
- **Decision**: Per-session random token in a hidden `csrf` field, compared in constant time;
  every POST also needs `Sec-Fetch-Site` of `same-origin` or `none` (when sent) and an `Origin`
  naming this host (when sent). The login form uses a double-submit cookie because no session
  exists yet. Cookies are `SameSite=Strict`.
- **Rationale**: Defence in depth: the token stops forged posts from browsers that do not send
  Fetch Metadata, Fetch Metadata and `Origin` stop most cross-site posts even before the token is
  looked at, `SameSite=Strict` stops the cookie from travelling at all.

## R4. Concurrency (ETag) in forms
- **Decision**: Every form that changes an existing issue carries `version`, the number the page
  showed (the API's ETag). The store's `If-Match` check rejects a mismatch atomically, and the UI
  answers 409 with a page that shows the issue now (and, for the edit form, the text the person
  typed so it is not lost). Rank changes do not bump the issue version in the store, so the UI
  compares the version before ranking (a small race window remains: two rank clicks within the same
  millisecond both succeed, which is harmless because ranking is last-writer-wins by nature).
- **Alternatives**: a hidden `updated_at` (less precise); ignoring conflicts on moves (the spec says
  lost updates must be reported).

## R5. Idempotent create
- **Decision**: The create form carries a random `idem` value generated per render and used as the
  store's idempotency key, so a double click, a browser resubmit or a retry after a timeout creates
  one issue.

## R6. Safe text
- **Decision**: `renderText` supports paragraphs, bullet lists, fenced code, `**bold**`, inline
  code and `[text](url)` for absolute `http` and `https` URLs without credentials, whitespace,
  quotes or angle brackets. Everything else, including raw HTML, is escaped text. Links carry
  `rel="noopener noreferrer nofollow"`. Attachment URLs go through the same gate before they become
  `href`s. A test feeds hostile inputs and checks that only tags the renderer owns come out.
- **Alternatives**: a Markdown library (new dependency, larger attack surface, and CommonMark
  allows raw HTML unless configured); plain text only (poor for descriptions).

## R7. Filters are the search language
- **Decision**: The URL carries `q` (the search language) and a few structured fields. The server
  composes `field:value` terms from the structured fields after checking each value against a
  conservative character set, adds `is:open` unless `scope=all`, and passes the result to
  `Store.ListIssues`, which parses it with the existing scoped parser (bound parameters only, scope
  ANDed to the whole query) under a four second deadline.
- **Rationale**: One query language, one parser, no second implementation to keep safe.

## R8. Residual risks and limits
- A token pasted into a page is as sensitive as the token itself: the page is only served over TLS
  in production (cookies are `Secure` and `__Host-` prefixed when TLS or a trusted proxy header is
  seen), and the token is never written to disk. It lives in process memory; a memory dump reveals
  it. Prefer short-lived personal tokens with `work.write` only; do not sign in with the bootstrap
  admin token for daily work.
- Sessions do not survive a restart and are not shared between replicas (the tracker is a single
  replica with SQLite).
- The UI has no route through can-core's installer yet (the module has no subdomain), so it is
  reachable inside the cluster or through a port-forward until the module declares one.
- Accessibility is checked statically and by contrast arithmetic. No screen reader or real device
  pass was done.
- The board shows at most 200 cards per column (the store's limit) and says so; sprint pages read
  at most 8 open sprints and 100 issues each; the dashboard shows at most 100 issues; epic progress
  reads at most 1000 children per page of epics.
