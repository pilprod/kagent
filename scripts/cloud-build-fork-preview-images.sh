#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 4 ]]; then
  printf 'usage: %s build|record|promote VERSION SOURCE_COMMIT BUILD_ID\n' "$0" >&2
  exit 2
fi

action="$1"
version="$2"
source_commit="$3"
build_id="$4"
owner="pilprod"
registry="ghcr.io"
repository="${owner}/kagent"
components=(controller ui golang-adk codex-harness)

if [[ ! "${action}" =~ ^(build|record|promote)$ ]]; then
  printf 'unknown Cloud Build image action: %s\n' "${action}" >&2
  exit 2
fi
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$ ]]; then
  printf 'fork preview version is invalid: %s\n' "${version}" >&2
  exit 1
fi
if [[ ! "${source_commit}" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'source commit is invalid: %s\n' "${source_commit}" >&2
  exit 1
fi
if [[ ! "${build_id}" =~ ^[0-9A-Za-z.-]+$ ]]; then
  printf 'Cloud Build ID is invalid: %s\n' "${build_id}" >&2
  exit 1
fi

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd "${script_directory}/.." && pwd -P)"
evidence_directory="/workspace/release-inputs"
candidate_tag="${version}-cloudbuild-${build_id}"
builder="kagent-preview-${build_id}"

test "$(git -C "${repository_root}" rev-parse HEAD)" = "${source_commit}"
mkdir -p "${evidence_directory}"

inspect_digest() {
  docker buildx imagetools inspect "$1" |
    awk '$1 == "Digest:" { print $2; exit }'
}

if [[ "${action}" == "record" ]]; then
  for component in "${components[@]}"; do
    metadata="${evidence_directory}/build-${component}.json"
    digest="$(jq -er '."containerimage.digest"' "${metadata}")"
    [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]
    printf '%s=%s\n' "${component}" "${digest}" \
      > "${evidence_directory}/image-${component}.txt"
  done
  exit 0
fi

if [[ "${action}" == "promote" ]]; then
  for component in "${components[@]}"; do
    expected="$(cut -d= -f2 "${evidence_directory}/image-${component}.txt")"
    [[ "${expected}" =~ ^sha256:[0-9a-f]{64}$ ]]
    candidate="${registry}/${repository}/${component}:${candidate_tag}"
    final="${registry}/${repository}/${component}:${version}"
    actual="$(inspect_digest "${candidate}")"
    test "${actual}" = "${expected}"
    docker buildx imagetools create \
      --tag "${final}" "${registry}/${repository}/${component}@${expected}"
    promoted="$(inspect_digest "${final}")"
    test "${promoted}" = "${expected}"
  done
  exit 0
fi

build_date="$(git -C "${repository_root}" show -s --format=%cs "${source_commit}")"
version_package="github.com/kagent-dev/kagent/go/core/internal/version"
ldflags="-X ${version_package}.Version=${version}"
ldflags+=" -X ${version_package}.GitCommit=${source_commit}"
ldflags+=" -X ${version_package}.BuildDate=${build_date}"

docker buildx create \
  --name "${builder}" \
  --platform linux/amd64,linux/arm64 \
  --driver docker-container \
  --use \
  --driver-opt network=host
docker buildx inspect "${builder}" --bootstrap

build_component() {
  local component="$1"
  local dockerfile="$2"
  local context="$3"
  shift 3
  docker buildx build \
    --builder "${builder}" \
    --progress plain \
    --platform linux/amd64,linux/arm64 \
    --push \
    --provenance=mode=max \
    --sbom=true \
    --metadata-file "${evidence_directory}/build-${component}.json" \
    --label org.opencontainers.image.source=https://github.com/pilprod/kagent \
    --build-arg "VERSION=${version}" \
    --build-arg "LDFLAGS=${ldflags}" \
    --build-arg BASE_IMAGE_REGISTRY=cgr.dev \
    --build-arg DOCKER_REGISTRY=ghcr.io \
    --build-arg DOCKER_REPO=pilprod/kagent \
    --build-arg TOOLS_GO_VERSION=1.27.0 \
    --build-arg TOOLS_NODE_VERSION=24 \
    --tag "${registry}/${repository}/${component}:${candidate_tag}" \
    "$@" \
    --file "${repository_root}/${dockerfile}" \
    "${repository_root}/${context}"
}

build_component controller go/Dockerfile go \
  --build-arg BUILD_PACKAGE=core/cmd/controller-v2/main.go
build_component ui ui/Dockerfile ui
build_component golang-adk go/Dockerfile go \
  --build-arg BUILD_PACKAGE=adk/cmd/main.go
build_component codex-harness go/harness/codex/Dockerfile go
