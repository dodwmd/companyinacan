# jobs chart

Helm-compatible templates for the in-house jobs module, rendered by can-core's sandboxed renderer
(not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Port 8080 (`jobs`, `ats`), Service `jobs`, ClusterIP.
- The IngressRoute is rendered only when the manifest declares `runtime.subdomain` (host
  `<subdomain>.<domain>`, supplied by can-core). It routes `/careers` and `/feeds` (prefixes) and
  `/robots.txt` (exact), one route per path, each `Host(...) && PathPrefix(...)` or `Path(...)`
  because the installer accepts no `||`. `/v1` is never routed.
- Set `config.company.name` and `config.company.base_url` (absolute URLs in JSON-LD, feeds, sitemap).
  `scanner_required: true` stays on: no resume uploads until a real scanner exists.
- The sealed-field keyring is the `keyring` key of the `can-module` Secret, mounted with `subPath` at
  `/etc/can-jobs/keyring`. It has the same known gap as people: the loader wants no group or other
  bits, a mounted Secret is root-owned, so the chart uses mode `0440` plus `fsGroup: 65532` and the
  loader must be relaxed to accept that before the pod starts. Not covered by any test here.
- `webhook.url` is not set: events go to can-core's module API. Settings from `company.yaml` are not
  yet mapped onto `config:`.

## What is verified

Unit tests (`core/server/installer/modulecharts_test.go`, part of `make test`) install the chart
through the installer against the fake Kubernetes API, check the route is scoped to the public paths,
check a tampered template with a foreign host is refused, and parse the rendered config with the
module's own loader. `make release-check` fails while `image.digest` is a placeholder. The chart has
not been run on a cluster.
