# forms chart

Helm-compatible templates for the in-house forms gateway, rendered by can-core's sandboxed renderer
(not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Ports: public 8080 (the site's same-origin `/_forms` route, reached through Traefik, not through a
  module host) and API 8081 (`leads`, ClusterIP only). Readiness and liveness use the public port.
- `forms` declares no `runtime.subdomain`, so the chart renders no IngressRoute; can-core creates
  the `/_forms` route (`runtime.site_route`).
- `values.yaml` `config:` is written to `forms.yaml` (strict loader). It ships one sample `contact`
  form because the loader requires at least one; replace it with the company's forms. No `webhook.url`:
  `lead.created.v1` goes to can-core's module API and from there to subscribers and notification rules.
- `CAN_FORMS_API_TOKEN` reads, changes and, since 0.2.0, **erases** leads: give it only to what may do that.
- Secrets (`CAN_FORMS_TOKEN_KEY`, `CAN_FORMS_API_TOKEN`, `CAN_FORMS_API_READ_TOKEN`) come from the
  `can-module` Secret that can-core generates once and injects as environment.
- Settings from `company.yaml` are laid over `config:`
  ([docs/module-wiring.md](../../../docs/module-wiring.md#settings)).

## What is verified

Unit tests (`core/server/installer/modulecharts_test.go`, part of `make test`) install the chart
through the installer against the fake Kubernetes API and parse the rendered config with the module's
own loader. `make release-check` fails while `image.digest` is a placeholder. The chart has not been
run on a cluster.
