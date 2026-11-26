# Plan: Public access (E03f)

## Design

- **One source for "what is public"**: `core/routing.Surfaces` (company.yaml `expose` plus the
  manifest). DNS lists, the wizard and the module page use it.
- **DNS**: `core/cli/dns_records.go` adds module hosts to `can up`'s records, marked `NoAdopt`; the
  Cloudflare plugin enforces FR-002. `can status` resolves each name (informational).
- **Redirect**: `core/launch/public.go`: Middleware and IngressRoute in `can-system` on `web`,
  applied in the routes phase on TLS, deleted otherwise.
- **Limits**: `core/routing/edge.go` holds the figures. can-core adds `can-login-*` and
  `can-public-*` Middlewares per module namespace; `can up` the same in `can-system`. The off switch
  travels as `edgeLimit` in `can-routing` (`can apply` takes it from `dns.proxied`).
- **Chart routes**: the installer adds Middleware references to rendered IngressRoutes, so the
  release record and rollback carry them. Middlewares are applied before the rollout and pruned only
  after it.
- **Invoice links**: manifest `runtime.public_link`; can-core route `can-link`; `public_base_url`
  and `trust_proxy` set while routed.
- **Client address**: manifest `public_config` (jobs careers) and `public_ui.config.trust_proxy`
  (people API, new `trust_proxy` setting); all services read the last `X-Forwarded-For` header line.

No new dependency, process, database migration or can-core RBAC verb. The admission policy is
unchanged: the new routes fit its rules.

## Upgrade behaviour

Every module is reconciled once; jobs and people pods restart. `can up` adds the redirect and the
admin and site limits; until it runs, an upgraded can-core already adds module-side limits (an
absent `edgeLimit` fact means on). finance publishes nothing until asked.
