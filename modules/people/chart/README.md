# people chart

Helm-compatible templates for the in-house people module, rendered by can-core's sandboxed renderer
(not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Port 8081 only (`directory`, `performance`), Service `people-api`, ClusterIP. Never routed: people
  data is not public.
- The sealed-field keyring is the `keyring` key of the `can-module` Secret (generated once by
  can-core), mounted as one file with `subPath` at `/etc/can-people/keys/keyring` because the loader
  refuses symlinks.
- Known gap: the loader (`core/people`) requires no group or other permission bits, but a mounted
  Secret is owned by root, so a non-root pod can only read it through `fsGroup` with group-readable
  mode. The chart mounts it `0440` with `fsGroup: 65532`; until the loader accepts a root-owned file
  that is group-readable by its own group, the pod does not start. This is not covered by any
  test here.
- `config:` is written to `people.yaml` (strict loader); `webhook.url` stays empty because events go
  to can-core's module API. Settings from `company.yaml` are not yet mapped onto `config:`.

## What is verified

Unit tests (`core/server/installer/modulecharts_test.go`, part of `make test`) install the chart
through the installer against the fake Kubernetes API and parse the rendered config with the module's
own loader. `make release-check` fails while `image.digest` is a placeholder. The chart has not been
run on a cluster.
