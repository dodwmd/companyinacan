# can-static: the tiny static file server for the site pod (epic E04).
#
# Build from the repository root:
#   docker build -f deploy/can-static/Dockerfile -t can-static:dev .
#
# Base images MUST be pinned by digest in release builds. The digests below are placeholders:
# resolve them when cutting a release (see deploy/can-static/README.md, "Pinning") and let the
# release job fail if a placeholder is still present.
ARG GO_IMAGE=golang:1.27@sha256:REPLACE_WITH_GOLANG_DIGEST
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:REPLACE_WITH_DISTROLESS_DIGEST

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/can-static ./cmd/can-static
COPY core/staticsrv ./core/staticsrv
COPY core/obs ./core/obs
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/can-static ./cmd/can-static

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/can-static /can-static
# The site directory is a volume (the can-core-managed PVC), mounted read-only.
ENV CAN_STATIC_DIR=/srv/site \
    CAN_STATIC_ADDR=:8080 \
    GOMEMLIMIT=24MiB \
    GOGC=50
USER 65532:65532
EXPOSE 8080
# No shell or curl in distroless: probes use the binary itself (exec probe) or GET /healthz.
HEALTHCHECK --interval=30s --timeout=5s CMD ["/can-static", "-healthcheck"]
ENTRYPOINT ["/can-static"]
