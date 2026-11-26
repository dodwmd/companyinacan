# Plan: Module routing (E03e)

## Design

- **Setting**: `company.yaml` `modules.<id>.expose` (schema `company.v1`). `can validate` refuses
  `ui`/`api` for a module without `runtime.public_ui`.
- **Manifest**: `runtime.public_ui {service, port, exact, prefixes, config}` (work, people, jobs,
  finance) and `runtime.site_route {path, service, port}` (forms). `config` is merged into the
  chart's `config:` block while the UI is routed. `runtime.ui` (wrapped upstream apps) is unrelated
  and unchanged.
- **Who creates routes**: can-core (`core/server/installer/routes.go`), not the charts: one place for
  the host rule, headers and NetworkPolicy. Charts keep their own public routes. Objects: `can-web`,
  `<id>-public`, Middlewares `can-web-headers`, `can-site-headers`, `<id>-strip`. They are not part of
  the release record.
- **Order**: plan, render, check admission, namespace, secrets, remove unwanted routes, roll out,
  then NetworkPolicy and routes. Disable removes can-core's routes and closes the policy.
- **Routing facts**: ConfigMap `can-system/can-routing` (`domain`, `tls`, `siteHosts`, `catchAll`),
  written by `can up` (all four) and `can apply` (only when missing or for another domain). The
  admission policy takes it as its parameter, so it cannot be a can-core environment variable.
- **Reconcile**: the installer's routing fingerprint is part of the settings hash; exposure changes
  that alter chart configuration also annotate the pod template so pods restart.
- **Apps**: the reconciler records the routed host in its copy of the manifest; the ticket launcher
  uses it unless `CAN_MODULE_UI_URLS` names the module. No database migration.
- **forms**: `can up` no longer applies the route itself; it waits for can-core's.

## Constitution check

No new dependency, no new long-running process, no database migration. can-core gains no RBAC verb
beyond `get` on two named admission objects; `configmaps` in `can-system` it already had, and the new
policy takes writes to `can-routing` away.

## Upgrade behaviour

- Apply the new base (`can up` does) before or with the new can-core: routes need the two new
  policies, and can-core refuses to install a routed module without them. Run `can up` or `can apply`
  so `can-routing` exists; until it does, the policy denies every module route.
- Every module is reconciled once (the settings hash now includes routing).
- A route made **by hand** to a module UI stops working: the namespace now admits Traefik only on
  ports can-core knows about. Use `expose.ui`.
