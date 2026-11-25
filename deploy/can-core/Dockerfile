# can-core: the in-cluster control plane (epic E03). Build from the repository root:
#   docker build -f deploy/can-core/Dockerfile -t can-core:dev .
#
# Base images MUST be pinned by digest in release builds. The digests below are placeholders:
# resolve them when cutting a release and let `make release-check` fail while any is present.
# CI (dev-cluster workflow) overrides both with --build-arg to unpinned tags for the throwaway cluster.
ARG GO_IMAGE=golang:1.27@sha256:REPLACE_WITH_GOLANG_DIGEST
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:REPLACE_WITH_DISTROLESS_DIGEST

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/can-core ./cmd/can-core
COPY core ./core
COPY deploy/embed.go ./deploy/embed.go
COPY deploy/templates ./deploy/templates
COPY schemas ./schemas
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/can-core ./cmd/can-core

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/can-core /can-core
# Module manifests and charts are embedded in the image (SPEC 3.1): the cluster needs no chart repository.
COPY modules /modules
ENV CAN_MODULES_DIR=/modules \
    CAN_DB_PATH=/var/lib/can/state.db \
    CAN_MEMLIMIT_MB=64
USER 65532:65532
EXPOSE 8080 8090
# No shell in distroless: probes are HTTP (/healthz).
ENTRYPOINT ["/can-core"]
CMD ["serve"]
