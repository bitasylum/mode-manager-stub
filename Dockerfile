# The client library comes from the version go.mod requires, unless the build
# is given a "library" build context with a local checkout of it, which make
# docker-build passes when a go.work file points at one.
FROM scratch AS library

# Build the mode-manager-stub binary.
# Base images are pinned by digest, and Dependabot updates the tag and digest
# together. hack/build-go-version.sh reads the Go version from this line, so
# CI and make vulncheck use the Go that builds the image. The builder runs on
# the build machine's platform and cross-compiles for the target.
FROM --platform=$BUILDPLATFORM golang:1.26.10@sha256:5c3f3f28a273c9b61698b2d34f18375929790ab1f1d3835d032d21e2b31eae0c AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace/stub
COPY --from=library / /workspace/library/
COPY go.mod go.sum ./
RUN if [ -f /workspace/library/go.mod ]; then go work init . /workspace/library; fi && go mod download

COPY . .
# VERSION is stamped into the binary and logged at startup. It's declared
# here, because every RUN after it would miss the cache when it changes.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /mode-manager-stub ./cmd/mode-manager-stub

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
# GHCR links an image to the repository named here, so the package inherits
# the repository's access permissions. Its visibility is set separately: a new
# package is private until made public in its settings.
LABEL org.opencontainers.image.source="https://github.com/bitasylum/mode-manager-stub" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /mode-manager-stub /mode-manager-stub
USER 65532:65532

ENTRYPOINT ["/mode-manager-stub"]
