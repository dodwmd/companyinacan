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
| `people` | in-house | experimental | 30 | Apache-2.0 | `directory`, `performance` | Unit, race and contract tests offline; 13.8 MiB idle measured locally. No Helm templates, no admin UI, no cluster run |
| `work` | in-house | experimental | 20 | Apache-2.0 | `work`, `projects` | Tests and local measurement offline. No web UI, image not built or run on a cluster |
| `jobs` | in-house | experimental | 30 | Apache-2.0 | `jobs`, `ats` | Tests and local measurement (16 MiB idle) offline. No Helm templates, no recruiter UI, board connectors are stubs |
| `finance` | in-house | experimental | 20 | Apache-2.0 | `invoices`, `contracts`, `timesheets` | Tests offline (16 MB measured idle per modules/README.md). Not run on a cluster |

## Wrapped open-source modules

The chart of each wrapped module is checked offline: manifest validation, `helm template` assertions
(restricted pod security, ClusterIP, one PVC, same-namespace NetworkPolicy shape, `Host(...)` route),
the installer's allowlist through its fakes, hook scripts parsed with `sh -n`, capability fixtures.
**None has been pulled, started or measured**; images carry `REPLACE_WITH_DIGEST` placeholders and
`make release-check` fails until they are resolved. Every adapter is `planned`.

| Module | Wraps | Status | RAM (MB) | Licence | Capability | Notes |
|---|---|---|---|---|---|---|
| `support` | FreeScout | not-deployable | 250 | AGPL-3.0-only | `tickets` | Community image needs root; tag unresolved; MariaDB |
| `newsletter` | Listmonk | experimental | 50 | AGPL-3.0-only | `newsletter` | Postgres; SMTP relay settings manual until the adapter exists |
| `analytics` | GoatCounter | experimental | 20 | EUPL-1.2 | `analytics` | SQLite; first site created by hand |
| `wiki` | DokuWiki | experimental | 64 | GPL-2.0-only | `wiki` | No database (flat files); upstream documents a non-root uid; "only" vs "or later" unverified; heavier alternative BookStack (MIT) |
| `crm` | EspoCRM | not-deployable | 350 | AGPL-3.0-only | `crm` | MariaDB; official image serves port 80 and names no non-root user; `lead.created.v1` subscription planned; personal data, erasure documented |
| `status` | Uptime Kuma | experimental | 128 | MIT | `status` | SQLite; rootless image tag seen on Docker Hub, not run; only public status paths are routed, admin not exposed |

## What would make a row `ready`

1. The image runs on k3s under the `restricted` profile (pulled by digest, probes pass).
2. Idle memory measured for ten minutes settled, within 1.2x of the manifest figure.
3. Backup and restore hooks exercised against real data and a restore verified.
4. The adapter for its capability built, or the capability accepted as not offered.

Open verification work is tracked per spec: `specs/007-starter-modules/tasks.md` (Phase 7) and
`specs/019-wiki-crm-status/tasks.md` (Phase 7).

## Not modules yet

Simple tasks (Vikunja) from Epic E10, SSO, the approval engine and notifications (E11; the latter two
are built inside can-core, not as modules), the admin dashboard (E12), booking, e-sign and equity are
not in `modules/`.
