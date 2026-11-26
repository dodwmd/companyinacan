# Epics

Each epic is one Spec Kit feature: `specify -> clarify -> plan -> tasks -> analyze -> implement`,
on its own feature branch, with its artifacts under `specs/NNN-<name>/`. `SPEC.md` stays the
source of truth; an epic's spec refines it and MUST NOT contradict it (Constitution I).

Supported clients are web and Android; macOS and iOS are out of scope for now (no Mac build hosts), and CI is Linux-only.

Epics are ordered by dependency. Phases refer to the roadmap in `SPEC.md` §9.

| # | Epic | Phase | Depends on | Status |
|---|---|---|---|---|
| E00 | Phase 0 measurements (k3s baseline, footprint harness, timing) | 0 | — | partial: CI-runnable part done (harness, k3s/forms/can-static numbers, NetworkPolicy and TokenReview proofs, findings); real 2 GB/4 GB VPS and `make up` timing blocked on a provider account — `specs/009-phase0-measurements/`, `docs/measurements/phase0-ci.md` |
| E01 | Foundation: repo layout, `can` CLI skeleton, schemas, CI | 1 | — | done — `specs/001-project-foundation/` |
| E02 | Local dev cluster (`make dev`, k3d provider, base manifests) | 1 | E01 | done — verified on a real k3d in CI (advisory job); ACME issuance, `externalTrafficPolicy: Local` and local-machine/Podman runs unverified — `specs/004-local-dev-cluster/` |
| E03 | can-core MVP (in-cluster control plane, module API, auth, audit) | 1 | E01, E02 | done for the platform core (part 1 + part 2: installer, admission policy, TokenReview cache, sandboxed chart renderer, k3d lifecycle test in advisory CI); can-core runs a shared Postgres on demand (E07b); MariaDB provisioning and upstream-Helm-chart support pending (admin enable/disable UI: E12b) (`email.send`: E03d) — `specs/005-can-core-mvp/` |
| E03c | Module wiring: charts, tokens, secrets, events and approvals for the in-house modules (forms, work, people, jobs, finance) | 1-3 | E03, E05, E11a, E13a, E14a, E15a, E16a | built offline and tested end to end with the real can-core services; `work` installed through can-core on k3d in the advisory CI job (passed once); since then every in-house module installs through can-core on k3d (`modules-k3d`, waits for Ready only; [status](../status.md#modules)); forms has no durable outbox (routing: E03e), finance wires invoices only — `specs/016-module-wiring/`, `docs/module-wiring.md` |
| E03e | Module routing: `modules.<id>.expose` puts a module web UI (work, people, jobs, finance) on `<id>.<domain>` or its API, forms is routed at `/_forms`, TLS, security headers, NetworkPolicy by routed port, Apps addresses derived, admission policy bounding which host a module may claim | 1-3 | E03c, E11c | built; unit and contract tested; k3d only in the advisory `module-routing` job (work and forms, self-signed certificate, no browser); DNS and the other gaps: E03f — `specs/040-module-routing/`, `docs/module-routing.md` |
| E03f | Public access: DNS records for module hosts in `can up`, `can status` and the wizard (Cloudflare creates them, never over a record it did not create), HTTP to HTTPS redirect, edge rate limits, client address behind Traefik, finance's invoice-link route, headers on chart routes, exposure on the module page | 1-3 | E03e, E12 | built; unit tested with fakes; k3d only in the advisory `module-routing` job; no real DNS, ACME, browser or ServiceLB client address — `specs/045-public-access/`, `docs/module-routing.md` |
| E03d | Outbound email: `email.send` on the module API over an operator SMTP relay (TLS required, credentials bound to the host), durable outbox with retries and dead letters, per-module recipient allow-lists and quotas, forms lead notification and the notification email channel, delivery page, `can up` credentials, development SMTP sink | 1 | E03, E05, E11a | built offline, experimental: tested against in-process fake relays and the compose sink; no real relay or cluster; SMTP credentials for third-party apps, DNS records and inbound mail not built — `specs/039-email-send/`, `docs/email.md` |
| E04 | Site renderer and static server | 1 | E01, E02 | done (`static` provider deploy and PVC wiring deferred) — `specs/002-site-renderer/` |
| E05 | Forms gateway and `leads` capability | 1 | E03, E04 | built: library, contract, chart and wiring tests; runs in the Docker Compose stack and its end-to-end suite (form to lead through the edge); installed through can-core on k3d in CI (`make-up-e2e`, `module-routing`, `module-backup`, `restore-e2e`), no durable event outbox; **E05b**: one lead can be found and erased, and a restore replays it (`specs/054-lead-erasure/`); a hosted form page needs no script — `specs/003-forms-gateway/` |
| E06 | Server providers and `make up` (ssh, Hetzner, sslip.io) | 1 | E02, E03 | built; unit tested against fakes; real-server checks pending (needs a provider account). **E06c**: `can verify --server` (read-only checks, opt-in probes, shareable report) and the ordered runbook [`real-server-verification.md`](../real-server-verification.md); tested with fakes and k3d only — `specs/006-providers-make-up/`, `specs/052-real-server-verification/` |
| E24 | `make up` deploys the company: platform base, images, can-core, admin account, site, routes, optional backup timer | 1 | E06 | built; deploy phases verified on k3d in CI; server creation and timing on a real server human-gated; ships can-core, can-static and the images of the enabled modules (E06d) — `specs/033-make-up-e2e/`, `docs/make-up.md` |
| E07 | Starter modules: support, newsletter, analytics | 1 | E03 | built offline (manifests, charts, hooks, contracts, email policy) — `specs/007-starter-modules/`; **E07b**: `analytics`, `support` and `newsletter` install from images pinned by digest on k3d (advisory CI job), experimental: no real server, domain or relay; adapters and inbound mail pending; backups: E08e — `specs/041-wrapped-modules-live/` |
| E08 | Backup, restore and recovery kit | 1 | E03, E06 | implemented; verified on k3d with a local restic repository only: dumps (E08b), restore (E08c), in-house module hooks (E08d), analytics, support and newsletter (E08e). wiki, crm and status still need the unpublished `can-hooks` image; can-core collects module erasures into the restore ledger (`specs/048-erasure-collection/`), except for `finance`, `work`, `provisioning` and the wrapped apps — `specs/008-backup-restore/`, `037-backup-dumps/`, `042-restore/`, `043-module-backup-hooks/`, `046-wrapped-backups/` |
| E23 | End-to-end compose tests in CI and documentation refresh | 1-3 | E03c, E13c, E14c, E16b | built: `test/e2e` (Go, build tag `e2e`) drives the compose stack through the Caddy edge (site and lead, admin login with TOTP, work, people, jobs, finance, equity, cross-module negatives, restart persistence); advisory `e2e` workflow passed on a real Docker Compose v5.1.3 (first real Compose v2+ verification of `docker-compose.yml`); README, getting-started, architecture, security-model and docs index rewritten, offline doc-link check. Personal access tokens, the module catalogue and Apps sign-in work in compose through can-core's development mode (issue #43). Gap: some people and equity identities come from a fake introspection stub — `specs/032-e2e-docs/` |

Backlog (broken down into epics when the previous phase is done):

| # | Epic | Phase |
|---|---|---|
| E10 | CRM, wiki, status page, simple tasks | 2 — **E10a (wrapped `wiki` DokuWiki, `crm` EspoCRM, `status` Uptime Kuma; chart, hooks, capability contracts, lead-to-CRM adapter design, `docs/modules-catalogue.md`) is built offline**: wiki and status experimental, crm not deployable; adapters, simple tasks (Vikunja) and all cluster verification pending: `specs/019-wiki-crm-status/` |
| E11 | SSO, RBAC, approval engine, notifications, webhooks, retention engine | 2 — **E11a (approvals, notifications, module-namespaced RBAC inside can-core) is built**; **E11b (OpenID Connect single sign-on for the admin UI with group to role mapping, `docs/sso.md`) is built offline against a fake provider**, no real IdP verified: `specs/021-oidc-sso/`; **E11c (single-use launch tickets from the admin UI into the module web UIs) is built, compose-tested**: `specs/036-module-ui-sso/`; **E11e (users and roles in the admin UI: one-time set-up links, disable and delete, role assignment and overrides, MFA reset, sessions and tokens, SSO identities, step-up) is built offline**: no browser, relay or cluster run, `specs/044-users-roles/`; retention engine pending: `specs/015-approvals-notifications/` |
| E12 | Admin dashboard and onboarding wizard; DigitalOcean provider; Cloudflare DNS | 2 — **E12a (server-rendered admin UI: login, dashboard, modules, audit, deliveries, leads, settings, PWA manifest) is built**; **E12b (module catalogue with footprint meter and requires/provides graph, generated settings forms, preview-then-apply enable/disable/upgrade with progress, separate purge flow, onboarding wizard, status page) is built offline against fakes, no real-cluster verification**: `specs/017-module-ui-wizard/`; **E12c (`digitalocean` provider with tag-based ownership, Cloudflare DNS plugin, `dns` block in company.yaml, validation) is built offline against `httptest` fakes, no real-account verification**: `specs/020-digitalocean-cloudflare/`; the `provider-native` DNS plugin (DigitalOcean domains API), DNS-01, and the purge executor, certificate and sslip.io readers pending: `specs/010-admin-ui/` |
| E13 | `files`, `directory`/`people`, `identity.provision`, hire/leave workflows | 3a |
| E14 | `work` tracker (API-first) and `performance` (1:1s, goals, reviews); timesheets. **E14a (in-house `work` service, API, CLI, contracts) is built offline**: image and cluster checks, `performance` and timesheets pending, see `specs/011-work-tracker/`; **E14c (server-rendered web UI inside can-work: sign-in with a pasted token, backlog with URL filters, issue page, button-driven board, sprint planning, epics, manager dashboard) is built offline and checked on the local compose stack**: launch-ticket sign-in (E11c) and routing (E03e) came later; manual accessibility pass pending, see `specs/023-work-ui/`, `docs/work-ui.md` | 3a |
| E13a | first slice of E13/E14: in-house `people` (directory, org, lifecycle, checklists) and `performance` (1:1s, goals), retention/erasure | 3a — built offline, experimental: `specs/012-people-directory/` |
| E14b | reviews and time off in the in-house `people` module: review cycles (self, manager, 360 feedback with anonymity thresholds, calibration, approved release), leave types and accruals as data, balances ledger, requests approved by the manager, holiday calendars, team calendar | 3a — built offline, experimental: `specs/022-reviews-timeoff/` |
| E13c | server-rendered employee and manager self-service web UI for the in-house `people` module: login with a personal access token and server-side session, home, profile and sensitive screen, directory and org chart, team, 1:1s, goals, time off with masked calendar, reviews and anonymous feedback; every page calls the same Service as the API | 3a — built offline, experimental, not browser- or cluster-verified: `specs/026-people-ui/`, `docs/people-ui.md` |
| E13d | in-house `provisioning` module and the `identity.provision` capability: a SCIM 2.0 server over the people directory (users read-mostly, groups, filter eq/and, pagination, PATCH, ETag, scoped tokens) and joiner, mover and leaver workflows with connectors (SCIM client, signed webhook; Google Workspace, GitHub, Slack are unverified stubs), effective-dated and urgent offboarding, approvals for privileged access, retries, dead letters, compensation, a persisted state machine, and a can-core kill switch (`POST /v1/identity/revoke`) limited to people the people module reported offboarded | 3a — built offline, experimental, nothing run against a real service or cluster; the module namespace NetworkPolicy and secret provisioning do not yet let it work on one: `specs/028-identity-provision/`, `docs/provisioning.md` |
| E13e | in-house `files` module and the `files` capability: spaces with user and group access lists (viewer, editor, manager) where what a caller may not see is a 404 identical to a missing id, immutable versions, upload checks (allow-list by extension and magic bytes, size, quota, counts, random storage keys, a fail-closed virus-scan hook with a clamd client), downloads always as attachments with the SHA-256 verified while streaming, signed expiring revocable one-time-optional share links with failure-only rate limiting, sealed names with key rotation, audit before return and in the same transaction, trash, retention, legal hold, erasure with tombstones and a WAL scrub, and local and S3 (SigV4) backends | 3a — built, experimental; since installed on k3d by `module-backup` (uploads are refused on an install until a scanner exists); no real S3 or ClamAV used; directory groups not resolved; blobs not encrypted at rest; the other modules do not call it yet: `specs/034-files/`, `docs/files.md` |
| E13f | web UI of the `files` module inside can-files: sign-in by launch ticket or token, spaces, members, folders, streamed upload with the scan verdict, download, versions, rename and move, trash, share links, legal hold, retention and erasure with step-up; no JavaScript, one authorisation path, routed with `expose.ui` | 3a — built; Go tests and the compose stack; headless Chrome only (E19c), UI not exercised on a cluster; no search: `specs/050-files-ui/`, `docs/files.md` |
| E15 | `invoices`, `contracts`, expenses, vendors, accounting/payroll adapters | 3b — in progress as slices E15a to E15d inside `finance`; no separate vendors module; no real accounting, payroll or payment provider |
| E15a | first slice of E15: in-house `finance` (parties, invoices and credit notes, payments, contracts, timesheets, expenses, purchase orders) | 3b — built offline, experimental: `specs/014-finance-basics/` |
| E15d | contract signing by emailed single-use link in the in-house `finance` module: external and company signers, optional countersign order, decline, frozen text hash, hash-chained evidence, completion certificate, `expose.signing` route. A **simple** electronic signature built in-house instead of the Documenso wrapper SPEC.md plans (decision and limits in the spec); the `documents` capability is still not provided | 3b — built offline, experimental, never on a cluster or a real relay: `specs/049-contract-esign/` |
| E15c | accounting interfaces for the in-house `finance` module: chart of accounts, double-entry journal posted from finance events (invoices, credit notes, payments, expenses) with immutable entries, reversals, manual journals with separation of duties, trial balance, general ledger, profit and loss, balance sheet, period close; bank accounts with sealed numbers, CSV/OFX/CAMT.053 statement import, suggestion-only matching with confirm/ignore and reconciliation per month; `AccountingExporter` (CSV; xero, quickbooks, wave are documented stubs) and `PayrollAdapter` (CSV import) seams; a tool, not accounting advice | 3b — built offline, experimental, fakes only, never on a cluster: `specs/035-accounting/`, `docs/accounting.md` |
| E15b | server-rendered finance web UI for the in-house `finance` module: login with a personal access token and server-side session, dashboard and approver queue, parties with an erase flow that shows legal retention, invoices (lines editor, approve with separation of duties, send, payments, void, credit notes, public link management, printable view, CSV export), recurring schedules, contracts from templates, timesheet grid, expenses with receipt metadata, purchase orders, aging and statement reports, read-only settings; every page calls the same Store as the API | 3b — built offline, experimental, not browser- or cluster-verified: `specs/031-finance-ui/`, `docs/finance-ui.md` |
| E16 | `jobs` (careers page, feeds, ATS, board connectors) | 3b — slices E16a and E16b built; board connectors are stubs; installs on k3d (`modules-k3d`) |
| E16a | first slice of E16: in-house `jobs` (requisitions, careers pages with JobPosting data, feeds, application intake, ATS pipeline, talent pool, consent/retention/erasure); board connectors are stubs (partner access needed) | 3b — built offline, experimental; QA follow-ups applied (EEO report publishes closed months only, scorecard submit needs an active assignment or staff role): `specs/013-jobs-recruiting/` |
| E16b | server-rendered recruiter, hiring-manager and interviewer web UI for the in-house `jobs` module: login with an access token and server-side session, requisitions with lifecycle and approval status, pipeline board with reasoned moves, application page with an audited reveal of candidate data and a safe resume download, blind interview feedback, talent pool, my interviews, admin diversity report (closed months, suppressed); every page calls the same Store as the API | 3b — built offline, experimental, not browser- or cluster-verified: `specs/027-jobs-ui/`, `docs/recruiting-ui.md` |
| E17 | `equity`, `board`, `entity`; investor and other portals; search, reporting, import | 3b — slices E17a to E17e built; portals and search not started; `equity`, `board`, `entity` install on k3d (`modules-k3d`) but have no web UI |
| E17a | first slice of E17: in-house `equity` (entities, share classes, plans and pools, stakeholders, option grants with vesting, exercises, share ledger, cap table, valuations, illustrative round modelling) | 3b — built offline, experimental; runs in the compose stack and its end-to-end suite (grant approval by a board identity from a fake introspection service); no cluster run: `specs/018-equity/` |
| E17b | second slice of E17: in-house `board` (bodies, meetings, quorum, resolutions by roll call, written consent or secret ballot, minutes, action items; the `resolutions` capability for equity's approval seam) and `entity` (legal entities, officers and directors, statutory registers with as-of views, filings calendar), one binary `can-board` | 3b — built offline, experimental: `specs/029-board-entity/`, `docs/board.md`, `docs/entity.md` |
| E17c | third slice of E17: CSV import of directory people, finance parties and work issues through each module's API and `can import`: preview, column mapping, per-row errors, all or nothing, idempotent on an external id, size and row limits. No web UI page, vendor presets or `import` module hook yet | 3b — built offline, experimental, not cluster-verified: `specs/047-csv-import/` |
| E17d | verified holder identity in `equity`: directory-checked person links, callers resolved through the people directory, second-actor confirmation of holders before grants, exercises and ledger writes, review report, break-glass; can-core wires directory mode since E03g | 3b — built offline, experimental, not cluster-verified: `specs/051-holder-identity/` |
| E17e | business overview in the admin UI: per-module aggregates and "needs attention" through the `summary` capability, asked on behalf of the signed-in person with single-use grants; E17f added jobs, equity and files figures and made the overview the landing page; no history or search | 3b — built offline, experimental, not cluster- or browser-verified: `specs/053-business-overview/` |
| E18 | Registry, more providers, hibernation, multi-company on one server | 3c |
| E19 | Android companion app (approvals, notifications, timesheets, 1:1s, work items) on the public API; PWA hardening for the web UI | 3a — **E19a (PWA hardening and Web Push: installable manifest with maskable icons and shortcuts, phone layouts, hashed static assets, a push-only service worker, a `webpush` notification channel with VAPID/RFC 8291 implemented on the standard library, device management with a test button) is built offline against fakes; nothing verified on a real browser, push service or Android device**: `specs/025-pwa-webpush/`, `docs/pwa.md`. Decision: PWA first; the native Kotlin app (E19b, timesheets, 1:1s, work items) starts only if PWA limits are proven |
| E19c | headless Chrome suite against the compose stack, desktop and phone viewport (admin UI, module web UIs, public forms, contract signing) | 1-3 — built, advisory `browser-e2e` job; no real phone, other browser engine or https: `specs/054-browser-e2e/`. Rows below that say "not browser-verified" predate it |
| E22 | Observability, health and diagnostics across can-core and every in-house service: `core/obs` (stdlib Prometheus exposition, process and footprint self-check metrics, HTTP and database metrics), `/readyz`, request ids and log redaction, `can diagnose`, `can doctor` reachability, `deploy/monitoring` alerts, dashboard and compose profile | 1-3 — built offline and verified against the compose stack only, no cluster, Prometheus and Grafana images not run: `specs/030-observability/`, `docs/observability.md` |

### Follow-ups

| Item | Why |
|---|---|
| Give `provisioning`'s connectors their credentials | Installed, it reads `people` but cannot provision anywhere ([status.md](../status.md#modules)) |
| Erasure feeds for `finance`, `work`, `provisioning` and the wrapped apps | What is erased there comes back with a restore of an older backup |
| Hold equity's switch to directory mode for approval | Enabling `people` where `equity` runs can freeze its actions (`specs/057-module-capability-wiring/`); today the plan only says so |
| Per-entry approval of manual `equity` ledger writes | One confirmation of a holder record covers every later entry to it (`specs/051-holder-identity/`) |

---

## E00 — Phase 0 measurements

**Goal.** Replace the guessed numbers in `SPEC.md` (§3.2, §3.4, §5) with measured ones, and settle
the open verification items, before anything depends on them.

**Scope.**
- Measure idle RAM of single-node k3s + Traefik + CoreDNS + local-path, with and without trimming
  flags (k3d locally; a real 2 GB and 4 GB VPS when an account is available).
- Footprint harness: a reusable script that deploys a chart on k3d and records idle RAM.
- Prove the NetworkPolicy default-deny + TokenReview module-auth design with two stub modules
  (including pod-to-node egress denial and the CoreDNS `coredns-custom` rewrite).
- Time `make up` end to end (needs a provider account: human-gated).
- Check licences and footprints of the wrapped candidates (FreeScout on Postgres?, Listmonk,
  GoatCounter/Umami, Plane, Kimai/Solidtime, Documenso, Snipe-IT, Invoice Ninja/Akaunting).
- Record results in `docs/measurements/` and update `SPEC.md`.

**Done when.** `docs/measurements/` holds dated results, and the spec numbers are corrected or
confirmed. Parts needing real servers or accounts are listed as blocked, not guessed.

## E01 — Foundation

**Goal.** A buildable, tested skeleton everything else plugs into.

**Scope.**
- Go module and layout per `SPEC.md` §8 (`core/`, `modules/`, `schemas/`, `deploy/`, `dev/`).
- `can` CLI skeleton (`init`, `doctor`, `apply` stubs, `version`) and the thin `Makefile`.
- JSON Schemas for `company.yaml` and `module.yaml` (including `permissions`, `approvals`,
  `data_classes`, `publishes`, `subscribes`, `provides`, `consumes`, `resources`, hooks) with
  validation and contract tests; example company repo with fake data.
- Licence check and `THIRD_PARTY_LICENSES` generation (fails on missing upstream licence).
- CI (GitHub Actions): lint, unit and contract tests, licence check.
- OSS hygiene files: `CONTRIBUTING`, `SECURITY.md`, `CODE_OF_CONDUCT`, DCO check, issue/PR templates.

**Done when.** `make test` passes in CI; `can init` writes a valid `company.yaml`; invalid
manifests are rejected with clear errors.

## E02 — Local dev cluster

**Goal.** `make dev` gives a working local k3s (k3d) company with the same charts as production.

**Scope.** `local` target type; k3d cluster config; base manifests in `deploy/` (namespaces,
default-deny NetworkPolicies and allows, Traefik config with persistent ACME store, CoreDNS
rewrite via `coredns-custom`); `make dev|down|doctor`; `*.localtest.me` hostnames.

**Done when.** `make dev` produces a cluster passing integration tests that assert the network
policies and DNS behaviour.

## E03 — can-core MVP

**Goal.** The in-cluster control plane.

**Scope.** Go service in `can-system`; SQLite state; admin login (password + TOTP); admin and
module-API listeners/Services; capability registry; TokenReview-based introspection; HMAC-signed
webhooks to modules; `email.send` (SMTP relay wrapper); audit log (hash-chained); Helm SDK
rendering and `helm upgrade --install --atomic` of module charts with values mapping; database
creation and credential injection; `can apply` reconciliation from the in-cluster `company.yaml`.

**Done when.** A stub module can be enabled, receives injected config, calls the module API with
its projected token, and is disabled again, all under integration test.

## E04 — Site renderer and static server

**Goal.** A company website from `company.yaml`, published in under a second.

**Scope.** Renderer library shared by the CLI and can-core; section-based templates (hero,
features, pricing, FAQ, CTA); SEO basics; output to a PVC served by a tiny static server pod in
`can-system`; `static` provider deploy path (Cloudflare Pages / GitHub Pages) as a stretch.

## E05 — Forms gateway and `leads`

**Goal.** Working lead capture.

**Scope.** In-house Go service as a module; same-origin `/_forms` route with priority over the
site and prefix stripping; honeypot, timing check, optional Turnstile/hCaptcha; SQLite storage;
`leads` capability and admin leads view; email notification via `email.send`; direct webhook
until the event bus exists; `lead.created.v1` event schema defined.

## E06 — Server providers and `make up`

**Goal.** One command to a live site on a real server.

**Scope.** `Provider` interface; `ssh` provider (bootstrap script: k3s install with pinned
version and trimmed flags, kubelet swap flag, host firewall incl. pod/service CIDR allows, image
pre-seeding); `hetzner` provider; cloud-init path; sslip.io instant URLs with the HTTP fallback;
DNS plugin interface with `manual` (prints records); `make up|down|status`; `can` reaches the
k3s API over an SSH tunnel.

## E07 — Starter modules

**Goal.** Support inbox, newsletter and analytics.

**Scope.** Module manifests and charts for FreeScout, Listmonk and GoatCounter (or Umami), with
resource declarations, probes, backup/restore hooks and licence entries; the email policy
(relay credentials injection, inbound IMAP/parse adapter); per-module footprint checks.

## E08 — Backup, restore and recovery kit

**Goal.** A restore from the recovery kit onto a blank host.

**Scope.** Dump endpoints (PUT/GET) and hostPath dumps directory; host systemd timer running
restic with completion-marker check; backup scope and PVC handling; recovery kit printed at
`make up`; `can restore --kit` implementing the six-step sequence in `SPEC.md` §6.5; erasure
tombstone ledger; `can backup verify`.
