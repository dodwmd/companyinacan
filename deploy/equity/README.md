# deploy/equity

Container build for the equity service (`cmd/can-equity`). Same shape as `deploy/people`: a
two-stage build, `golang` then `gcr.io/distroless/static-debian12:nonroot` (no shell, uid 65532),
static binary (`CGO_ENABLED=0`, pure-Go SQLite). Health probes use the binary:
`/can-equity healthcheck -addr 127.0.0.1:8081`.

## Digest pinning

Both base images must be pinned by digest before a release build; the Dockerfile ships placeholders
(`sha256:REPLACE_WITH_...`) so an unpinned build fails at the `FROM` line. Look them up as described in
`deploy/forms/README.md` and pass them as build args.

## Runtime contract

| What | Value |
|---|---|
| API listener | `:8081` (`equity`, ClusterIP only, bearer token) |
| Data | `/data/equity.db` (SQLite, WAL, `secure_delete`), one persistent volume |
| Config | `/etc/can-equity/equity.yaml` (see `docs/equity.md`) |
| Keyring | `/etc/can-equity/keys/keyring`, a mounted Secret, mode 0400 (the service refuses group/world-readable key files) |
| Secrets (env) | `CAN_EQUITY_API_TOKEN`; `CAN_EQUITY_WEBHOOK_SECRET` when a webhook is set; `CAN_EQUITY_INTROSPECT_TOKEN` |
| Root filesystem | read-only works: only `/data` is written |

The image contains no key material and no default token. Back up the keyring separately from the
database and treat backups of `/data` as sensitive: encrypted backups only.

This is a tool, not legal, tax or securities advice; see `docs/equity.md`.
