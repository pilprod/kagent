#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
verifier="${root_dir}/scripts/verify-gke-preview-substrate-pin.sh"
pin_file="${root_dir}/.github/gke-preview-substrate-pin.json"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/kagent-substrate-pin-test.XXXXXX")"
trap 'rm -rf -- "${tmp_dir}"' EXIT

fail() {
  printf 'Substrate preview pin verifier test failed: %s\n' "$1" >&2
  exit 1
}

expect_failure() {
  local expected="$1"
  local candidate="$2"
  local output

  if output="$(
    GOWORK=off \
      GOPROXY=https://proxy.golang.org \
      GOSUMDB=sum.golang.org \
      GOPRIVATE= \
      GONOPROXY= \
      GONOSUMDB= \
      "${verifier}" "${candidate}" 2>&1
  )"; then
    fail "verifier unexpectedly accepted ${candidate}"
  fi
  grep -F -- "${expected}" <<<"${output}" >/dev/null ||
    fail "expected '${expected}' for ${candidate}, got: ${output}"
}

test -x "${verifier}" || fail "verifier is not executable"
test -f "${pin_file}" || fail "pin manifest does not exist"

jq -e '.status == "ready" and (.blocker | not)' "${pin_file}" >/dev/null ||
  fail "published pin manifest must be ready and contain no blocker"

blocked_candidate="${tmp_dir}/blocked.json"
jq '
  .status = "blocked" |
  .blocker = "immutable public Go checksums are pending" |
  .goModule.replacement.sum = null |
  .goModule.replacement.goModSum = null
' "${pin_file}" >"${blocked_candidate}"
expect_failure "pin manifest is intentionally blocked" "${blocked_candidate}"

declare -a mutations=(
  '.requiredCapabilities[1] = "pkg/api/v1alpha1.WorkerProviderContainer"'
  '.goModule.requiredPath = "example.invalid/substrate"'
  '.goModule.replacement.path = "github.com/example/substrate"'
  '.goModule.origin.url = "https://github.com/example/substrate"'
  '.helmChart.repository = "oci://ghcr.io/example/substrate/helm"'
  '.helmChart.name = "not-substrate"'
)

for index in "${!mutations[@]}"; do
  candidate="${tmp_dir}/tampered-${index}.json"
  jq "${mutations[$index]}" "${pin_file}" >"${candidate}"
  expect_failure "pin manifest semantic identity contract is malformed" "${candidate}"
done

printf 'Substrate preview pin verifier semantic identity tests passed\n'
