# can-files image (fragment). Build from the repository root:
#
#   docker build -f deploy/files/Dockerfile \
#     --build-arg GO_IMAGE_DIGEST=sha256:<digest> \
#     --build-arg DISTROLESS_DIGEST=sha256:<digest> \
#     --build-arg VERSION=$(git describe --tags --always) \
#     -t ghcr.io/dodwmd/can-files:$(git describe --tags --always) .
#
# The digests below are deliberately placeholders: base images are pinned by digest at
# release time (see deploy/files/README.md for how to look them up). A build with a
# placeholder fails at the FROM line, which is the intended guard against unpinned builds.

ARG GO_VERSION=1.27
ARG GO_IMAGE_DIGEST=sha256:REPLACE_WITH_GOLANG_DIGEST
ARG DISTROLESS_DIGEST=sha256:REPLACE_WITH_DISTROLESS_STATIC_DIGEST

FROM golang:${GO_VERSION}@${GO_IMAGE_DIGEST} AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/can-files ./cmd/can-files
COPY core ./core
COPY schemas ./schemas
# Pure Go (modernc.org/sqlite): no cgo, so the static distroless base is enough.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/dodwmd/companyinacan/core/files.Version=${VERSION}" \
      -o /out/can-files ./cmd/can-files

FROM gcr.io/distroless/static-debian12:nonroot@${DISTROLESS_DIGEST}
COPY --from=build /out/can-files /can-files
# /data holds the SQLite database (files.db), the upload staging area and, with the local
# backend, the blobs; mount a volume owned by uid 65532 there.
VOLUME ["/data"]
EXPOSE 8095
USER 65532:65532
ENTRYPOINT ["/can-files"]
CMD ["serve"]
