# Work tracker web UI

The work tracker (`can-work`) serves a small web UI next to its API: projects, a filterable
backlog, an issue page, a board, sprint planning, epic progress and a manager dashboard. It is
server-rendered HTML with one stylesheet. There is no JavaScript, no framework and nothing to
build. Everything is a link or a form, so it works with a keyboard, with a screen reader, on a
phone, and in a browser with scripts blocked.

Status: **experimental**, like the module. Built and tested offline; checked against the local
compose stack. No manual screen-reader or real-device pass has been done. Design and decisions:
`specs/023-work-ui/`.

## Where it is

| URL | |
|---|---|
| `/ui/` | project list (after sign-in) |
| `/ui/login` | sign in |
| `/` | redirects to `/ui/` |

The API (`/v1/...`) and `/healthz` are unchanged. In the compose stack the UI is at
<http://work.localhost:8080/ui/>. On a cluster the tracker has no public route yet (the module has
no subdomain; see "Reaching it on a cluster"), so use a port-forward:

```sh
kubectl -n mod-work port-forward svc/work 8080:8080     # then open http://localhost:8080/ui/
```

The full list of routes, with the session and permission each needs, is
`specs/023-work-ui/contracts/ui-routes.md`; a test keeps it equal to the code.

## Signing in

Paste a **personal access token**. Tokens that work:

* a personal access token issued in the admin UI (Tokens) for the `work` module, when the module is
  installed through can-core (`CAN_API_URL` is set); it carries the permissions you chose there;
* a token minted by the tracker itself (`POST /v1/tokens`, `cwt_...`);
* the bootstrap admin token (works, but keep it for emergencies: use a personal token).

The token needs `work.read` at least; `work.write` shows the forms and buttons that change things.
A token limited to some projects shows only those projects everywhere (a project or issue outside
the scope is a plain "Not found", exactly like one that does not exist).

What happens to the token: the server checks it, keeps it **in memory** for this session, and
checks it again on **every request**, so revoking it in the admin UI or expiring it ends the
session at the next click (for can-core tokens within the introspection cache lifetime, 30
seconds by default). Your browser only holds a random session id (`HttpOnly`, `SameSite=Strict`,
`Secure` and `__Host-` prefixed over TLS). The token is not written to disk, logged, put in the
cookie, or shown again. Sessions end after 8 hours idle or 24 hours, on log out, and when the
process restarts.

Wrong tokens are throttled per client address (the same limit as the API's failed
authentications, `rate_limit.auth_failures_per_minute`). A valid token is never throttled, so
somebody guessing from your office address cannot lock you out.

Single sign-on to the module UI is **not** available yet; it is a follow-up. The company's OpenID
Connect sign-in (E11b) covers the admin UI only; staff use a personal access token for modules, as
here. When module-level SSO exists the login page will gain a button, and the token paste stays as
the fallback.

## Pages

**Projects** lists the projects your token covers, with issue counts and links to the board,
sprints and epics.

**Backlog** (project home): a filter (words, status, priority, type, assignee, label, order, open or
all issues), the issue list in pages of 25, and an inline "Create an issue" form. The filter lives
in the page address, so you can bookmark or share it. Words go to the tracker's search language:
`status:in_progress label:web due<2026-12-01 -status:done "exact phrase"`; `assignee:me` means you.
A search the language does not understand shows why; one that runs too long is stopped after four
seconds and asks you to narrow it. The create form is safe against a double click: it carries a
one-time key, so one issue is made.

**Issue** shows description, details, child issues, links (blocks, relates to, duplicates), attachment
details, comments (oldest first), watchers and the activity history. With `work.write` it also has:
move to an allowed status, priority, assignee ("Assign to me" or empty to unassign), sprint, edit
(title, description, labels, estimate, due date, parent), add or remove a link, comment, watch or
stop watching. Descriptions and comments accept a tiny, safe subset of Markdown (below).

**Board** has one column per workflow status. Each card shows its key, type, priority, title,
assignee, estimate and labels, and **buttons**: one per allowed move (an arrow and the target
status) and Up and Down to change its order within the column. There is no drag and drop on
purpose: buttons work with a keyboard, a screen reader and a thumb. Choose what the board shows:
the active sprint (default), the backlog, everything, or a planned sprint. A column shows up to
200 cards and says when there are more.

**Sprints** lists planned and active sprints with their issues and points (the sum of estimates),
with "Start sprint" and "Complete sprint" (choose where unfinished issues go: the backlog or another
planned sprint), a form to create a sprint, the backlog of open issues that are not in a sprint with
an "Add to sprint" choice on each, and the closed sprints.

**Epics** lists epics with a progress bar and "N of M done" over their child issues.

**Team dashboard** takes a list of people (email addresses or user names, separated by commas) and
shows each person's open **P0 and P1** issues in every project you can see. The list is in the page
address; the page does not call the people service.

### Conflicts

