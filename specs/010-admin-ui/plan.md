# Implementation Plan: Admin Web UI, first slice (Epic E12a)

**Branch**: `epic/E12a-admin-ui` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Add package `core/server/adminui` (server-rendered `html/template`, embedded CSS/icons/manifest, no
JavaScript) and mount it in can-core on the admin listener next to the JSON API. Add the small read and
account helpers the UI needs to `store`, `audit` and `auth` (new files only), and three environment
variables to `runtime`. Design choices: [research.md](research.md); entities: [data-model.md](data-model.md);
routes: [contracts/ui-routes.md](contracts/ui-routes.md).

## Technical Context

**Language/Version**: Go 1.27 (existing module). **New dependencies**: none.

**Storage**: none of its own; reads and writes the can-core SQLite store through existing services.

**Testing**: `go test` (handlers over `httptest` with the real `auth.Service` and an in-memory store;
fake `LeadsClient`; golden HTML for 24 page states; static accessibility checklist; stylesheet contrast
test; route contract test; catalogue test; asset budget test).

**Constraints**: no cluster, browser or container is used in tests; no JS; CSP `script-src 'none'`.

## Constitution Check

| Principle | Before research | After design |
|---|---|---|
| I. Spec-first | SPEC.md 6.3 and 6.7 are the source; this feature adds spec, contracts and `docs/admin-ui.md` in the same PR | Same. The only deviation from SPEC.md section 7 ("Server-rendered + HTMX") is *not shipping HTMX yet* (research R1); recorded here instead of amending section 7, which still holds as the direction |
| II. Footprint is a feature | No new resident component: the UI runs inside can-core. Assets embedded, under 100 KiB total (tested), no sidecar | Templates are parsed once at start (a few KiB of heap); no caches; page data is read per request. can-core's declared RAM is unchanged. Idle RAM is measured by the existing E03 CI gate, not claimed here |
| III. Test-first | Handler, golden, a11y and contract tests were written before the handlers and templates; backend helpers have unit tests first | Done; see tasks.md |
| IV. Stable module contracts | Leads are read only through the `leads` capability (`leads.v1`) resolved by the registry; no module internals | Only `status` is sent to the provider. No capability or event schema changed |
| V. Secure and private by default | Session + CSRF + `Sec-Fetch-Site`, strict CSP, escaped output, no event payloads, no PII in audit details, throttled credential changes, all security actions audited | Tested with hostile data. Residual risks in Known gaps |
| VI. Open-source hygiene | Apache-2.0; no new third-party code or licence entries; icons are generated artwork committed as binary files | Same |
| VII. Simplicity and measured claims | One package, one stylesheet, one catalogue. No SPA, no JS, no service worker. No performance or size claim beyond the tested asset budget | Same. No complexity to justify |

**Result**: pass, no violations to justify.

## Project Structure

```text
core/server/adminui/
  adminui.go        Server, routes, middleware (session, CSRF, headers), rendering, static assets
  pages.go          login, dashboard, modules, audit, events, leads, settings handlers
  leads.go          LeadsClient interface, HTTPLeads (registry + bearer token), FileToken
  messages.go       English message catalogue
  templates/*.html  layout + one template per page (go:embed)
  static/           admin.css, icon.svg, icon-192.png, icon-512.png, manifest.webmanifest (go:embed)
  testdata/golden/  rendered HTML of 24 page states
  *_test.go         handlers, golden, a11y + contrast, leads client, catalogue, asset budget
core/server/store/sessions.go, delivery_get.go     ListSessions, RevokeSession, GetDelivery
core/server/audit/query.go                         Filter + Query
core/server/auth/account.go                        ChangePassword, RotateTOTP, ConfirmPassword
core/server/runtime/runtime.go                     mounts the UI; CAN_COMPANY_*, CAN_LEADS_TOKEN_FILE
docs/admin-ui.md
```

## Known gaps (not done in this slice)

- **Not run in a real browser or against a cluster.** All verification is `httptest` and static checks.
  A manual pass with a screen reader, keyboard-only use, 200% zoom and Chrome on Android "Install app" is
  owed.
- **Leads credential.** `CAN_LEADS_TOKEN_FILE` has no producer yet: the forms gateway still authenticates
  with static tokens (E05) and the can-core chart does not mount a credential. Until then the leads page
  shows "not available". The E03 token-introspection path (`POST /v1/tokens/introspect`) is the intended
  replacement.
- **Company name and domain** come from environment variables, not the applied `company.yaml`
  ConfigMap; the chart must set them.
- **No JSON endpoints for the account operations** (change password, rotate TOTP, sessions) yet, so the
  Android app cannot use them; they will be added to `adminapi` and the OpenAPI file when E19 needs them.
- **No QR code** for the new TOTP secret (it would need an encoder; the secret and the setup link are
  shown instead).
- **Not in this slice**: module enable/disable and settings forms, onboarding wizard, backups, DNS,
  attention inbox, roles (single admin role), i18n beyond English, HTMX, service worker.
- 405 responses for a wrong method on a known path come from the Go mux (plain text), not the styled page.
