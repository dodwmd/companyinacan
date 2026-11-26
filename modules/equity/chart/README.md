# equity chart

Helm-compatible templates for the in-house equity module, rendered by can-core's sandboxed renderer
(not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Port 8081 only, Service `equity-api`, ClusterIP. No route of its own: with `modules.equity.expose.ui`
  can-core routes the web UI's paths (never `/v1`) on `equity.<domain>` and sets `config.ui`.
- The sealed-field keyring is the `keyring` key of the `can-module` Secret, mounted `0440` with
  `fsGroup: 65532` as one file with `subPath`.
- Events stay in the service's outbox: this service has no can-core client yet.

## What is verified

Unit tests install the chart through the installer against the fake Kubernetes API and parse the
rendered config with the module's own loader. The `modules-k3d` workflow installs the module through
can-core on a throw-away k3d cluster and waits for it to become Ready; that is all it proves.
