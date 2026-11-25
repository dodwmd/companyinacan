# deploy/people

Container build for the people service (`cmd/can-people`). Same shape as `deploy/forms`: a
two-stage build, `golang` then `gcr.io/distroless/static-debian12:nonroot` (no shell, uid 65532),
static binary (`CGO_ENABLED=0`, pure-Go SQLite). Health probes use the binary:
`/can-people healthcheck -addr 127.0.0.1:8081`.

## Digest pinning

Both base images must be pinned by digest before a release build; the Dockerfile ships placeholders
(`sha256:REPLACE_WITH_...`) so an unpinned build fails at the `FROM` line. Look them up as described in
`deploy/forms/README.md` and pass them as build args.

## Runtime contract

| What | Value |
|---|---|
| API listener | `:8081` (`directory` and `performance`, ClusterIP only, bearer token) |
| Data | `/data/people.db` (SQLite, WAL, `secure_delete`), one persistent volume |
| Config | `/etc/can-people/people.yaml` (see `docs/people.md`) |
| Keyring | `/etc/can-people/keys/keyring`, a mounted Secret, mode 0400 (the service refuses group/world-readable key files) |
| Secrets (env) | `CAN_PEOPLE_API_TOKEN`; `CAN_PEOPLE_WEBHOOK_SECRET` when a webhook is set; `CAN_PEOPLE_INTROSPECT_TOKEN` |
| Root filesystem | read-only works: only `/data` is written |

The image contains no key material and no default token. Back up the keyring separately from the
database and treat backups of `/data` as sensitive: encrypted backups only.

Tool, not legal, HR or payroll advice; see `docs/people.md`.
