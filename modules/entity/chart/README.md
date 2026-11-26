# entity chart

Helm-compatible templates for the in-house entity module, rendered by can-core's sandboxed renderer
(not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Port 8082 only, Service `board-api`, ClusterIP, no route.
- The sealed-field keyring is the `keyring` key of the `can-module` Secret, mounted `0440` with
  `fsGroup: 65532` as one file with `subPath`.
- Events stay in the service's outbox: this service has no can-core client yet.
- `board` and `entity` are one binary, but can-core installs them as **two instances**, each with its
  own database and keyring. A rule that needs both kinds of record in one database (a director's
  access to an entity through board membership) therefore finds nothing and denies.

## What is verified

Unit tests install the chart through the installer against the fake Kubernetes API and parse the
rendered config with the module's own loader. The `modules-k3d` workflow installs the module through
can-core on a throw-away k3d cluster and waits for it to become Ready; that is all it proves.
