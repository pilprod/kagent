#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s codex|claude RELEASE_EVIDENCE_JSON\n' "$0" >&2
  exit 2
fi

runtime="$1"
evidence="$2"
case "${runtime}" in
  codex)
    evidence_key="codexHarness"
    placeholder="@@CODEX_HARNESS_IMAGE@@"
    ;;
  claude)
    evidence_key="claudeHarness"
    placeholder="@@CLAUDE_HARNESS_IMAGE@@"
    ;;
  *)
    printf 'unsupported testbed runtime: %s\n' "${runtime}" >&2
    exit 2
    ;;
esac

if [[ ! -f "${evidence}" ]]; then
  printf 'release evidence does not exist: %s\n' "${evidence}" >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  printf 'jq is required to read release evidence\n' >&2
  exit 1
fi

schema_version="$(jq -er '.schemaVersion | select(. == 2)' "${evidence}")" || {
  printf 'release evidence must use schemaVersion 2\n' >&2
  exit 1
}
[[ "${schema_version}" == "2" ]]

image="$(jq -er --arg key "${evidence_key}" '.runtime_images[$key] | strings | select(length > 0)' "${evidence}")" || {
  printf 'release evidence does not contain runtime_images.%s\n' "${evidence_key}" >&2
  exit 1
}
if [[ ! "${image}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]]; then
  printf 'release evidence runtime_images.%s is not digest-qualified: %s\n' "${evidence_key}" "${image}" >&2
  exit 1
fi

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
template="${script_directory}/${runtime}.yaml.tmpl"
rendered="$(<"${template}")"
if [[ "${rendered}" != *"${placeholder}"* ]]; then
  printf 'testbed template is missing its image placeholder: %s\n' "${placeholder}" >&2
  exit 1
fi
rendered="${rendered//${placeholder}/${image}}"
if [[ "${rendered}" == *"@@"* ]]; then
  printf 'testbed render still contains an unresolved placeholder\n' >&2
  exit 1
fi
printf '%s\n' "${rendered}"
