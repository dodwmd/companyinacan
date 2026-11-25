# work

In-house work tracker: a lean, API-first, Jira-style issue tracker. Library `core/work`, binary
`cmd/can-work`, image `deploy/work`, CLI `can work`. It is a module like any other
(`module.yaml`), provides the `work` and `projects` capabilities and publishes
`work.issue.created.v1`, `work.issue.updated.v1`, `work.issue.transitioned.v1` and
`work.comment.created.v1`.

**Status: experimental.** Built and tested offline and measured locally; the image has not been
built or run on a cluster, and there is no web UI yet. A Plane wrapper for 8 GB+ servers is a later,
separate module that provides the same capability.

The full guide, with curl examples, search syntax, tokens, events and limits, is
[`docs/work-tracker.md`](../../docs/work-tracker.md). The wire contract is
[`specs/011-work-tracker/contracts/openapi.yaml`](../../specs/011-work-tracker/contracts/openapi.yaml);
the stable cross-module subset is `schemas/capabilities/work.v1.json` and `projects.v1.json`.

## Configuration (`work.yaml`, optional)

Every key has a default; unknown keys are errors. Secrets are never in this file.

```yaml
listen: ":8080"
db_path: /data/work.db
max_body_bytes: 1048576          # 413 above
trust_proxy: false               # true ONLY behind a proxy that appends the client address (Traefik)
rate_limit:
  per_minute: 600                # per API token; 0 disables
  auth_failures_per_minute: 30   # per client address; 0 disables
  max_tracked: 10000             # bound on remembered keys
webhook: { url: https://hooks.example.com/work }   # needs CAN_WORK_WEBHOOK_SECRET
retention_days: 1825             # delete issues resolved longer ago; 0 keeps everything
audit_retention_days: 2555
```

Environment: `CAN_WORK_ADMIN_TOKEN` (required, 32+ characters with some variety, for example `openssl rand -base64 32`: the bootstrap admin token),
`CAN_WORK_WEBHOOK_SECRET` (16+ characters, required with `webhook.url`), `CAN_WORK_CONFIG`
(config path; default `/etc/can-work/work.yaml` when present).

`can-work check-config` validates configuration and secrets without starting anything.

## Security model in one paragraph

Tokens are `cwt_<id>_<secret>`; only a SHA-256 of the secret is stored. Scopes `work.read`,
`work.write` and `work.admin` (each implies the ones before) and an optional project list decide
what a token can do, and a project outside the list answers 404 like a missing one. Requests are
size-limited, rate-limited per token, bad tokens are throttled per client address, unknown JSON
fields are rejected, the search language only ever binds parameters, and admin actions are written to
an audit log without secrets.

## Footprint

Idle RAM target 20 MB (limit 48 MB). Measured on the static linux/amd64 build: 13.6 MiB RSS after
start and 20.4 MiB after 200 issue creates plus 100 searches and 100 board reads (VmRSS). See
`specs/011-work-tracker/research.md` R7.
