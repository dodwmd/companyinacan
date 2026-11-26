# deploy/hooks

The `can-hooks` image: Alpine plus `curl`, `gzip`, the MariaDB and Postgres clients and `sqlite3`.
The wrapped modules (`support`, `newsletter`, `analytics`, `crm`, `wiki`, `status`) name it as `hooks.image` because their
upstream images have none of those tools, and can-core's own image has no shell (SPEC 4.1).

It carries no secrets and no code beyond the Alpine packages; the hook scripts themselves live in
`modules/<id>/hooks/` and are mounted by can-core as a ConfigMap.

## Digest pinning

Same convention as `deploy/forms`: the Dockerfile ships a `sha256:REPLACE_WITH_ALPINE_DIGEST`
placeholder, `make release-check` fails while it remains, and the digest is looked up with
`crane digest alpine:3.22` and committed in a reviewed change. The `hooks.image` reference in the
module manifests is a tag; nothing resolves it to a digest yet.

## Client versions

Dump tools must not be older than the server they dump: `pg_dump` refuses a newer server, and the
Postgres client here is `postgresql17-client`, so the shared Postgres in `can-data` must be major
version 17 or lower. The MariaDB client series should match the shared MariaDB major. At first
build record the exact `apk` versions (`apk info -v`) here and in the release notes, and compare
them with the database image versions E03 deploys; bump the client packages together with the
database images. The Alpine digest freezes the base layer, not the package index, so the exact
versions in each published image are the record.

## Status

CI builds this image on pull requests that touch it or a module (`.github/workflows/module-backup.yml`), with the
base digest resolved at that moment, checks that the tools are present and parses every `hooks/*.sh` in it. Nothing
is pushed. Not yet done: publishing (the release pipeline pushes `can-hooks:vX.Y.Z` and `:X.Y.Z`; the manifests name
`0.1.0`, so a release of another version needs the manifests updated), and running any hook against the application
it dumps.
