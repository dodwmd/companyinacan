# deploy/jobs

Container build for the jobs and recruiting module (`cmd/can-jobs`).

## Image

`Dockerfile` is a two-stage build: `golang` builder, then `gcr.io/distroless/static-debian12:nonroot`
(no shell, no package manager, uid 65532). The binary is static (`CGO_ENABLED=0`; SQLite is the
pure-Go `modernc.org/sqlite`). Because there is no `curl`, health probes use the binary itself:
`/can-jobs healthcheck -addr 127.0.0.1:8080`.

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
| Listener | `:8080`: public careers pages (`/careers`, `/feeds/*`, `/robots.txt`) and the `jobs` and `ats` capability API under `/v1` (bearer token). Expose only the public paths through the ingress; `/v1` stays ClusterIP. `GET /healthz` needs no token |
| Data | `/data/jobs.db` (SQLite, WAL) and `/data/resumes/` (resume files, 0600), one persistent volume |
| Config | `/etc/can-jobs/jobs.yaml`; `company.name` and `company.base_url` are required (see `modules/jobs/README.md`) |
| Secrets | env `CAN_JOBS_ADMIN_TOKEN` and `CAN_JOBS_HASH_KEY` (32+ characters each); `CAN_JOBS_WEBHOOK_SECRET` when `webhook.url` is set; the field-encryption keyring is a mounted file (mode 0400, mounted with `subPath`) |
| Idle RAM | 16.0 MiB RSS measured (23.0 MiB after 100 creates and 200 page and list requests); target 30 MB, limit 64 MB |
| Root filesystem | read-only works: only `/data` is written (multipart uploads are streamed, so no temp directory is needed) |

SIGTERM triggers a graceful shutdown (in-flight requests finish, the database closes) within 20
seconds; set the pod `terminationGracePeriodSeconds` to at least 30. Queued webhook events are
in the SQLite outbox, so a restart loses none.

The image has **not** been built in CI or run on a cluster yet; the Dockerfile is a fragment with
digest placeholders, and `scripts/check-image-pins.sh` fails a release build until they are pinned.
