# Data Model: Wiki, CRM and Status Page (Epic E10a)

## Manifests

| Field | wiki | crm | status |
|---|---|---|---|
| status marker | experimental | not-deployable | experimental |
| `provider` | oss-wrapper, DokuWiki, GPL-2.0-only, osi | oss-wrapper, EspoCRM, AGPL-3.0-only, osi | oss-wrapper, Uptime Kuma, MIT, osi |
| `runtime.subdomain` / `ui` | `wiki` / `wiki:8080` | `crm` / `crm:80` | `status` / `status:3001` |
| `requires.database` | none | mariadb | sqlite |
| `requires.capabilities` | none | email.send | none |
| `resources` ram / limit / disk MB (estimates) | 64 / 160 / 2048 | 350 / 640 / 5120 | 128 / 320 / 1024 |
| `provides` (planned) | wiki (wiki.v1) at wiki-adapter:8081 | crm (crm.v1) at crm-adapter:8081 | status (status.v1) at status-adapter:8081 |
| `consumes` / `subscribes` | none | leads / lead.created.v1 at /hooks/lead (optional, planned) | none |
| `permissions` | wiki.read, wiki.write, wiki.admin | crm.read, crm.write, crm.admin | status.read, status.write, status.admin |
| `roles` | reader, editor, admin | sales, viewer, admin | viewer, ops, admin |
| `data_classes` (all standard) | wiki-page 10y, wiki-revision 2y | crm-contact 3y, crm-activity 3y, crm-deal 7y | monitor-heartbeat 180d, incident-history 3y |
| `hooks` dump | `storage.tar.gz` (tar of /storage) | `database.sql.gz` (mariadb-dump) | `kuma.db.gz` (SQLite .backup) |
| `healthcheck` | /doku.php | / | / |

## Chart values (shared keys as in specs/007-starter-modules/data-model.md)

wiki adds `app{phpMemoryLimit,phpUploadLimit,timezone}`; crm adds `daemon{enabled,resources}` and
`probes.startup`; status adds `ingress.publicOnly` and `ingress.publicPaths[]`.

Pod requests: wiki app 56 + adapter 8 = 64; crm app 208 + daemon 96 + adapter 40 = 344 (manifest 350);
status app 112 + adapter 16 = 128.

## Capabilities

| Capability | Verbs (* = write) | Definitions |
|---|---|---|
| wiki.v1 | list_pages, get_page, search, put_page* | PageId, PageSummary, PageList, Page, SearchResults, PutPageRequest |
| crm.v1 | list_contacts, get_contact, upsert_contact*, list_deals, erase_contact* | Contact, ContactList, UpsertContactRequest, Deal, DealList, EraseContactRequest, EraseResult |
| status.v1 | summary, list_monitors, schedule_maintenance* | State, Summary, Monitor, MonitorList, MaintenanceRequest, Maintenance |

## Module Secrets

As the E07 starters: `can-database` (crm only), `can-smtp` (declared, optional, not consumed),
`can-module` (admin credentials for first-run bootstrap and the adapter's API URL and webhook secret).
EspoCRM reads the database and admin variables from them; DokuWiki and Uptime Kuma consume none yet.

## Dumps

`storage.tar.gz` (wiki), `database.sql.gz` (crm), `kuma.db.gz` (status), through
`/v1/dumps/<module>/<name>` as in specs/007-starter-modules/contracts/hooks.md (provisional path).
