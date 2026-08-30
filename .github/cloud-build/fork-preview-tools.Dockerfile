FROM docker.io/library/golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS go
FROM docker.io/alpine/helm:3.21.4@sha256:82c0ce1b4196539946ed01bdfd9345cf74ca999b95d3074ce3f2f5ea45c96e80 AS helm
FROM ghcr.io/jqlang/jq:1.8.2@sha256:b9c68867e5766576263a222e91db3de422d802069c7af70440e667a95344e486 AS jq

FROM gcr.io/cloud-builders/gcloud:latest@sha256:3bcfea90f299ae18ced1c0bce4ec035bc4d19049f16c22690ba7c4e730478fbc

COPY --from=go /usr/local/go /usr/local/go
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=jq /jq /usr/local/bin/jq

ENV PATH="/usr/local/go/bin:${PATH}"
ENTRYPOINT ["/bin/bash"]
