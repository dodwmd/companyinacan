# Research and decisions: People web UI (E13c)

## Where the UI lives

Inside package `people`, next to the Service, not a sub-package: `cmd.go` mounts it, and a
sub-package that imports `people` could not be imported back. One binary, one process, no new image
(the 30 MB RAM target of the module holds; measured in `specs/012-people-directory/measurements.md`,
not re-measured for the UI).

## Authentication

Module auth is can-core personal access tokens (`specs/016-module-wiring`). A browser cannot hold
a bearer header, so a PAT is exchanged once for a server-side session scoped to the people origin.

- **Why the PAT is kept in server memory.** A session that outlives revocation of its PAT is a
  credential that cannot be withdrawn. Keeping the token in memory (never on disk, never sent back
  to the browser) lets the session re-authenticate through the shared `Authenticator` every 5
  minutes. The alternative (store only the principal) makes revocation wait for the 8-hour
  absolute timeout. A memory disclosure of the process would expose live PATs; the same process
  already holds the keyring that decrypts every sensitive column, so the trust boundary is the same.
- **Why not the admin UI cookie.** `E11b` removed the UI session value from module introspection;
  the admin cookie is scoped to another origin and must never be a module credential. This UI never
  reads it.
- **Sessions are in memory**: a restart signs everyone out. Acceptable for a self-service tool;
  nothing is lost, and a leaver cannot survive a restart.
- **SSO follow-up.** The merged SSO gives SSO users `user:sso-...` PATs, which already work here.
  A smoother flow (redirect to can-core's OIDC login, return with a one-time code the module
  redeems for a session, no pasting) needs a can-core endpoint that does not exist yet; documented as
  the follow-up, not faked.
- **Throttling.** As in the API, the credential is checked first and only failures draw on the
  per-address budget (burst 10, one per 5 s). A flood of bad tokens cannot lock out a valid user. A
  known limit shared with the API: junk tokens still cost one introspection round trip each (the
  introspector caches negatives for 5 s).
- **Login needs a person.** A token whose subject has no active person record cannot sign in, so
  the shared `svc-can-core` service token is not a UI login.

## CSRF and cookies

`SameSite=Strict` already blocks cross-site sends; the session CSRF token (forms) and the
`Sec-Fetch-Site` check are defence in depth for same-site subdomains and older browsers. The login
form uses a double-submit cookie (random 256 bits) because there is no session yet. `__Host-`
prefixed names when `Secure` (no `Domain`, `Path=/`).

## Presentation vs authorisation

The UI shows links broadly (every signed-in user sees every nav item) and lets the Service answer
for empty or forbidden pages. Role-based hiding would be a second, drifting opinion. Where a form
would be misleading (goal edit, review forms) a Service helper wraps the same `Decide` call the
write path uses.

## Anonymity details

The service returns `FeedbackView.Responses` and `RatingAgg.N` to managers and HR (they may need
them once feedback is shown). The UI nevertheless draws nothing about counts or who answered while
the cycle is `open`, and shows aggregated results only after the service says `Shown`. Nominations
show the nomination workflow state (proposed, approved, declined) only, which the service already
keeps independent of whether a reviewer answered.

## Directory search cost

`ListPeople` filters and masks per row. The UI reads up to five pages of 100 and matches in memory
after masking, so a search never filters on a masked field in SQL and never walks more than 500 rows.

## PWA

A manifest and icon make the site installable. A service worker would need script, which the CSP
forbids; offline use and push are not offered.

## Alternatives rejected

- A SPA or htmx: contradicts "no JS framework, `script-src 'none'`".
- Storing sessions in SQLite: adds a table of credential-equivalents to a database that is backed up.
- Role checks in handlers: second authorisation path.
