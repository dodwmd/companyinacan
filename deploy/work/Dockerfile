# can-work image (fragment). Build from the repository root:
#
#   docker build -f deploy/work/Dockerfile \
#     --build-arg GO_IMAGE_DIGEST=sha256:<digest> \
#     --build-arg DISTROLESS_DIGEST=sha256:<digest> \
#     --build-arg VERSION=$(git describe --tags --always) \
#     -t ghcr.io/dodwmd/can-work:$(git describe --tags --always) .
#
# The digests below are deliberately placeholders: base images are pinned by digest at
# release time (see deploy/work/README.md for how to look them up). A build with a
# placeholder fails at the FROM line, which is the intended guard against unpinned builds.

ARG GO_VERSION=1.27
ARG GO_IMAGE_DIGEST=sha256:REPLACE_WITH_GOLANG_DIGEST
ARG DISTROLESS_DIGEST=sha256:REPLACE_WITH_DISTROLESS_STATIC_DIGEST

FROM golang:${GO_VERSION}@${GO_IMAGE_DIGEST} AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/can-work ./cmd/can-work
COPY core ./core
COPY schemas ./schemas
# Pure Go (modernc.org/sqlite): no cgo, so the static distroless base is enough.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/dodwmd/companyinacan/core/work.Version=${VERSION}" \
      -o /out/can-work ./cmd/can-work

FROM gcr.io/distroless/static-debian12:nonroot@${DISTROLESS_DIGEST}
COPY --from=build /out/can-work /can-work
# /data holds the SQLite database (work.db); mount a volume owned by uid 65532 there.
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/can-work"]
CMD ["serve"]
