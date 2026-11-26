# Module catalogue

Every module directory under `modules/`, with its status marker, declared memory, licence and what has
actually been verified. The first column must match `modules/<id>/module.yaml`; a contract test
(`TestModulesCatalogueCoversEveryModule`) fails when a module is missing or its licence or status here
disagrees with its manifest. RAM figures are the manifest's `ram_mb` (a request budget). Unless a row
says "measured", they are **estimates, not measurements** (constitution II and VII).

Status markers: `ready` (no marker), `experimental` (not verified on a cluster, or needs manual steps)
and `not-deployable` (known not to run as shipped). `can validate` warns for `experimental`; for
`not-deployable` it is an **error** (`semantic.module-not-deployable`) when the module is enabled in
`company.yaml`, so `can validate`, `can apply` and the reconciler all refuse it; there is no override.
A capability marked `planned` is declared but nothing serves it yet (adapter not built): it is not a
provider, not counted for subscription defaults (the default webhook port is the first served
provision's), not listed by the admin or module API and cannot be a `primary`.

## Core and in-house modules (Apache-2.0)

| Module | Kind | Status | RAM (MB) | Licence | Capabilities | Verified |
|---|---|---|---|---|---|---|
| `forms` | in-house | ready | 15 | Apache-2.0 | `leads` | Unit and contract tests offline; 13.9 MiB RSS measured locally on the static build. Not run on a cluster by these tests |
| `people` | in-house | experimental | 30 | Apache-2.0 | `directory`, `performance` | Unit, race and contract tests offline; 13.8 MiB idle measured locally; self-service web UI (E13c); runs in the compose stack and its e2e suite (two identities through a fake introspection service). No Helm templates for the UI route, no cluster run |
| `work` | in-house | experimental | 28 | Apache-2.0 | `work`, `projects` | Tests and local measurement offline; web UI (E14c); installed through can-core on k3d once in the advisory CI job; runs in the compose e2e suite |
| `jobs` | in-house | experimental | 30 | Apache-2.0 | `jobs`, `ats` | Tests and local measurement (16 MiB idle) offline; recruiter, hiring-manager and interviewer UI (E16b); runs in the compose e2e suite (careers page, public application, blind feedback). Board connectors are stubs, no cluster run |
| `finance` | in-house | experimental | 28 | Apache-2.0 | `invoices`, `contracts`, `timesheets` | Tests offline (16 MB measured idle per modules/README.md, before the UI); web UI (E15b); invoice flow with a second approver and the public link run in the compose e2e suite. Not run on a cluster |
| `provisioning` | in-house | experimental | 20 | Apache-2.0 | `identity.provision` | Unit, race and contract tests offline against fakes (a fake directory, a fake SCIM server); RAM measured locally. No connector has talked to a real service (Google Workspace, GitHub and Slack are stubs), the module namespace NetworkPolicy does not yet allow its egress, and nothing ran on a cluster |
| `files` | in-house | experimental | 20 | Apache-2.0 | `files` | Unit, race and contract tests offline; a fake S3 that recomputes SigV4 and a fake clamd; 12.9 MiB idle and 19.8 MiB peak measured locally while streaming 20 MB files; runs in the compose stack (podman) and its e2e scenario (j). No real S3, no ClamAV, no cluster; blobs not encrypted by the module; directory groups not resolved; no existing module calls it yet ([files.md](files.md)) |
| `equity` | in-house | experimental | 16 | Apache-2.0 | `equity` | Unit, race, fuzz and contract tests offline; 15.8 MiB idle measured locally; grant approval by a board identity and vesting run in the compose e2e suite (fake introspection). No Helm templates, no admin UI, no cluster run. A tool, not legal, tax or securities advice |
| `board` | in-house | experimental | 20 | Apache-2.0 | `board`, `resolutions` | Unit, race, fuzz and contract tests offline; 14.4 MiB idle measured locally (one binary with `entity`). No Helm templates, no admin UI, no cluster run, equity approval seam not wired. A tool, not legal advice |
| `entity` | in-house | experimental | 20 | Apache-2.0 | `entity` | Same binary and release as `board`; tested offline with it. Jurisdiction rules (registers, filings) are data, not implemented. A tool, not legal advice |

## Wrapped open-source modules

The chart of each wrapped module is checked offline: manifest validation, `helm template` assertions
(restricted pod security, ClusterIP, one PVC, same-namespace NetworkPolicy shape, `Host(...)` route),
the installer's allowlist through its fakes, hook scripts parsed with `sh -n`, capability fixtures.
`analytics`, `support` and `newsletter` are also installed through can-core on k3d by the advisory
`wrapped-modules` CI job, from upstream images pinned by digest ([specs/041](../specs/041-wrapped-modules-live/spec.md)).
`wiki`, `crm` and `status` have **never been pulled, started or measured**: their images carry
`REPLACE_WITH_DIGEST` placeholders. Every adapter is `planned`, and `make release-check` fails while
any placeholder remains. `analytics`, `support` and `newsletter` are backed up by can-core
([backup-restore.md](backup-restore.md#wrapped-modules)). **Enabling `wiki`, `crm` or `status` stops the
company's backups** until the `can-hooks` image is published.

| Module | Wraps | Status | RAM (MB) | Licence | Capability | Notes |
|---|---|---|---|---|---|---|
| `support` | FreeScout | experimental | 192 | AGPL-3.0-only | `tickets` | Third-party non-root image (PikaPods); Postgres; admin password generated; relay applied to system mail only, mailboxes (in and out) configured by hand; 63 to 86 MiB idle measured on k3d |
| `newsletter` | Listmonk | experimental | 50 | AGPL-3.0-only | `newsletter` | Upstream image; Postgres; admin password generated; SMTP settings manual; 26 to 29 MiB idle measured on k3d |
| `analytics` | GoatCounter | experimental | 64 | EUPL-1.2 | `analytics` | Upstream image; SQLite; first site and user created with a generated password; 25 to 35 MiB idle measured on k3d |
| `wiki` | DokuWiki | experimental | 64 | GPL-2.0-only | `wiki` | No database (flat files); upstream documents a non-root uid; "only" vs "or later" unverified; heavier alternative BookStack (MIT) |
| `crm` | EspoCRM | not-deployable | 350 | AGPL-3.0-only | `crm` | MariaDB; official image serves port 80 and names no non-root user; `lead.created.v1` subscription planned; personal data, erasure documented |
| `status` | Uptime Kuma | experimental | 128 | MIT | `status` | SQLite; rootless image tag seen on Docker Hub, not run; only public status paths are routed, admin not exposed |

## What would make a row `ready`

1. The image runs on k3s under the `restricted` profile (pulled by digest, probes pass).
2. Idle memory measured for ten minutes settled, within 1.2x of the manifest figure.
3. Backup and restore hooks exercised against real data and a restore verified.
4. The adapter for its capability built, or the capability accepted as not offered.

Open verification work is tracked per spec: `specs/041-wrapped-modules-live/tasks.md`,
`specs/007-starter-modules/tasks.md` (Phase 7) and `specs/019-wiki-crm-status/tasks.md` (Phase 7).

## Not modules yet

Simple tasks (Vikunja) from Epic E10, SSO, the approval engine and notifications (E11; the latter two
are built inside can-core, not as modules), the admin dashboard (E12), booking and e-sign are
not in `modules/`.
