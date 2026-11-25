# modules

A status-by-status overview of every module is in [docs/modules-catalogue.md](../docs/modules-catalogue.md).

One directory per module: `module.yaml`, a Helm chart, hooks, and settings schema (see SPEC.md §4).
Directories starting with `_` are examples and are not enabled or listed in THIRD_PARTY_LICENSES.

| Module | Wraps | Licence | Idle RAM (estimate) | Database | Capability |
|---|---|---|---|---|---|
| `forms` | in-house | Apache-2.0 | 15 MB | SQLite | `leads` |
| `support` | FreeScout | AGPL-3.0 | 250 MB | MariaDB | `tickets` |
| `newsletter` | Listmonk | AGPL-3.0 | 50 MB | Postgres | `newsletter` |
| `analytics` | GoatCounter | EUPL-1.2 | 20 MB | SQLite | `analytics` |
| `wiki` | DokuWiki | GPL-2.0 | 64 MB | none (flat files) | `wiki` |
| `crm` | EspoCRM | AGPL-3.0 | 350 MB | MariaDB | `crm` |
| `status` | Uptime Kuma | MIT | 128 MB | SQLite | `status` |
| `finance` | in-house | Apache-2.0 | 20 MB (16 MB measured idle) | SQLite | `invoices`, `contracts`, `timesheets` |
| `people` | in-house | Apache-2.0 | 30 MB (14 MB measured idle) | SQLite | `directory`, `performance` |
| `jobs` | in-house | Apache-2.0 | 30 MB (16 MB measured idle) | SQLite | `jobs`, `ats` |
| `equity` | in-house | Apache-2.0 | 16 MB (15.8 MiB measured idle) | SQLite | `equity` |
| `files` | in-house | Apache-2.0 | 20 MB (12.9 MiB idle, 19.8 MiB peak measured) | SQLite plus a PVC or S3 | `files` |

## Licences and network copyleft

Wrapped apps keep their own licences; the manifests, charts and hooks in this repository are
Apache-2.0. FreeScout, Listmonk and EspoCRM are **AGPL-3.0** and GoatCounter is **EUPL-1.2**; both are
*network copyleft* licences, in different words:

- AGPL-3.0 section 13: if you **modify** the program and let users interact with it remotely
  over a network, you must offer those users the source of your modified version.
- EUPL-1.2 Article 1 defines "Distribution or Communication" to include providing online access
  to the work's essential functionalities, and Article 5 requires Derivative Works that are
  distributed or communicated to stay under the EUPL (or a compatible licence listed in its appendix).
  Running it unmodified creates no Derivative Work.

This is engineering guidance for this repository, not legal advice; check with counsel before
shipping a modified fork.

What that means here:

- We run these apps **unmodified**, as separate containers that talk to the rest of the platform
  over HTTP. That is aggregation, not a combined work, so the copyleft does not reach this
  repository's Apache-2.0 code, and the upstream source is already public.
- If a company **patches** one of them (including a patched image or a vendored fork in a chart),
  it must offer the corresponding source of its modified version to everyone who uses the running
  service, for example with a link in the app footer. Keep such patches in a public fork.
- Adapters and glue in this repository must stay separate processes that use only the public
  API of the wrapped app; linking AGPL/EUPL code into them would pull them under the same terms.
- AGPL-3.0 and EUPL-1.2 are on `schemas/accepted-licences.yaml`; EUPL-1.2 lists AGPL-3.0 as a
  compatible licence in its appendix. Anything else is `licence_class: source-available` and opt-in.
