#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s package VERSION\n' "$0" >&2
  exit 2
fi

action="$1"
version="$2"
if [[ "${action}" != "package" ]]; then
  printf 'public chart publication is retired; app-gcp owns private GAR promotion\n' >&2
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
charts=(kagent kagent-crds)

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
