# Implementation plan: People self-service web UI (E13c)

Spec: [spec.md](spec.md). Research: [research.md](research.md). Data model: [data-model.md](data-model.md).
Contract: [contracts/ui-routes.md](contracts/ui-routes.md).

## Shape

- Package `core/people`, files `ui.go` (handler, middleware, routes, login/logout, static),
  `ui_session.go` (session store), `ui_pages.go` (home, profile, directory, org, 1:1s, goals),
  `ui_pages_e14b.go` (time off, reviews, feedback), `ui_messages.go` (catalogue),
  `ui/templates/*.html`, `ui/static/*` (embedded). Mounted by `cmd.go` next to the API when
  `ui.enabled`; `/v1/*` and `GET /healthz` still go to the API.
- Small `Service` helpers so the UI never decides access: `Actor.Active`, `SensitiveFieldsFor`,
  `CanWriteGoal`, `ReviewCapabilities` (each wraps a decision the write path already enforces).
- Seams as interfaces: `ApprovalSource` (nil: no approvals section), `NotificationsURL` (empty: no link).

## Constitution check

| Principle | Check |
|---|---|
| I Small and honest | No JavaScript, no framework, no new dependency; templates and CSS are embedded; idle cost is a map of sessions. Compose people service stays at its RAM limit (UI adds no process). |
| II Open and replaceable | Apache-2.0; plain `html/template`; the UI is a client of the documented Service, replaceable by another client. |
| III Test first | Tests were written before the handlers and seen to fail (undefined `UI`; then each page); golden HTML, a11y checklist, contrast, catalogue, route-contract tests. |
| IV Secure by default | Deny by default through the policy layer; CSRF, CSP, no-store, `__Host-` cookies; failed-login throttle; hashed session ids; no sensitive field outside the sensitive screen. |
| V Sensitive data in-house | No data leaves the module; the UI adds no store; sensitive reads audited before they are returned; sessions never persist the token to disk. |
| VI Honest status | Marked experimental, not browser- or cluster-verified; gaps listed in `tasks.md` and the PR. |

## Design decisions in brief

1. Session: server-side, in memory, SHA-256 of a 256-bit id; bounded (5000, 5 per subject).
2. Token kept in memory only, to re-authenticate every 5 minutes (revocation) -- see `research.md`.
3. Login requires an active person; the shared static service token (no person) cannot sign in.
4. Redirect targets after POST are fixed local paths built from ids the service accepted, never from request input.
5. Error pages share one template; a 404 never marks a section as current.

## Testing

`go test -race ./core/people/...` including: lifecycle, timeouts, CSRF, hostile text, sensitive
audit, leave masking, 404 uniformity, private notes, goals flow, tasks/approvals seam, leaver loses
routes, cyclic org chain, throttling, static/no inline script, no second authorisation path,
time off flow and masked calendar, restricted PATs, review flow with calibration, anonymous feedback
without counts, route contract, catalogue completeness, a11y checklist, stylesheet contrast, goldens
(`-update` rewrites them).
