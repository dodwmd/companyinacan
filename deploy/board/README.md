# deploy/board

Container build for the board and entity service (`cmd/can-board`, both modules in one binary). Same shape as `deploy/people`: a
two-stage build, `golang` then `gcr.io/distroless/static-debian12:nonroot` (no shell, uid 65532),
static binary (`CGO_ENABLED=0`, pure-Go SQLite). Health probes use the binary:
`/can-board healthcheck -addr 127.0.0.1:8082`.

## Digest pinning

Both base images must be pinned by digest before a release build; the Dockerfile ships placeholders
(`sha256:REPLACE_WITH_...`) so an unpinned build fails at the `FROM` line. Look them up as described in
`deploy/forms/README.md` and pass them as build args.

## Runtime contract

| What | Value |
|---|---|
| API listener | `:8082` (`board`, `resolutions` and `entity`, ClusterIP only, bearer token) |
| Data | `/data/board.db` (SQLite, WAL, `secure_delete`), one persistent volume |
| Config | `/etc/can-board/board.yaml` (see `docs/board.md`) |
| Keyring | `/etc/can-board/keys/keyring`, a mounted Secret, mode 0400, or 0440 with `fsGroup` as the chart mounts it (never world-readable) |
| Secrets (env) | `CAN_BOARD_API_TOKEN`; `CAN_BOARD_WEBHOOK_SECRET` when a webhook is set; `CAN_BOARD_INTROSPECT_TOKEN` |
| Root filesystem | read-only works: only `/data` is written |

The image contains no key material and no default token. Back up the keyring separately from the
database and treat backups of `/data` as sensitive: encrypted backups only.

This is a tool, not legal advice; see `docs/board.md` and `docs/entity.md`.
