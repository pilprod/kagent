### STAGE 1: base image
ARG BASE_IMAGE_REGISTRY=cgr.dev
ARG GO_BUILDER_DIGEST=sha256:f83f9c10f9c18d7b9a71d241b63b8824de5a0ad6caea4255406e42b4005320fe
ARG ALPINE_IMAGE=alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE_REGISTRY}/chainguard/go:latest@${GO_BUILDER_DIGEST} AS builder
ARG TARGETARCH
ARG TARGETPLATFORM
ARG BUILDPLATFORM

# Which package to build (e.g. core/cmd/controller-v2/main.go, adk/cmd/main.go)
ARG BUILD_PACKAGE=core/cmd/controller-v2/main.go

WORKDIR /workspace
# Copy the Go module manifests
COPY go.mod go.sum .
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN --mount=type=cache,target=/root/go/pkg/mod,rw      \
    --mount=type=cache,target=/root/.cache/go-build,rw \
     go mod download
# Copy all source
COPY api/ api/
COPY core/ core/
COPY adk/ adk/
COPY harness/ harness/

# Build
ARG LDFLAGS
RUN --mount=type=cache,target=/root/go/pkg/mod,rw             \
    --mount=type=cache,target=/root/.cache/go-build,rw        \
    echo "Building on $BUILDPLATFORM -> linux/$TARGETARCH" && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags "$LDFLAGS" -o /app "$BUILD_PACKAGE"

### STAGE 2: final image
FROM ${ALPINE_IMAGE}
ARG TARGETPLATFORM

RUN apk add --no-cache bash ca-certificates git 'openssl>=3.5.8-r0' && \
    mkdir -p /skills /plugins /data/plugins && \
    chown -R 65532:65532 /skills /plugins /data

WORKDIR /
COPY --from=builder /app /app
USER 65532:65532
ARG VERSION

LABEL org.opencontainers.image.source=https://github.com/kagent-dev/kagent
LABEL org.opencontainers.image.description="Kagent app is the UI and apiserver for running agents."
LABEL org.opencontainers.image.version="$VERSION"

ENTRYPOINT ["/app"]
