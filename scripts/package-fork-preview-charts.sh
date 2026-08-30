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
repository_root="$(cd "${script_directory}/.." && pwd -P)"
temporary_directory="$(mktemp -d)"
trap 'rm -rf "${temporary_directory}"' EXIT
python="${PYTHON:-python3}"
if ! command -v "${python}" >/dev/null 2>&1; then
  printf 'python is required to create deterministic chart archives: %s\n' "${python}" >&2
  exit 1
fi

mkdir -p "${output_directory}"
output_directory="$(cd "${output_directory}" && pwd -P)"

for chart in kagent-crds kagent; do
  source_directory="${repository_root}/helm/${chart}"
  staged_directory="${temporary_directory}/${chart}"
  cp -R "${source_directory}" "${staged_directory}"
  rm -rf "${staged_directory}/charts"
  if [[ "${chart}" == "kagent" ]]; then
    # This suite targets the deliberately excluded kagent-tools subchart. All
    # parent-chart suites still run against the exact preview archive source.
    rm -f "${staged_directory}/tests/kagent-tools-nodeselector_test.yaml"
  fi

  # Fork previews use an independently installed external Substrate control
  # plane and disable every optional subchart. Removing the dependency block
  # makes the preview archive a closed, source-only artifact: release jobs do
  # not contact chart repositories or resolve a floating dependency range.
  sed '/^dependencies:/,$d' "${source_directory}/Chart-template.yaml" |
    sed "s/\${VERSION}/${version}/g" > "${staged_directory}/Chart.yaml"
  cat >> "${staged_directory}/Chart.yaml" <<'EOF'
annotations:
  preview.yourown.chat/deployment-class: external-substrate-testbed
  preview.yourown.chat/optional-subcharts: excluded
EOF
  rm -f "${staged_directory}/Chart-template.yaml" "${staged_directory}/Chart.lock"

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
