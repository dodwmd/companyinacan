# Implementation plan: Recruiting web UI (E16b)

Spec: [spec.md](spec.md). Research: [research.md](research.md). Data model: [data-model.md](data-model.md).
Contract: [contracts/ui-routes.md](contracts/ui-routes.md).

## Shape

- Package `core/jobs`, files `ui.go` (handler, middleware, routes, login/logout, static),
  `ui_session.go` (session store), `ui_pages.go` (all pages), `ui_messages.go` (catalogue),
  `ui/templates/*.html`, `ui/static/*` (embedded). Mounted by `NewApp` on the module's listener when
  `ui.enabled`; `/v1/*`, `/careers*`, `/feeds/*`, `/robots.txt` and `GET /healthz` still go to the
  API and the public pages (`reservedPath`).
- Service-side helpers in `svc_web.go` (named so the "no authorisation in ui*.go" test does not
  scan them): `Principal.Caps`, `Store.Require`, `FunnelCounts`, `ListMyInterviews`, `IsAssigned`,
  `GetCandidateSummary`, `SearchTalentPool`. Each applies the same role check and scope predicate as
  the methods it sits next to.
- `Store.MoveApplicationNote` and migration 004 (`stage_history.note_sealed`): a stage move carries a
  sealed reason. Key rotation and erasure cover the new column; the JSON API's `move` is unchanged
  (no note).
- Config: `ui.enabled` (default false), `ui.insecure_cookies`, `ui.idle_minutes`,
  `ui.absolute_hours`.

## Constitution check

| Principle | Check |
|---|---|
| I Spec-first | This directory precedes the code; SPEC.md already names the web client. |
| II Footprint | No JavaScript, framework or dependency; templates and CSS embedded; idle cost is a map of sessions. No new process or image. |
| III Test-first | Tests were written before the handlers and seen to fail (compile errors for the missing `UI`, then each page); golden HTML, a11y checklist, contrast, catalogue, route-contract and role-matrix tests. |
| IV Stable contracts | The JSON API and the events are unchanged; one additive migration; the UI route table is a contract file with a test. |
| V Secure and private by default | Deny by default through the Store; blind feedback, sealed fields behind an audited reveal, diversity data only as the service's aggregate, scoped 404s, CSRF, CSP, no-store, hashed session ids, failed-login throttle only, deadlines. Candidate data never leaves the module; the UI adds no store. |
| VI Open-source hygiene | Apache-2.0; no new dependency. |
| VII Simplicity and measured claims | Experimental, not browser- or cluster-verified; gaps listed in `tasks.md` and the PR. |

## Design decisions in brief

1. Session: server-side, in memory, SHA-256 of a 256-bit id; bounded (5000, 5 per subject).
2. The token is kept in memory to re-authenticate (1 minute for writers, 5 for the rest).
3. Redirect targets after POST are fixed local paths built from ids the service accepted; the one
   request-supplied path (`return`, from the board) must match `^/jobs/job_[0-9a-f]{16}$`.
4. Error pages share one template; a 404 never marks a section as current and carries only a random
   request id.
5. Authorisation errors: out-of-scope objects are `404` (the service says not found); a role that
   may not use a feature is `403` (the pages for it are not linked either).

## Testing

`go test -race ./core/jobs/...` including: login and session lifecycle, fixation, logout replay,
timeouts and revocation recheck, session cap, CSRF on every POST, scope for hiring managers,
interviewers and job-scoped tokens, role dashboards, reveal and its audit and rate limit, diversity
absent from candidate pages, suppression, resume headers, hostile names, moves and reasons,
reject and hire (event in the outbox), blind feedback including removed interviewers, self-unassign,
requisition lifecycle with stale-version conflict, approval, bounded board, talent pool, settings,
503 on deadline, login throttle without locking valid users, UI off by default and API/careers
unaffected, route contract, roles against the permission matrix, catalogue completeness, a11y
checklist, stylesheet contrast, goldens (`-update` rewrites them), no second authorisation path.
