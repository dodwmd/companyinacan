# Feature Specification: Module management and onboarding wizard in the admin UI (Epic E12b)

**Feature Branch**: `epic/E12b-module-ui-wizard` | **Created**: 2026-11-25 | **Status**: built offline; no real-cluster verification
**Refines**: `SPEC.md` section 6 (admin UI, onboarding wizard, `make up` flow, email policy, DNS and ACME), section 3.2 (footprint budget), section 5 (catalogue), `docs/epics/README.md` E12; builds on `specs/010-admin-ui` (E12a), `specs/005-can-core-mvp` (reconcile, installer, registry) and `specs/015-approvals-notifications` (RBAC).

## Summary

E12a gave can-core a read-mostly admin UI. This epic lets an operator run the company from the browser:
see every module that exists, choose which to run and whether they fit the server, change their settings,
apply the change through the existing reconcile and installer path while watching its progress, and
walk through first-login onboarding. `company.yaml` stays the single source of truth; the UI edits the
in-cluster copy, shows the diff first, and records who did what in the audit log.

## User scenarios

### US1. See the catalogue and whether it fits (P1)

The operator opens **Catalogue** and sees every manifest in `CAN_MODULES_DIR`: status badge (`ready`,
`experimental`, `not-deployable`), licence and a source-available warning, declared RAM request and limit,
what the module requires and provides (a *planned* provision is shown as "not yet served"), conflicts
(an exclusive capability served by two modules without a primary), and, against the server size declared in
`company.yaml`, the total with a verdict (fits, tight, too big, or unknown when no size is declared).

- **Given** web enabled and a server of 2048 MB, **then** the page shows platform 650 MB + module RAM and "Fits".
- **Given** no declared server size, **then** the verdict is "Not declared", never "Fits".
- **Given** a manifest with `<script>` in its name, **then** the page shows it escaped.

### US2. Change a module's settings from a generated form (P1)

Each module's `settings.schema.json` becomes a form for string, integer, boolean, enum and secret-ref
properties. The server validates with the same JSON Schema (the browser's attributes are only hints);
properties the form cannot show (arrays, objects) are kept untouched and listed. A change is recorded
as a `company.yaml` patch and previewed before it is applied. A secret-ref holds the *name* of a Secret;
no secret value is ever shown or stored in `company.yaml`.

### US3. Enable, disable and upgrade with a plan preview and progress (P1)

The operator ticks modules and presses **Preview**. The page shows the `company.yaml` diff, what the
reconcile plan will do, what is created per module (namespace, Secrets, volumes, ingress host,
database) and, for a disable, what is kept (namespace, volumes, Secrets, database). **Apply** writes
`company.yaml` (optimistic concurrency) and runs the reconcile; a progress page reloads itself
(`<meta http-equiv="refresh">`, no JavaScript) and shows per-module results and a sanitised last error.

- Enabling a not-deployable module, or a source-available or experimental one without its own
  acknowledgement, is refused by the server.
- A change prepared against an older `company.yaml` is refused (409), and a second apply while one runs gets 503.
- Applying the same change twice does the work once.

### US4. Destroy a module's data, separately (P2)

Disable never deletes data. Purging a disabled module needs a different permission (`platform.purge`,
owner only by default), the exact typed text `purge <id>`, and the operator's current password; the intent is
audited before anything is touched. The executor is an interface; this build ships none (see gaps).

### US5. Onboarding wizard (P1)

At first login the owner is taken to a wizard: company name and domain and server size; a template
(starter bundle or blank; since [specs/041](../041-wrapped-modules-live/spec.md) the starter bundle is forms
only); modules with the footprint meter; email relay settings (SMTP credentials go
into a Secret and are never echoed); the DNS records to create, with the sslip.io instant URL and its
status; admin account hardening (rotate password, replace the authentication secret, a pointer to
`can backup kit`); review and apply. Answers are saved per step (resumable) and applying is idempotent.

### US6. Health and status (P2)

A status page shows module readiness, the last reconcile result, certificate and ingress status, and disk
usage. Sources that are not configured or fail are shown as unavailable, never as healthy.

## Requirements

- **FR-001** Every new route is permission-gated (`platform.view` read, `platform.operate` change,
  `platform.setup` wizard, `platform.purge` purge); a test enumerates the routes and fails for an ungated one.
- **FR-002** Every POST carries the session CSRF token and is refused cross-site (existing middleware, retested for the new routes).
- **FR-003** Sensitive changes are audited before and after: the intent is appended before `company.yaml` is written and the write is refused if the append fails.
- **FR-004** All errors fail closed: an RBAC lookup error is a 500, an unreadable manifest is listed as invalid and cannot be enabled, an unavailable status source is shown as unavailable.
- **FR-005** Database work is bounded: at most 50 stored proposals, 200 catalogue modules, 256 KiB documents and manifests, 64 KiB patches and schemas, 1500-line diffs.
- **FR-006** Output is escaped (`html/template`); hostile manifest strings, schema descriptions, submitted values and error texts are tested.
- **FR-007** Valid credentials are never throttled by this feature; the password re-check of the purge flow uses the existing per-account throttle, which only counts failures.
- **FR-008** `company.yaml` writes use optimistic concurrency (a version per stored document) and keep comments and untouched keys.
- **FR-009** The apply runs through the existing `reconcile.Reconciler` and `ModuleInstaller`; no new installer path.
- **FR-010** Pages follow E12a conventions: server-rendered, no JavaScript, strict CSP, message catalogue, golden HTML, static accessibility checklist.
- **FR-011** The periodic reconcile reads the same store the UI writes, so a UI change is never reverted by a stale mounted file.

## Out of scope and known gaps

- **No real-cluster verification.** Everything below the interfaces (kube API, Helm, certificates, disks, ACME, sslip.io) is exercised with fakes only. Nothing here ran against k3s.
- No purge executor is wired: `Purger` is an interface with a fake. In this build the purge page says destroying data is not available.
- No certificate/ingress reader is wired (`IngressStatus`); the status page shows it as unavailable. Disk usage reads the can-core data volume only (not module PVCs).
- The sslip.io probe is an interface; the build does not make the outbound request, so the instant URL shows "Not checked".
- The email relay is written to the Secret `can-system/can-smtp-relay`; the installer does not yet copy it to module namespaces as `can-smtp` (E07 email policy follow-up).
- Pod readiness is the recorded module state unless a `ReadyProbe` is provided (none is).
- Module management needs `CAN_INSTALLER=kubernetes` (or a test installer); with `none` its pages answer 503.
- Provider changes, DigitalOcean and Cloudflare DNS (the rest of E12) are not part of this slice.
- Screenshots and a manual screen-reader pass were not done.
