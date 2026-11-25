# provisioning chart

Helm-compatible templates for the in-house provisioning module, rendered by can-core's sandboxed
renderer (not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Port 8090 (`identity.provision`), Service `provisioning`, ClusterIP, one pod, `Recreate` (SQLite).
- The IngressRoute is rendered only when the manifest declares `runtime.subdomain` (`scim`, host
  `scim.<domain>`, supplied by can-core). It routes only the prefix `/scim/v2`; the `/v1/provision`
  API and `/healthz` stay cluster-internal.
- `workflows.dry_run` is **true** in the shipped values: a first install records plans and calls no
  connector. Turn it off only after reading the plans (`POST /v1/provision/plans`).
- `config.connectors` is empty. Add connectors there; each needs a credential in an environment
  variable named by `token_env` or `secret_env`.

## Known gaps (not covered by any test here)

- **Credentials.** can-core generates `CAN_PROVISIONING_API_TOKEN` and `CAN_WEBHOOK_SECRET` into the
  `can-module` Secret. The directory token (`CAN_PROVISIONING_DIRECTORY_TOKEN`) and the connector
  credentials are not generated, and the installer refuses a reference to any other Secret, so there
  is no supported way yet to provide them.
- **Egress.** The module namespace allows egress to DNS, can-core, Traefik and the databases only. The
  module also needs the people module's API and its connectors' hosts.

## What is verified

Unit tests (`core/server/installer/modulecharts_test.go`, part of `make test`) install the chart
through the installer against the fake Kubernetes API, check the route is scoped to `/scim/v2`, and
parse the rendered config with the module's own loader. `make release-check` fails while
`image.digest` is a placeholder. The chart has not been run on a cluster.
