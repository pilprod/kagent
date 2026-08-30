#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s VERSION OUTPUT_DIRECTORY\n' "$0" >&2
  exit 2
fi

version="$1"
output_directory="$2"
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$ ]]; then
  printf 'fork preview chart version is invalid: %s\n' "${version}" >&2
  exit 1
fi

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
temporary_directory="$(mktemp -d)"
trap 'rm -rf "${temporary_directory}"' EXIT
python="${PYTHON:-python3}"
if ! command -v "${python}" >/dev/null 2>&1; then
  printf 'python is required to create deterministic chart archives: %s\n' "${python}" >&2
  exit 1
fi

mkdir -p "${output_directory}"
output_directory="$(cd "${output_directory}" && pwd -P)"

"${script_directory}/stage-fork-preview-charts.sh" \
  "${version}" "${temporary_directory}/staged"

for chart in kagent-crds kagent; do
  staged_directory="${temporary_directory}/staged/${chart}"

  helm lint --strict "${staged_directory}"
  if [[ "${chart}" == "kagent" ]]; then
    helm unittest "${staged_directory}"
  fi
  # Helm 3.21 stamps time.Now into tar headers even when every staged file has
  # a fixed mtime. Package the already linted source with normalized headers
  # and gzip metadata so the same signed source has one stable OCI manifest.
  archive="${output_directory}/${chart}-${version}.tgz"
  "${python}" "${script_directory}/package-deterministic-helm-chart.py" \
    "${staged_directory}" "${archive}"
  helm show chart "${archive}" >/dev/null
done

for chart in kagent-crds kagent; do
  archive="${output_directory}/${chart}-${version}.tgz"
  [[ -f "${archive}" ]] || {
    printf 'missing packaged preview chart: %s\n' "${archive}" >&2
    exit 1
  }
  if tar -xOf "${archive}" "${chart}/Chart.yaml" | grep -q '^dependencies:'; then
    printf 'preview chart unexpectedly contains dependencies: %s\n' "${chart}" >&2
    exit 1
  fi
done
