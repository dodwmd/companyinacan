# Research: Admin UI (E12a)

**R1. Rendering: `html/template` with no JavaScript.** SPEC section 7 says "Server-rendered + HTMX". HTMX
is optional progressive enhancement; every first-slice interaction (filter, page, requeue, verify, revoke,
status change) is a form post with a redirect, which needs no JS. Skipping HTMX keeps the CSP at
`script-src 'none'`, removes a vendored 14 KiB dependency and its licence entry, and means the UI cannot be
broken by a script failure. HTMX can be added later for partial refresh behind a `script-src 'self'`
asset if a screen needs it; that is a spec amendment, not a rewrite.

**R2. Session and CSRF reuse.** The UI uses the same `can_session` cookie and server-side sessions as the
API (`auth.Service.Authenticate`), so a login in either is valid in both. The API requires an
`X-CSRF-Token` header; forms cannot set headers, so the UI takes the same per-session token from a hidden
`csrf` field and compares with `auth.CSRFValid` (constant time). `SameSite=Strict` already blocks
cross-site sends in modern browsers; the token and a `Sec-Fetch-Site` check are defence in depth.

**R3. Login CSRF.** The login form has no session yet. A random token is set as an HttpOnly,
SameSite=Strict cookie on `GET /login` and must equal the hidden field (double submit). This stops
login-CSRF (forcing a victim into the attacker's session) without server state.

**R4. No service worker.** Chrome on Android installs a PWA from a valid manifest with icons and an HTTPS
origin; a fetch-handling service worker has not been required for installability since Chrome 108. An
offline shell has no value for an admin UI whose every page is live data behind a login, and a service
worker that caches responses risks serving one user's authenticated page after logout or to the next
user of a shared device. We ship the manifest only and document the decision; PWA hardening (E19) may add
a worker that caches only `/static/*` and the offline page, never authenticated HTML.

**R5. Icons.** Two small PNGs (192 and 512, maskable-safe padding) and an SVG favicon, generated once by
a throwaway Go program and committed; no image tooling is needed at build time.

**R6. Account operations.** `SetAdminCredentials` already ends all of a user's sessions, so a password or
TOTP change logs the operator out everywhere; the UI says so. Rotation needs proof of possession of the
new secret: the secret is rendered in a hidden field of the confirmation form (it is shown on the page
anyway; the operator is authenticated and re-enters the password), and the confirm step validates a code
generated from it. No server-side pending state is needed. Wrong current passwords are throttled per
account (5 failures, 15 minutes) so a stolen session cannot be used as a password oracle.

**R7. Audit filters.** `audit.Log.Query` filters on actor (exact), action (literal prefix, no LIKE
wildcards), target (exact) and pages with `before=<seq>`; newest first. The hash chain is unchanged.

**R8. Leads access.** The forms gateway authenticates callers with bearer tokens carrying `leads.read` and
`leads.write`. can-core resolves the provider with `registry.Resolve("leads")` (the URL is built from
validated DNS labels, so it cannot be redirected) and calls it with a bearer token read from
`CAN_LEADS_TOKEN_FILE` on every call (so rotation needs no restart). Until the token is provisioned by the
chart the leads page shows an explicit "not configured" empty state. Redirects are not followed and
responses are size-limited.

**R9. i18n.** A single `map[string]string` catalogue (`messages.go`) keyed by dotted ids, looked up by a
template function. Adding a language means adding a catalogue and choosing it per request; nothing else
changes. Dates are rendered in UTC with a `<time datetime>` element.

**R10. Static accessibility check.** There is no HTML parser in the standard library and adding
`golang.org/x/net/html` only for tests is not worth a dependency, so the checklist test tokenises the
generated markup with a small regexp-based scanner (the markup is ours and well formed). It is a
guard-rail, not a substitute for a manual axe/screen-reader pass (listed under Known gaps).
