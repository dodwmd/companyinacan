# People self-service web UI

Employees and managers use the `people` module from a browser or a phone: their profile, the
directory and org chart, 1:1s, goals, time off and reviews. It is server-rendered Go templates with an embedded
stylesheet: no JavaScript, no framework, no build step. It is a client of the same service as the
JSON API (see [`people.md`](people.md)), so what a person may see or do is decided by the one policy
layer, not by the pages. Source: `core/people/ui*.go`. Specification: `specs/026-people-ui/`. Route
table: [`specs/026-people-ui/contracts/ui-routes.md`](../specs/026-people-ui/contracts/ui-routes.md).

This is a tool, not legal or HR advice. The UI is **experimental**: it is unit- and contract-tested
offline and has not been checked in a real browser, with a screen reader, or on a cluster.

## Turning it on

Off by default. In `people.yaml`:

```yaml
ui:
  enabled: true
  # insecure_cookies: true   # plain-HTTP development only (drops Secure and the __Host- prefix)
  # trust_proxy: true        # behind your ingress: take the client address from X-Forwarded-For
  # notifications_url: https://admin.example.com/notifications
  # idle_minutes: 30
  # absolute_hours: 8
```

The UI is served on the module's listener at `/`; `/v1/*` and `GET /healthz` stay the API. In the dev
compose stack it is at `http://people.localhost:8080/` (`dev/compose/config/people.yaml` turns it
on). Behind TLS leave the cookie flags at their defaults. The Helm chart does not expose the `ui`
block or a route yet (follow-up).

## Signing in

1. Create a personal access token in the admin UI (tokens page) with the people permissions you
   need. Users who signed in through single sign-on have `user:sso-...` tokens.
2. Open the people site, paste the token at **Sign in**. The server checks it with the same
   authenticator chain the API uses and needs an active person record behind it (the shared service
   account cannot sign in).
3. You get a session: a random id in an `HttpOnly`, `SameSite=Strict` cookie (`Secure`, with a
   `__Host-` name, over TLS), scoped to the people origin. The server stores only its SHA-256.
   Idle sessions end after 30 minutes, any session after 8 hours; **Log out** deletes it on the
   server. Every request re-checks that the person is still active staff, and every five minutes the
   token is checked again, so revoking the token or offboarding the person ends the session.
4. The token is held in server memory only (to re-check it) and is never sent back to the browser.
   A restart signs everyone out. The admin UI's session cookie is not used here.

Single-sign-on login without pasting a token is a follow-up.

## What each page shows

| Page | Notes |
|---|---|
| **Home** | Open checklist tasks (mark done), approvals waiting (only when can-core approvals are wired in; otherwise absent), upcoming 1:1s, goals in progress, link to notifications when configured |
| **My profile** | Edit preferred name and work phone. Other details are HR's |
| **Sensitive details** | A notice and the fields you may reveal. Pressing **Reveal** reads them through the service, which records the read per field (never the values) before returning anything. Fields you may not read say so. Date of birth, national id, home address, personal phone and compensation appear nowhere else |
| **Directory** | Search by name, email, team, position or location (reads at most 500 people, shows 50). Someone on leave appears as active to colleagues, as everywhere |
| **Org chart** | The reporting line above a person (at most 12 levels, no id twice) and their direct reports |
| **My team** | Your direct reports with links to their goals and 1:1 series, and a button to start a series |
| **1:1s** | Series, meetings, agenda, shared notes, action items. The manager also has a private notes box the other person never sees and cannot detect |
| **Goals** | Personal, team and company goals, key results with progress, alignment, visibility. Edit forms appear only where the service would allow the change |
| **Time off** | Balances by leave type, a request form with half days (afternoon start, morning end), draft, submit, cancel; the manager's queue with approve and decline (notes are visible on the request page, not in lists); the team calendar |
| **Reviews** | Your review (self-assessment, nominate peers, the released outcome, acknowledge), reviews of your reports (manager form, approve nominations), feedback requests addressed to you |

## Privacy properties (tested)

- Pages for people, series, meetings, goals, reviews, leave requests and feedback requests you may not
  see are the same 404 as for ones that do not exist.
- The calendar shows colleagues as **out** and nothing more; type and status appear only for yourself
  and where the service returns them (your manager, HR). Drafts and pending requests are nobody else's business.
- While a review cycle is open no page shows how many people answered feedback or who did, even to
  managers and HR; after release the subject sees only what the service releases. Calibration notes
  are never shown to the subject. The inbox says who to review, not who asked.
- Every response is `Cache-Control: no-store` without `ETag` or `Last-Modified`.
- All text is escaped; the content security policy forbids scripts, inline styles and other origins.
- Forms carry a CSRF token; the browser's `Sec-Fetch-Site` must be same-origin when sent.
- Failed sign-ins are rate-limited per address; a valid token is never throttled.

## Accessibility and phones

Mobile-first layout, 44 px targets, labelled fields, one `h1` per page, landmarks and a skip link,
captioned tables, visible focus, dark mode, `prefers-reduced-motion`, contrast checked at WCAG AA in
both colour schemes. A web app manifest lets a phone install it; there is no service worker (no
script), so there is no offline mode or push. Strings live in the message catalogue
(`core/people/ui_messages.go`), so a translation is another map. Tests check the markup
mechanically; a keyboard and screen-reader pass is still to be done.

## Testing

`go test -race ./core/people/...` (UI tests are `TestUI*`). Golden HTML lives in
`core/people/testdata/ui/`; after reviewing a deliberate change run
`go test ./core/people -run TestUIGoldenPages -update`. `dev/compose/smoke.sh` checks the login page,
the redirect, a refused wrong token and that the API still answers.

## Not done

SSO-integrated login; HR administration screens (review cycles, templates, calibration, release,
leave types, holidays, blackouts: use the JSON API); chart values for the `ui` block and a route;
sessions that survive a restart; a browser and screen-reader pass.
