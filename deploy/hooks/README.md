# deploy/hooks

The `can-hooks` image: Alpine plus `curl`, `gzip`, the MariaDB and Postgres clients and `sqlite3`.
The starter modules (`support`, `newsletter`, `analytics`) name it as `hooks.image` because their
upstream images have none of those tools, and can-core's own image has no shell (SPEC 4.1).

It carries no secrets and no code beyond the Alpine packages; the hook scripts themselves live in
`modules/<id>/hooks/` and are mounted by can-core as a ConfigMap.

## Digest pinning

Same convention as `deploy/forms`: the Dockerfile ships a `sha256:REPLACE_WITH_ALPINE_DIGEST`
placeholder, `make release-check` fails while it remains, and the digest is looked up with
`crane digest alpine:3.22` and committed in a reviewed change. The `hooks.image` reference in the
module manifests is a tag; can-core resolves it to the digest recorded at release time.

## Status

Not built here (no container runtime in the authoring environment). Confirm on first build that
the packages `mariadb-client`, `postgresql17-client` and `sqlite` exist on the chosen Alpine
release and that `mariadb-dump`, `pg_dump`, `psql` and `sqlite3` are on `PATH`.
