# DEV ONLY: one Dockerfile that builds every service image for docker-compose.yml.
# Context is the repository root:  docker build -f dev/compose/Dockerfile --target work .
#
# Unlike deploy/*/Dockerfile (which pin base images by digest for releases and fail on the
# placeholders), this file uses UNPINNED tags, the same way the CI dev-cluster workflow does.
# Do not ship images built from it.
ARG GO_IMAGE=docker.io/library/golang:1.27
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot
ARG EDGE_IMAGE=docker.io/library/caddy:2.8-alpine

FROM ${GO_IMAGE} AS build
WORKDIR /src
# Dependency layer: only re-runs when go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY core ./core
COPY deploy/embed.go ./deploy/embed.go
COPY deploy/templates ./deploy/templates
COPY schemas ./schemas
COPY modules ./modules
COPY examples ./examples
COPY dev/compose/introstub ./dev/compose/introstub
COPY templates ./templates
RUN set -e; mkdir -p /out; \
    for b in can can-core can-static can-forms can-work can-people can-jobs can-finance can-equity; do \
      CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/$b ./cmd/$b; \
    done; \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/introstub ./dev/compose/introstub
# The example site, rendered once at build time and baked into the `site` image.
COPY dev/compose/site-extra /site-extra
RUN /out/can site build examples/acme-co --out /site --base-url http://localhost:8080 \
 && cp -r /site-extra/. /site/
# Empty data directories owned by the distroless nonroot user (65532). A new named volume
# copies the mount point's content and ownership from the image on first use.
RUN mkdir -p /rootfs/data /rootfs/var/lib/can && chown -R 65532:65532 /rootfs

FROM ${RUNTIME_IMAGE} AS base
USER 65532:65532

FROM base AS site
COPY --from=build /out/can-static /can-static
COPY --from=build /site /srv/site
ENV CAN_STATIC_DIR=/srv/site CAN_STATIC_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/can-static"]

FROM base AS forms
COPY --from=build /out/can-forms /can-forms
COPY --from=build --chown=65532:65532 /rootfs/data /data
COPY dev/compose/config/forms.yaml /etc/can-forms/forms.yaml
EXPOSE 8080 8081
ENTRYPOINT ["/can-forms"]
CMD ["serve", "-config", "/etc/can-forms/forms.yaml"]

FROM base AS work
COPY --from=build /out/can-work /can-work
COPY --from=build --chown=65532:65532 /rootfs/data /data
COPY dev/compose/config/work.yaml /etc/can-work/work.yaml
EXPOSE 8080
ENTRYPOINT ["/can-work"]
CMD ["serve"]

FROM base AS people
COPY --from=build /out/can-people /can-people
COPY --from=build --chown=65532:65532 /rootfs/data /data
COPY dev/compose/config/people.yaml dev/compose/config/people.e2e.yaml /etc/can-people/
EXPOSE 8081
ENTRYPOINT ["/can-people"]
CMD ["serve", "-config", "/etc/can-people/people.yaml"]

FROM base AS jobs
COPY --from=build /out/can-jobs /can-jobs
COPY --from=build --chown=65532:65532 /rootfs/data /data
COPY dev/compose/config/jobs.yaml /etc/can-jobs/jobs.yaml
EXPOSE 8080
ENTRYPOINT ["/can-jobs"]
CMD ["serve"]

FROM base AS finance
COPY --from=build /out/can-finance /can-finance
COPY --from=build --chown=65532:65532 /rootfs/data /data
COPY dev/compose/config/finance.yaml /etc/can-finance/finance.yaml
EXPOSE 8080
ENTRYPOINT ["/can-finance"]
CMD ["serve"]

FROM base AS equity
COPY --from=build /out/can-equity /can-equity
COPY --from=build --chown=65532:65532 /rootfs/data /data
COPY dev/compose/config/equity.yaml dev/compose/config/equity.e2e.yaml /etc/can-equity/
EXPOSE 8081
ENTRYPOINT ["/can-equity"]
CMD ["serve", "-config", "/etc/can-equity/equity.yaml"]

# E2E ONLY (dev/compose/e2e.yml): fake token introspection for people and equity.
FROM base AS introstub
COPY --from=build /out/introstub /introstub
COPY dev/compose/config/introstub.json /etc/introstub/tokens.json
EXPOSE 8099
ENTRYPOINT ["/introstub"]

FROM base AS core
COPY --from=build /out/can-core /can-core
COPY --from=build --chown=65532:65532 /rootfs/var/lib/can /var/lib/can
COPY modules /modules
ENV CAN_MODULES_DIR=/modules CAN_DB_PATH=/var/lib/can/state.db CAN_MEMLIMIT_MB=64
EXPOSE 8080 8090
ENTRYPOINT ["/can-core"]
CMD ["serve"]

# One-shot init job: writes the dev keyrings and bootstraps the admin account. It needs a
# shell, so it is based on the Go image (already pulled for the build stage), and runs as root.
FROM ${GO_IMAGE} AS init
COPY --from=build /out/can-core /usr/local/bin/can-core
COPY dev/compose/init.sh /init.sh
ENTRYPOINT ["/bin/sh", "/init.sh"]

FROM ${EDGE_IMAGE} AS edge
COPY dev/compose/config/Caddyfile /etc/caddy/Caddyfile
