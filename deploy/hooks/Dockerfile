# can-hooks image (fragment): the tools the module backup/restore hooks need and the upstream
# images lack (curl for the can-core dump endpoint, the MariaDB and Postgres clients, sqlite3).
# can-core runs a module's hooks in this image when module.yaml sets `hooks.image` (SPEC 4.1).
# Build from the repository root:
#
#   docker build -f deploy/hooks/Dockerfile \
#     --build-arg ALPINE_DIGEST=sha256:<digest> \
#     -t ghcr.io/dodwmd/can-hooks:0.1.0 deploy/hooks
#
# The digest is deliberately a placeholder (see deploy/forms/README.md): an unpinned build fails
# at the FROM line. NOT BUILT in the offline environment that wrote this file: the Alpine package
# names below (mariadb-client, postgresql17-client, sqlite) are from memory and must be confirmed
# by the first real build (specs/007-starter-modules/research.md R6).

ARG ALPINE_VERSION=3.22
ARG ALPINE_DIGEST=sha256:REPLACE_WITH_ALPINE_DIGEST

FROM alpine:${ALPINE_VERSION}@${ALPINE_DIGEST}
RUN apk add --no-cache curl gzip mariadb-client postgresql17-client sqlite
# Hooks run as an unprivileged user; the module data volume is group-writable via fsGroup.
USER 1000:1000
WORKDIR /tmp