Every button or form that changes an existing issue carries the version the page showed (the same
number as the API's `ETag`). If somebody else changed the issue in between, nothing is applied and
you get a "This issue changed" page (409) with the issue as it is now. After a failed edit your
typed title and description are shown so you can copy them. Moving a card whose status already
changed, or ranking a card that was edited, is reported the same way. Illegal workflow moves are
refused with the list of allowed moves.

### Markdown subset

Paragraphs, `- ` bullet lists, fenced code blocks, `**bold**`, `` `code` `` and `[text](url)`. A link
must be an absolute `http` or `https` URL without a user name or password, spaces or quotes;
anything else stays text. Links get `rel="noopener noreferrer nofollow"`. Raw HTML is never
interpreted: `<script>` is shown as the characters it is.

## Security model

* **Every route** needs a session, and then a permission (`work.read` or `work.write`), plus project
  scope, which the store enforces exactly as for the API. An invisible project, issue or sprint is a
  404 with the same page as a missing one.
* **CSRF**: every post carries the session's token in a hidden `csrf` field and must also come from
  the same origin (`Sec-Fetch-Site`, `Origin`). The login form uses a double-submit cookie. Cookies
  are `SameSite=Strict`.
* **Session fixation**: every login issues a new id; a cookie sent with the login is ignored.
* **Headers**: `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self';
  manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'` (scripts are
  never allowed), `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
  `Referrer-Policy: no-referrer`, `Cache-Control: no-store`.
* **Output**: `html/template` escapes everything; the Markdown subset is the only way to produce tags
  and it only produces the ones it owns. Tests feed hostile titles, descriptions, comments, labels,
  assignees, sprint names, attachment names and addresses, search text and path parts through
  every page.
* **Work is bounded**: forms are capped (512 KiB), lists are paged, a board shows 200 cards per
  column, sprint pages read at most 8 open sprints, searches have a deadline, every request has a
  12 second budget, sessions are capped (5000 in all, 20 per person).
* **Errors** show a short message and, for unexpected ones, a request id that matches the log. They
  do not show SQL, paths or tokens.

Residual risk: the token you paste is as sensitive as the token itself. Serve the UI over TLS only
(behind the cluster's ingress, or a trusted reverse proxy that sets `X-Forwarded-Proto: https` with
`trust_proxy: true`, so cookies become `Secure`). Prefer a personal token with only `work.write`,
short-lived, scoped to your projects. A memory dump of the process would reveal tokens of signed-in
users.

## Accessibility and phones

The pages use landmarks, a skip link, one `h1` per page, labelled controls, captions and column
headers on tables, `aria-current` on the current tab, visible focus rings, large (44 px) targets,
and a colour pair that passes WCAG AA contrast in both the light and dark schemes (checked by a
test). Colour is never the only signal: priorities, types and statuses are words. Motion is off
when the system asks for reduced motion. On a narrow screen tables turn into stacked cards and the
board becomes a single column; on a wide screen the board scrolls sideways. The page has a web app
manifest, so the browser can offer to install it on Android (it opens at `/ui/`).

Automated checks cover the markup (`ui_a11y_test.go`: labels, ids, headings, landmarks, captions,
CSRF fields, no scripts) and the stylesheet's contrast pairs. They do not replace a pass with a
screen reader, a keyboard and a real phone, which has not been done yet.

## Reaching it on a cluster

The `work` module declares no subdomain, so can-core's installer routes nothing to it and the chart's
`ingress` stays off. The UI is therefore reachable inside the cluster, or through a port-forward
(above). When the module gets a `runtime.subdomain`, a chart value of
`ingress.pathPrefixes: ["/ui"]` publishes only the UI and keeps `/v1` internal (the chart already
supports path prefixes; the API stays unrouted). That change is not made here because it also
needs a decision about exposing a login form on the public internet.

## Operating notes

* Config: nothing new. The UI uses `trust_proxy`, `max_body_bytes` and the `rate_limit` block.
* Logs: the access log already omits query strings and bodies; the UI adds no token or form value
  to it.
* A restart signs everybody out. If can-core cannot be reached to check a can-core token, requests fail closed: the session ends and the next click goes to the sign-in page.
* Footprint (static build, development machine): idle RSS 13.6 MiB without the UI and 16.8 MiB with it; binary 12.4 MB to 15.1 MB, mostly `html/template`. Not measured on a cluster.
* Roll back: the UI adds no table or migration; an older binary simply has no `/ui/`.
* Local compose: `dev/compose/smoke.sh` signs in with the dev admin token and opens the SMOKE
  project through the edge.

## Known gaps

SSO sign-in; a public route; creating projects, editing workflows and managing tokens from the UI
(use the admin UI or the API); uploading attachments; bulk edit; keyboard shortcuts; saved named
filters; a people picker; burn-down charts; other languages (the catalogue is ready: one map per
language). See `specs/023-work-ui/tasks.md`.
