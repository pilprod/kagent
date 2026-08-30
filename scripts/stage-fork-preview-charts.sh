#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s VERSION STAGING_DIRECTORY\n' "$0" >&2
  exit 2
fi

version="$1"
staging_directory="$2"
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$ ]]; then
  printf 'fork preview chart version is invalid: %s\n' "${version}" >&2
  exit 1
fi

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd "${script_directory}/.." && pwd -P)"

if [[ -e "${staging_directory}" ]]; then
  printf 'refusing to overwrite chart staging directory: %s\n' "${staging_directory}" >&2
  exit 1
fi
mkdir -p "${staging_directory}"
staging_directory="$(cd "${staging_directory}" && pwd -P)"

for chart in kagent-crds kagent; do
  source_directory="${repository_root}/helm/${chart}"
  staged_directory="${staging_directory}/${chart}"
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
done
