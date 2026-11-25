# deploy/forms

Container build for the forms gateway (`cmd/can-forms`).

## Image

`Dockerfile` is a two-stage build: `golang` builder, then `gcr.io/distroless/static-debian12:nonroot`
(no shell, no package manager, uid 65532). The binary is static (`CGO_ENABLED=0`; SQLite is the
pure-Go `modernc.org/sqlite`). Because there is no `curl`, health probes use the binary itself:
`/can-forms healthcheck -addr 127.0.0.1:8080`.

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
| Public listener | `:8080` (routed by Traefik at `/_forms`, prefix stripped) |
| API listener | `:8081` (`leads` capability, ClusterIP only, bearer token) |
| Data | `/data/forms.db` (SQLite, WAL), one persistent volume |
| Config | `/etc/can-forms/forms.yaml` (see `modules/forms/README.md`) |
| Secrets (env) | `CAN_FORMS_TOKEN_KEY`, `CAN_FORMS_API_TOKEN`; optional `CAN_FORMS_API_READ_TOKEN`, `CAN_FORMS_WEBHOOK_SECRET`, `CAN_FORMS_CAPTCHA_SECRET`, `CAN_FORMS_EMAIL_TOKEN` |
| Idle RAM | 13.9 MiB RSS measured (15.4 MiB after 200 requests); target 15 MB, limit 32 MB |
| Root filesystem | read-only works: only `/data` is written |

SIGTERM triggers a graceful shutdown (in-flight requests finish, queued notifications flush,
database closes) within 20 seconds; set the pod `terminationGracePeriodSeconds` to at least 30.
