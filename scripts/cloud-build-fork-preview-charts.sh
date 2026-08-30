#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s package|push VERSION\n' "$0" >&2
  exit 2
fi

action="$1"
version="$2"
if [[ ! "${action}" =~ ^(package|push)$ ]]; then
  printf 'unknown Cloud Build chart action: %s\n' "${action}" >&2
  exit 2
fi
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$ ]]; then
  printf 'fork preview version is invalid: %s\n' "${version}" >&2
  exit 1
fi

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd "${script_directory}/.." && pwd -P)"
distribution_directory="/workspace/chart-dist"
reproducibility_directory="/workspace/chart-dist-reproducibility-check"
evidence_directory="/workspace/release-inputs"
charts=(kagent kagent-crds)

if [[ "${action}" == "package" ]]; then
  test ! -e "${distribution_directory}"
  test ! -e "${reproducibility_directory}"
  helm plugin install https://github.com/helm-unittest/helm-unittest.git \
    --version 33c48cac798e465deda9a66c8e6c07c0973cf53d
  "${script_directory}/package-fork-preview-charts.sh" \
    "${version}" "${distribution_directory}"
  "${script_directory}/package-fork-preview-charts.sh" \
    "${version}" "${reproducibility_directory}"
  for chart in "${charts[@]}"; do
    cmp \
      "${distribution_directory}/${chart}-${version}.tgz" \
      "${reproducibility_directory}/${chart}-${version}.tgz"
  done
  exit 0
fi

test -n "${GHCR_USERNAME:-}"
test -n "${GHCR_TOKEN:-}"
mkdir -p "${evidence_directory}"
printf '%s' "${GHCR_TOKEN}" | helm registry login ghcr.io \
  --username "${GHCR_USERNAME}" --password-stdin
trap 'helm registry logout ghcr.io >/dev/null 2>&1 || true' EXIT

for chart in "${charts[@]}"; do
  archive="${distribution_directory}/${chart}-${version}.tgz"
  test -f "${archive}"
  output="$(helm push "${archive}" "oci://ghcr.io/pilprod/kagent/helm")"
  digest="$(awk '$1 == "Digest:" { print $2; exit }' <<<"${output}")"
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]
  printf '%s=%s\n' "${chart}" "${digest}" \
    > "${evidence_directory}/chart-${chart}.txt"
  printf '%s\n' "${output}"
done
