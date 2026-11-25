# deploy/work

Container build for the work tracker (`cmd/can-work`).

## Image

`Dockerfile` is a two-stage build: `golang` builder, then `gcr.io/distroless/static-debian12:nonroot`
(no shell, no package manager, uid 65532). The binary is static (`CGO_ENABLED=0`; SQLite is the
pure-Go `modernc.org/sqlite`). Because there is no `curl`, health probes use the binary itself:
`/can-work healthcheck -addr 127.0.0.1:8080`.

## Digest pinning

Both base images must be pinned by digest before a release build. The Dockerfile ships with
placeholders (`sha256:REPLACE_WITH_...`) so an unpinned build fails instead of silently using
a moving tag. Look the digests up, review them, and pass them as build args (or commit them):

```sh
crane digest golang:1.27                          # GO_IMAGE_DIGEST
crane digest gcr.io/distroless/static-debian12:nonroot   # DISTROLESS_DIGEST
```

Updating a digest is a normal reviewed change. The release pipeline (a later epic) records the
digests it built with next to the image.

## Runtime contract

| What | Value |
|---|---|
| API listener | `:8080` (`work` and `projects` capabilities, ClusterIP only, bearer token; `GET /healthz` needs none) |
| Data | `/data/work.db` (SQLite, WAL), one persistent volume |
| Config | `/etc/can-work/work.yaml`, optional: built-in defaults apply when it is absent (see `modules/work/README.md`) |
| Secrets (env) | `CAN_WORK_ADMIN_TOKEN` (32+ characters); `CAN_WORK_WEBHOOK_SECRET` when `webhook.url` is set |
| Idle RAM | 13.6 MiB RSS measured (20.4 MiB after 200 creates and 200 searches); target 20 MB, limit 48 MB |
| Root filesystem | read-only works: only `/data` is written |

SIGTERM triggers a graceful shutdown (in-flight requests finish, the database closes) within 20
seconds; set the pod `terminationGracePeriodSeconds` to at least 30. Queued webhook events are
in the SQLite outbox, so a restart loses none.

The image has **not** been built in CI or run on a cluster yet; the Dockerfile is a fragment with
digest placeholders, and `scripts/check-image-pins.sh` fails a release build until they are pinned.
