# Feature Specification: Wrapped modules that install (analytics, support, newsletter)

**Feature Branch**: `epic/E07b-wrapped-live`
**Created**: 2026-11-25
**Status**: Built; verified on k3d by the advisory `wrapped-modules` CI job and, for the images alone,
with local containers. Experimental: no real server, domain or mail relay.
**Input**: SPEC.md sections 4 and 5; epics E07 and E03 (`docs/epics/README.md`: "adapters, images and
cluster checks pending", "MariaDB provisioning and upstream-chart support pending");
[specs/007-starter-modules](../007-starter-modules/spec.md).

## Goal

The product promise names support, newsletter and analytics. Before this feature none of their charts
could run: images were placeholders, no database server existed, the charts read a Secret
(`can-database`) that can-core never wrote, and no admin account could be created. After it, enabling
`analytics`, `support` or `newsletter` in `company.yaml` gives a running app with its own database and
an administrator whose password was generated for that installation.

## User Scenarios

### US1 - Enable analytics (P1)
An operator enables `analytics`. A GoatCounter site for `stats.<domain>` exists before the server
starts, with a user `admin@<domain>` (setting `admin_email`) and a generated password. A page view sent
to `/count` is stored.

### US2 - Enable support (P1)
An operator enables `support`. can-core starts the shared Postgres if no module needed it before,
creates the database and user `mod_support`, and FreeScout migrates and seeds its first administrator.
The operator signs in and creates a mailbox. With a relay configured (E03d), FreeScout's system mail
settings use it.

### US3 - Enable newsletter (P2)
As US2 for Listmonk (`admin`, database `mod_newsletter`). SMTP is configured by hand.

### US4 - Disable (P1)
Disabling scales the module to zero and keeps its volume, Secrets and database.

## Requirements

- **FR-001** The three charts MUST name upstream or third-party images by tag and `sha256` digest, run
  under the `restricted` profile and keep their licences recorded (`THIRD_PARTY_LICENSES`).
- **FR-002** can-core MUST run a Postgres in `can-data` itself when a module requires `postgres` and no
  external server is configured (`CAN_POSTGRES_ADMIN_URL`), only from the first such module, with a
  generated superuser password, resource requests and limits, and an image pinned by digest.
- **FR-003** Each module MUST get its own database and role; one module's role MUST NOT be able to
  connect to another module's database.
- **FR-004** No app may start with a default password. can-core generates a password per module, once,
  of at most 50 bytes, in the Secret `mod-<id>/can-module`; the app's first administrator is created
  with it before the app serves requests.
- **FR-005** Public routing is the existing one: `Host(<subdomain>.<domain>)` to the module's Service,
  under the rules of [specs/040](../040-module-routing/spec.md): it needs the routing facts `can up`
  writes (ConfigMap `can-system/can-routing`), `modules.<id>.expose.public: false` removes it, and the
  ingress controller reaches the routed port only. Routing itself is not changed here.
- **FR-006** Probes MUST not report an app ready before its first-boot migrations are done.
- **FR-007** A module in the SMTP list (`support`) that declares `email.send` MUST receive the relay of
  [docs/email.md](../../docs/email.md) in the Secret `can-smtp` only when `company.yaml` says
  `email.modules.<id>.enabled: true` (default: not). Otherwise, and without a usable relay, the Secret is
  empty; opting out MUST be an upgrade that empties it. A relay without TLS is never handed out.
- **FR-008** An advisory CI job MUST install each module through can-core on k3d, load a page through
  Traefik, sign in with the generated password, make the app write to its database and read it back,
  record idle memory, disable it and delete its namespace.
- **FR-009** Memory figures in manifests MUST be based on a measurement, recorded in
  [research.md](research.md) with how it was taken.

- **FR-010** Each module role MUST have a connection limit (20); on the managed server a module role MUST
  NOT be able to connect to `postgres` or `template1`; the database ports in `can-data` MUST be open only
  to can-core and to the namespaces of modules that require a database.
- **FR-011** An installation that used `CAN_POSTGRES_ADMIN_URL` MUST NOT get a managed server when the
  setting is lost: can-core refuses (marker Secret `can-data/can-postgres-external`, a `postgres`
  Service it did not make, or a module database recorded on another host).
- **FR-012** The starter template MUST NOT contain a module whose backup hook cannot run, and applying a
  change that enables one MUST need an explicit confirmation naming it (wizard review and change page).
- **FR-013** The managed Postgres's data directory MUST be left out of the file backup, and the
  documents and the enable screens MUST say that Postgres is not backed up.
- **FR-014** Third-party packaged and infrastructure images MUST be listed in `THIRD_PARTY_LICENSES`.

- **FR-015** After a failed upgrade, the next apply MUST upgrade the module again even if `company.yaml`
  was reverted to what is recorded (the failed upgrade may already have removed routes or rewritten
  Secrets).

## Success Criteria

- **SC-001** `make test` passes.
- **SC-002** The `wrapped-modules` job passes on the pull request.
- **SC-003** Idle working set of each app is within its manifest's `ram_mb`.

## Out of scope

Capability adapters (`tickets`, `newsletter`, `analytics`); inbound mail; backups (PR #49 and the
`can-hooks` image); module routing; MariaDB (nothing here needs it: `crm` still does); upstream Helm
charts; upgrades between app versions; the tracking snippet in the site renderer; single sign-on.

## Edge cases and consequences

- **Backups.** With the dump runner of E08b, a module whose backup hook cannot run makes every dump run
  fail, so that nothing is backed up. All three modules name the unpublished `can-hooks` image: until it
  is published, enabling any of them stops backups for the whole company. Do not enable them where data
  matters. The managed Postgres has no backup of its own either.
- The admin password is read with
  `kubectl -n mod-<id> get secret can-module -o jsonpath='{.data.CAN_<ID>_ADMIN_PASSWORD}' | base64 -d`.
  It is also in the app container's environment. Changing it in the app does not update the Secret.
- Mail that FreeScout sends itself bypasses `email.send`: no recipient allow-list, quota or delivery
  log. Hence the opt-in.
- A new password in the relay Secret reaches the module only at its next install or upgrade. A Secret
  written less than a minute or two before the apply may not be mounted yet: the module is then given
  nothing until its next upgrade (seen in CI).
- Opting out empties `can-smtp` but does not clear what FreeScout stored in its database: clear its mail
  and mailbox settings by hand and rotate the relay password.
- While opted in with a relay, FreeScout's system mail settings are overwritten at every pod start.
- **Starter template.** SPEC.md section 1 promises a support inbox right after `make up`. The wizard's
  starter template no longer selects `support`: enabling it would stop every backup, forms' included. It
  returns when the hooks image is published.
- **Image patch lag.** Every image here is pinned by digest and nothing updates them: no bot reads chart
  values or `deploy/templates`. A security fix needs a maintainer to bump tag and digest (and the module's
  `version`, or installed modules are not upgraded), a can-core release and an upgrade by the operator.
  The Postgres image is re-applied at the next install or upgrade of a module that uses it. The PikaPods
  build adds a third party's delay. A Postgres major upgrade is not handled.
- The shared Postgres stays running after the last module using it is disabled. If the Secret
  `can-data/can-postgres` is lost while the volume remains, the password must be reset by hand.
- Database modules can reach each other's server port (it is one server); credentials and the per-database
  `CONNECT` grants are the barrier. Modules without a database cannot reach it.
