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
| E03 | can-core MVP (in-cluster control plane, module API, auth, audit) | 1 | E01, E02 | done for the platform core (part 1 + part 2: installer, admission policy, TokenReview cache, sandboxed chart renderer, k3d lifecycle test in advisory CI); `email.send`, MariaDB provisioning, upstream-Helm-chart support and admin enable/disable UI pending — `specs/005-can-core-mvp/` |
| E03c | Module wiring: charts, tokens, secrets, events and approvals for the in-house modules (forms, work, people, jobs, finance) | 1-3 | E03, E05, E11a, E13a, E14a, E15a, E16a | built offline and tested end to end with the real can-core services; `work` installed through can-core on k3d in the advisory CI job (passed once); the other four never on a cluster; forms has no public route and no durable outbox, finance wires invoices only — `specs/016-module-wiring/`, `docs/module-wiring.md` |
| E04 | Site renderer and static server | 1 | E01, E02 | done (`static` provider deploy and PVC wiring deferred) — `specs/002-site-renderer/` |
| E05 | Forms gateway and `leads` capability | 1 | E03, E04 | partial: library and contract (integration pending E03/E06) — `specs/003-forms-gateway/` |
| E06 | Server providers and `make up` (ssh, Hetzner, sslip.io) | 1 | E02, E03 | built offline; real-server checks pending |
| E07 | Starter modules: support, newsletter, analytics | 1 | E03 | built offline (manifests, charts, hooks, contracts, email policy); adapters, images and cluster checks pending — `specs/007-starter-modules/` |
| E08 | Backup, restore and recovery kit | 1 | E03, E06 | partly implemented (offline part, `core/backup`; see specs/008-backup-restore) |

Backlog (broken down into epics when the previous phase is done):

| # | Epic | Phase |
|---|---|---|
| E10 | CRM, wiki, status page, simple tasks | 2 — **E10a (wrapped `wiki` DokuWiki, `crm` EspoCRM, `status` Uptime Kuma; chart, hooks, capability contracts, lead-to-CRM adapter design, `docs/modules-catalogue.md`) is built offline**: wiki and status experimental, crm not deployable; adapters, simple tasks (Vikunja) and all cluster verification pending: `specs/019-wiki-crm-status/` |
| E11 | SSO, RBAC, approval engine, notifications, webhooks, retention engine | 2 — **E11a (approvals, notifications, module-namespaced RBAC inside can-core) is built**; **E11b (OpenID Connect single sign-on for the admin UI with group to role mapping, `docs/sso.md`) is built offline against a fake provider**, no real IdP verified: `specs/021-oidc-sso/`; retention engine and role-override UI pending: `specs/015-approvals-notifications/` |
| E12 | Admin dashboard and onboarding wizard; DigitalOcean provider; Cloudflare DNS | 2 — **E12a (server-rendered admin UI: login, dashboard, modules, audit, deliveries, leads, settings, PWA manifest) is built**; **E12b (module catalogue with footprint meter and requires/provides graph, generated settings forms, preview-then-apply enable/disable/upgrade with progress, separate purge flow, onboarding wizard, status page) is built offline against fakes, no real-cluster verification**: `specs/017-module-ui-wizard/`; **E12c (`digitalocean` provider with tag-based ownership, Cloudflare DNS plugin, `dns` block in company.yaml, validation) is built offline against `httptest` fakes, no real-account verification**: `specs/020-digitalocean-cloudflare/`; the `provider-native` DNS plugin (DigitalOcean domains API), DNS-01, and the purge executor, certificate and sslip.io readers pending: `specs/010-admin-ui/` |
| E13 | `files`, `directory`/`people`, `identity.provision`, hire/leave workflows | 3a |
| E14 | `work` tracker (API-first) and `performance` (1:1s, goals, reviews); timesheets. **E14a (in-house `work` service, API, CLI, contracts) is built offline**: image and cluster checks, `performance` and timesheets pending, see `specs/011-work-tracker/`; **E14c (server-rendered web UI inside can-work: sign-in with a pasted token, backlog with URL filters, issue page, button-driven board, sprint planning, epics, manager dashboard) is built offline and checked on the local compose stack**: SSO sign-in, a public route, manual accessibility pass pending, see `specs/023-work-ui/`, `docs/work-ui.md` | 3a |
| E13a | first slice of E13/E14: in-house `people` (directory, org, lifecycle, checklists) and `performance` (1:1s, goals), retention/erasure | 3a — built offline, experimental: `specs/012-people-directory/` |
| E14b | reviews and time off in the in-house `people` module: review cycles (self, manager, 360 feedback with anonymity thresholds, calibration, approved release), leave types and accruals as data, balances ledger, requests approved by the manager, holiday calendars, team calendar | 3a — built offline, experimental: `specs/022-reviews-timeoff/` |
| E13c | server-rendered employee and manager self-service web UI for the in-house `people` module: login with a personal access token and server-side session, home, profile and sensitive screen, directory and org chart, team, 1:1s, goals, time off with masked calendar, reviews and anonymous feedback; every page calls the same Service as the API | 3a — built offline, experimental, not browser- or cluster-verified: `specs/026-people-ui/`, `docs/people-ui.md` |
| E15 | `invoices`, `contracts`, expenses, vendors, accounting/payroll adapters | 3b |
| E15a | first slice of E15: in-house `finance` (parties, invoices and credit notes, payments, contracts, timesheets, expenses, purchase orders) | 3b — built offline, experimental: `specs/014-finance-basics/` |
| E16 | `jobs` (careers page, feeds, ATS, board connectors) | 3b |
| E16a | first slice of E16: in-house `jobs` (requisitions, careers pages with JobPosting data, feeds, application intake, ATS pipeline, talent pool, consent/retention/erasure); board connectors are stubs (partner access needed) | 3b — built offline, experimental; QA follow-ups applied (EEO report publishes closed months only, scorecard submit needs an active assignment or staff role): `specs/013-jobs-recruiting/` |
| E16b | server-rendered recruiter, hiring-manager and interviewer web UI for the in-house `jobs` module: login with an access token and server-side session, requisitions with lifecycle and approval status, pipeline board with reasoned moves, application page with an audited reveal of candidate data and a safe resume download, blind interview feedback, talent pool, my interviews, admin diversity report (closed months, suppressed); every page calls the same Store as the API | 3b — built offline, experimental, not browser- or cluster-verified: `specs/027-jobs-ui/`, `docs/recruiting-ui.md` |
| E17 | `equity`, `board`, `entity`; investor and other portals; search, reporting, import | 3b |
| E17a | first slice of E17: in-house `equity` (entities, share classes, plans and pools, stakeholders, option grants with vesting, exercises, share ledger, cap table, valuations, illustrative round modelling) | 3b — built offline, experimental: `specs/018-equity/` |
| E18 | Registry, more providers, hibernation, multi-company on one server | 3c |
| E19 | Android companion app (approvals, notifications, timesheets, 1:1s, work items) on the public API; PWA hardening for the web UI | 3a — **E19a (PWA hardening and Web Push: installable manifest with maskable icons and shortcuts, phone layouts, hashed static assets, a push-only service worker, a `webpush` notification channel with VAPID/RFC 8291 implemented on the standard library, device management with a test button) is built offline against fakes; nothing verified on a real browser, push service or Android device**: `specs/025-pwa-webpush/`, `docs/pwa.md`. Decision: PWA first; the native Kotlin app (E19b, timesheets, 1:1s, work items) starts only if PWA limits are proven |

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
