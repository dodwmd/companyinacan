# files chart

Helm-compatible templates for the in-house files module, rendered by can-core's sandboxed renderer
(not Helm) and applied by its installer. Same templates as the other in-house charts; see
`modules/work/chart/README.md` for the object table.

- Port 8095 (`files`), Service `files`, ClusterIP, one pod, `Recreate` (SQLite on a ReadWriteOnce
  volume).
- The IngressRoute is rendered only when the manifest declares `runtime.subdomain` (`files`, host
  `files.<domain>`, supplied by can-core). It routes only the prefix `/s/` (share-link downloads); the
  `/v1` API and `/healthz` stay cluster-internal. Set `config.share.base_url` to the public origin so
  links created through the API are absolute.
- The web UI (`config.ui.enabled`, off by default) is routed by can-core, not by this chart, with
  `modules.files.expose.ui` (`module.yaml` `runtime.public_ui`).
- `config.scanner.required` is **true** in the shipped values: no upload is accepted until a real
  scanner exists (`scanner.type: clamd` with `scanner.address`). There is no ClamAV chart yet.
- `config.backend.type: local` keeps bytes on the PVC (`/data/blobs`, 0700). `s3` needs
  `CAN_FILES_S3_ACCESS_KEY_ID` and `CAN_FILES_S3_SECRET_ACCESS_KEY` in the environment.
- The sealed-field keyring is the `keyring` key of the `can-module` Secret, mounted with `subPath` at
  `/etc/can-files/keyring`, mode `0440` plus `fsGroup: 65532` (the loader accepts 0400, 0440 and 0600).

- can-core also generates `CAN_FILES_RESTORE_TOKEN`, the restore-only credential of `POST /v1/purge`.

## Known gaps (not covered by any test here)

- **S3 credentials.** can-core generates `CAN_FILES_API_TOKEN` and `CAN_FILES_LINK_SECRET` and the
  keyring. The S3 credentials are not generated and the installer refuses a reference to any other
  Secret, so there is no supported way yet to provide them: the local backend is the working default.
- **Versioned S3 buckets** keep deleted objects until a lifecycle rule expires them (docs/files.md).
- **Egress** to an S3 endpoint or a ClamAV service is blocked: the manifest does not set `runtime.internet_egress`.
  Whoever configures the `s3` backend must set it.
- **Groups.** No directory group resolver is wired; only user grants apply.

## What is verified

Unit tests (`core/server/installer/modulecharts_test.go`, part of `make test`) install the chart
through the installer against the fake Kubernetes API, check the route is scoped to `/s/`, and parse
the rendered config with the module's own loader. `make release-check` fails while `image.digest` is a
placeholder. The chart has not been run on a cluster.
