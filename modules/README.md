# modules

One directory per module: `module.yaml`, a Helm chart, hooks, and settings schema (see SPEC.md §4).
Directories starting with `_` are examples and are not enabled or listed in THIRD_PARTY_LICENSES.

| Module | Wraps | Licence | Idle RAM (estimate) | Database | Capability |
|---|---|---|---|---|---|
| `forms` | in-house | Apache-2.0 | 15 MB | SQLite | `leads` |
| `support` | FreeScout | AGPL-3.0 | 250 MB | MariaDB | `tickets` |
| `newsletter` | Listmonk | AGPL-3.0 | 50 MB | Postgres | `newsletter` |
| `analytics` | GoatCounter | EUPL-1.2 | 20 MB | SQLite | `analytics` |

## Licences and network copyleft

Wrapped apps keep their own licences; the manifests, charts and hooks in this repository are
Apache-2.0. FreeScout and Listmonk are **AGPL-3.0** and GoatCounter is **EUPL-1.2**; both are
*network copyleft* licences: the obligation to offer source applies to users who interact with
the program over a network, not only to people who receive copies.

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
