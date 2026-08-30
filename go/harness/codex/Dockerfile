ARG BASE_IMAGE_REGISTRY=cgr.dev
ARG GO_BUILDER_DIGEST=sha256:f83f9c10f9c18d7b9a71d241b63b8824de5a0ad6caea4255406e42b4005320fe
ARG ALPINE_IMAGE=alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE_REGISTRY}/chainguard/go:latest@${GO_BUILDER_DIGEST} AS builder
ARG TARGETARCH
ARG TARGETOS

WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod,rw \
    --mount=type=cache,target=/root/.cache/go-build,rw \
    go mod download
COPY api/ api/
COPY adk/ adk/
COPY core/ core/
COPY harness/ harness/
RUN --mount=type=cache,target=/root/go/pkg/mod,rw \
    --mount=type=cache,target=/root/.cache/go-build,rw \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -o /kagent-codex ./harness/codex/cmd

FROM ${ALPINE_IMAGE}
ARG CODEX_VERSION=0.151.0
ARG CODEX_AMD64_ARCHIVE_SHA256=605b4b183f22c645f5def63a5b7191767407fb66a6feaec4eaf10b5b7e0058f6
ARG CODEX_ARM64_ARCHIVE_SHA256=c1cf2baf375e261c1469381a52dc2c8fd05b6fb45cfff83fed0988fd6c5369b6
ARG TARGETARCH
ARG VERSION

RUN apk add --no-cache bash ca-certificates git libgcc libstdc++ ripgrep \
    && case "${TARGETARCH}" in \
        amd64) platform="x86_64-unknown-linux-musl"; checksum="${CODEX_AMD64_ARCHIVE_SHA256}" ;; \
        arm64) platform="aarch64-unknown-linux-musl"; checksum="${CODEX_ARM64_ARCHIVE_SHA256}" ;; \
        *) echo "unsupported target architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && archive="/tmp/codex-${platform}.tar.gz" \
    && wget -T 60 -t 3 -O "${archive}" \
        "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-${platform}.tar.gz" \
    && echo "${checksum}  ${archive}" | sha256sum -c - \
    && tar -xzf "${archive}" -C /tmp \
    && install -m 0755 "/tmp/codex-${platform}" /usr/local/bin/codex \
    && rm -f "${archive}" "/tmp/codex-${platform}" \
    && codex --version | grep -Fx "codex-cli ${CODEX_VERSION}" \
    && addgroup -S -g 65532 kagent \
    && adduser -S -D -H -u 65532 -G kagent -h /data -s /sbin/nologin kagent \
    && mkdir -p /data/adapter /data/codex /data/workspace \
    && chown -R 65532:65532 /data

COPY --from=builder /kagent-codex /usr/local/bin/kagent-codex

USER 65532:65532
WORKDIR /data/workspace
EXPOSE 80 8081

LABEL org.opencontainers.image.source="https://github.com/kagent-dev/kagent"
LABEL org.opencontainers.image.description="Kagent Codex native Harness adapter"
LABEL org.opencontainers.image.version="$VERSION"

ENTRYPOINT ["/usr/local/bin/kagent-codex"]
