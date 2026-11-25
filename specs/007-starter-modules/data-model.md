# Data Model: Starter Modules (Epic E07)

## Module manifest (`modules/<id>/module.yaml`, schema `module.v1`)

| Field | support | newsletter | analytics |
|---|---|---|---|
| `provider` | oss-wrapper, FreeScout, AGPL-3.0-only, osi | oss-wrapper, Listmonk, AGPL-3.0-only, osi | oss-wrapper, GoatCounter, EUPL-1.2, osi |
| `runtime.subdomain` / `ui` | `support` / `support:8080` | `newsletter` / `newsletter:9000` | `stats` / `analytics:8080` |
| `requires.database` | mariadb | postgres | sqlite |
| `requires.capabilities` | email.send | email.send | none |
| `resources` (ram / limit / disk MB, estimates) | 250 / 512 / 2048 | 50 / 128 / 1024 | 20 / 96 / 1024 |
| `provides` | tickets (tickets.v1) at support-adapter:8081 | newsletter (newsletter.v1) at newsletter-adapter:8081 | analytics (analytics.v1) at analytics-adapter:8081 |
| `consumes` | none | leads | none |
| `publishes` | ticket.created.v1 | subscriber.added.v1 | none |
| `subscribes` | none | lead.created.v1 (optional) at /hooks/lead | none |
| `permissions` | tickets.read, tickets.write, tickets.admin | newsletter.read, newsletter.write, newsletter.admin | analytics.read, analytics.admin |
| `roles` | agent, viewer, admin | marketing, viewer, admin | viewer, admin |
| `data_classes` (all standard) | support-conversation 3y, support-attachment 3y | newsletter-subscriber 3y, campaign-engagement 2y | pageview-aggregate 3y |
| `hooks` | image can-hooks, backup, restore | same | same |
| `healthcheck` | /login | /health | /status |

Invariants (tested): `ram_limit_mb >= ram_mb`; no `sensitive` class (only in-house modules may hold
sensitive data); `id` equals the directory; every capability name is in `schemas/names.yaml`;
`publishes.schema` is byte-identical to `schemas/events/<event>.json`.

## Chart values contract (`chart/values.yaml`)

Shared keys (all three charts): `image{repository,tag,digest,pullPolicy}`, `service.port`,
`ingress{enabled,host,tls,certResolver,adapterRoutes[]}`, `persistence{enabled,size,storageClass,mountPath}`,
`resources{requests,limits}`, `podSecurityContext`, `securityContext`, `secrets{database,smtp,module}`,
`probes`, `adapter{enabled,port,image,env,resources}`. Module-specific: support `app`, `email`,
`inbound`; newsletter `database.sslMode`, `migrateResources`; analytics `goatcounter.tlsMode`.

Rules: `ingress.host` is empty by default and required at render; `adapter.enabled` is false;
digests are 64-hex sha256 or the placeholder; app plus adapter requests and limits sum to the manifest figures.

## Module Secrets (created by can-core in the module namespace)

| Secret | Keys | Used by |
|---|---|---|
| `can-database` | `host port name user password` | support, newsletter, hook Jobs (analytics: unused) |
| `can-smtp` | `host port user password from encryption` | support, newsletter (optional refs) |
| `can-module` | `api-url webhook-secret admin-user admin-password admin-email` | app admin bootstrap, adapter |

## Capability contract (`schemas/capabilities/<name>.v<N>.json`)

JSON Schema 2020-12 document with `x-capability`, `x-verbs[]` (`verb`, `method`, `path`,
`request?`, `response?`, `write?`, `permission`, `description?`) and `$defs` for every named shape.
Rule: every non-GET verb carries `write: true`; every referenced definition exists and has a
valid fixture; fixtures are `testdata/capabilities/<name>.v<N>/<valid|invalid>-<Def>-<case>.json`.

| Capability | Verbs | Definitions |
|---|---|---|
| tickets.v1 | list, get, create*, update_status*, reassign* | Ticket, TicketList, CreateTicketRequest, UpdateStatusRequest, ReassignRequest, Customer |
| newsletter.v1 | list_lists, get_subscriber, subscribe*, unsubscribe*, erase* | ListSummary, ListCollection, Subscriber, SubscribeRequest (consent required), UnsubscribeRequest, Consent |
| analytics.v1 | snippet, summary, top_paths, register_site* | Snippet, Summary, PathStats, RegisterSiteRequest |

(* = write verb)

## Events (envelope as `lead.created.v1`: `id`, `type`, `time`, `source`, `data`)

- `ticket.created.v1`: source `support`; data `ticket_id (tk_<n>)`, `mailbox`, `subject`,
  `channel (email|web|api)`, `customer{email,name?,party_id?}`, `created_at`. No message body.
- `subscriber.added.v1`: source `newsletter`; data `subscriber_id (sb_<n>)`, `email`, `name?`,
  `lists[]` (1 to 50 unique slugs), `status (unconfirmed|confirmed)`, `origin (form|api|import|lead)`,
  `lead_id?`, `party_id?`, `added_at`.

## Dump

A named gzip file per module held by can-core in the host dumps directory:
`database.sql.gz` (support, newsletter), `goatcounter.sqlite3.gz` (analytics). Transport in
[contracts/hooks.md](contracts/hooks.md).
