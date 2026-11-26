# Contract: work UI routes

Served by `can-work` itself next to the JSON API (`/v1/*`, `/healthz`), under the `/ui/` prefix.
`GET /` answers `303 /ui/`. A contract test (`TestUIRoutesMatchContract`) compares this table with
`work.UIRoutes()`, and `TestUIRoutePermissionTable` drives every row: an anonymous request to a
`Session = yes` route gets `303 /ui/login`, a token without `work.write` gets 403 on every
`work.write` row, a token without `work.admin` on every `work.admin` row, and a project outside the token's scope is 404 (never 403) on every project or
issue route.

Unsafe requests are `application/x-www-form-urlencoded` posts carrying the hidden field `csrf` (the
session's CSRF token; on the login form, the double-submit token of the login cookie). Every post
is also refused when `Sec-Fetch-Site` is not `same-origin` or `none`, or when `Origin` names another
host. Posts that change an existing issue carry `version` (the version the page showed, the same
number as the API's `ETag`); a stale `version` is answered `409` with a page that shows the issue as
it is now, and nothing is applied. Redirects after a post go to the form field `next` when it is a
local `/ui/` path, otherwise to the default noted below, with `ok=<name>` for a one-line notice.

| Route | Session | Permission | Purpose |
|---|---|---|---|
| `GET /ui/login` | no | none | Sign-in form; sets the login CSRF cookie. Query `next` (local `/ui/` path) |
| `POST /ui/login` | no | none | Fields `token` (a personal access token), `csrf`, `next`. 303 on success with a new session; 401 or 429 re-render the form; 403 for a missing or wrong login CSRF token |
| `GET /ui/login/ticket` | no | none | Single sign-on landing (E11c, `specs/036-module-ui-sso`): query `t` (a launch ticket). Shows a Continue form only; it never consumes the ticket, so a link scanner or prefetch cannot spend it. Sets the login CSRF cookie. 400 for a malformed ticket, 404 when not installed under can-core |
| `POST /ui/login/ticket` | no | none | Fields `ticket`, `csrf` (double-submit). Redeems the ticket at can-core (this module is the audience), then 303 to a clean URL (`/ui/` or the checked landing path) with a new session. 401 for every refusal (unknown, used, expired, foreign ticket: one message), 403 stale form or cross-site, 429 after repeated failures (a valid ticket is never throttled), 503 when can-core cannot answer. 403 for the form when `ui.token_login` is off applies to `POST /ui/login` only |
| `POST /ui/logout` | yes | work.read | Ends the session; 303 `/ui/login` |
| `GET /ui/{$}` | yes | work.read | Project list (only projects the token can see) |
| `GET /ui/dashboard` | yes | work.read | Manager dashboard. Query `assignees` (comma separated, at most 20): open P0 and P1 issues per person |
| `GET /ui/team/workload` | yes | work.read | Who is on what: unresolved work per person. Query `project` (`specs/065-work-manager`) |
| `GET /ui/team/ageing` | yes | work.read | Unresolved P0 and P1 by time at that priority. Query `project` |
| `GET /ui/team/stale` | yes | work.read | Issues in progress with no activity. Query `project`, `days` (1 to 365, default 7) |
| `GET /ui/filters` | yes | work.read | Your saved filters and the shared ones; a form for a new one when you have work.write |
| `GET /ui/filters/{filter}` | yes | work.read | The issues a saved filter matches now, as you may see them. Query `page` |
| `POST /ui/filters` | yes | work.write | Fields `name`, `project`, `query`, `sort`, `shared` (`1`); 303 to the filter |
| `POST /ui/filters/{filter}/delete` | yes | work.write | Your own filter; with work.admin also a shared one |
| `GET /ui/projects/new` | yes | work.admin | New project form (403 for a token limited to some projects) |
| `POST /ui/projects` | yes | work.admin | Fields `key`, `name`, `description`; 303 to the project. A rejected form is shown again with what was typed (409, 422) |
| `GET /ui/projects/{key}/settings` | yes | work.admin | Name and description, members, archive or restore |
| `POST /ui/projects/{key}/settings` | yes | work.admin | Fields `name`, `description` |
| `POST /ui/projects/{key}/archive` | yes | work.admin | Field `archived` (`1` archive, `0` restore) |
| `POST /ui/projects/{key}/members` | yes | work.admin | Field `subject`; adds a member (a roster, not an access list) |
| `POST /ui/projects/{key}/members/remove` | yes | work.admin | Field `subject` |
| `GET /ui/projects/{key}` | yes | work.read | Project home: filters, backlog list, inline create form. Query `q`, `status`, `priority`, `type`, `assignee`, `label`, `sort`, `scope` (`open`, `all`), `page` |
| `POST /ui/projects/{key}/issues` | yes | work.write | Fields `title`, `type`, `priority`, `assignee`, `labels`, `estimate`, `due_date`, `description`, `idem` (idempotency key); 303 to the project |
| `GET /ui/projects/{key}/board` | yes | work.read | Board by workflow column. Query `sprint` (`active`, `backlog`, `all` or a sprint id) |
| `GET /ui/projects/{key}/sprints` | yes | work.read | Sprint planning: open sprints with their issues, backlog, closed sprints |
| `POST /ui/projects/{key}/sprints` | yes | work.write | Fields `name`, `goal`, `starts_on`, `ends_on`; 303 to the sprint page |
| `GET /ui/projects/{key}/epics` | yes | work.read | Epics with child progress. Query `page` |
| `POST /ui/sprints/{id}/start` | yes | work.write | Start a planned sprint; 409 when the project already has an active one |
| `POST /ui/sprints/{id}/complete` | yes | work.write | Field `move_to` (planned sprint id, empty for the backlog) |
| `GET /ui/issues/{issue}` | yes | work.read | Issue detail: description, comments, activity, links, watchers, attachments, children. Query `ca`, `aa` (comment and activity cursors) |
| `POST /ui/issues/{issue}/edit` | yes | work.write | Fields `version`, `title`, `description`, `labels`, `estimate`, `due_date`, `parent` |
| `POST /ui/issues/{issue}/transition` | yes | work.write | Fields `version`, `to` (must be an allowed workflow move) |
| `POST /ui/issues/{issue}/priority` | yes | work.write | Fields `version`, `priority` |
| `POST /ui/issues/{issue}/assignee` | yes | work.write | Fields `version`, `assignee` (empty unassigns) |
| `POST /ui/issues/{issue}/sprint` | yes | work.write | Fields `version`, `sprint` (empty removes from its sprint) |
| `POST /ui/issues/{issue}/rank` | yes | work.write | Fields `version`, one of `before`, `after` (issue key) or `position` (`first`, `last`) |
| `POST /ui/issues/{issue}/comments` | yes | work.write | Field `body` |
| `POST /ui/issues/{issue}/links` | yes | work.write | Fields `link_type` (`blocks`, `relates`, `duplicates`), `target` |
| `POST /ui/issues/{issue}/links/{link}/remove` | yes | work.write | Remove a link the issue is part of |
| `POST /ui/issues/{issue}/watch` | yes | work.write | Field `on` (`1` watch, `0` stop); only ever the caller |
| `GET /ui/static/{file}` | no | none | Embedded stylesheet and icon (cache one hour) |
| `GET /ui/manifest.webmanifest` | no | none | Web app manifest (`start_url` and `scope` `/ui/`) |

Any other path under `/ui/` is the HTML 404 page, or 405 when the path exists for another method.

## Status codes on pages

200; 303 (redirects, login); 400 (unreadable form, bad search or filter value); 403 (CSRF or
cross-site post; read-only token on a write route; the token may not do that); 404 (unknown or not
visible: the same page for both); 405; 409 (stale version, illegal workflow move, sprint rules,
cycle, archived project); 413; 422 (validation problems listed on the page); 429 (too many failed
sign-ins, or too many requests); 500 (request id shown, details only in the log); 503 (search
timeout, `Retry-After`).

## Response headers (all `/ui/` responses)

`Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'` (no `script-src`: scripts are
never allowed), `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY`, `Permissions-Policy: camera=(), microphone=(), geolocation=()`,
`Cache-Control: no-store` (static files: `public, max-age=3600`).

## Cookies

| Cookie | Content | Flags |
|---|---|---|
| `work_session` (`__Host-work_session` over TLS) | random session id (256 bits); the server keeps only its SHA-256 | `HttpOnly`, `SameSite=Strict`, `Path=/`, `Secure` over TLS |
| `work_login_csrf` (`__Host-work_login_csrf` over TLS) | random double-submit token of the login form, one hour | same |
