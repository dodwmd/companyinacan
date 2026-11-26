# finance chart

Helm-compatible templates for the in-house finance module, rendered by can-core's sandboxed renderer
(not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Port 8080 (`invoices`, `contracts`, `timesheets`), Service `finance`, ClusterIP.
- The chart makes no IngressRoute. can-core routes `/public/invoices/` (the signed, expiring invoice
  view) from `runtime.public_link` when company.yaml says `expose.public: true`
  (`docs/module-routing.md`).
- Secrets (`CAN_FINANCE_ADMIN_TOKEN`, `CAN_FINANCE_LINK_SECRET`, `CAN_FINANCE_SEAL_KEY`) come from the `can-module` Secret that
  can-core generates once and injects as environment.
- `config:` is written to `finance.yaml` (strict loader); no `webhook.url`, events go to can-core's
  module API. Settings from `company.yaml` are not yet mapped onto `config:`.

## What is verified

Unit tests (`core/server/installer/modulecharts_test.go`, part of `make test`) install the chart
through the installer against the fake Kubernetes API and parse the rendered config with the module's
own loader. `make release-check` fails while `image.digest` is a placeholder. The chart has not been
run on a cluster.
