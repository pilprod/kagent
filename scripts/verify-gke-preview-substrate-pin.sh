#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin_file="${1:-${root_dir}/.github/gke-preview-substrate-pin.json}"

fail() {
  printf 'Substrate preview pin verification failed: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail "jq is required"
test -f "${pin_file}" || fail "pin manifest does not exist: ${pin_file}"

jq -e '
  .schemaVersion == 1 and
  (.status == "blocked" or .status == "ready") and
  .requiredCapabilities == [
    "pkg/api/v1alpha1.ActorTemplateSpec.WorkerProvider",
    "pkg/api/v1alpha1.WorkerProviderExternalSlot",
    "api.Control.OpenActorIngress",
    "api.ActorIngressFrame"
  ] and
  .goModule.requiredPath == "github.com/agent-substrate/substrate" and
  .goModule.replacement.path == "github.com/pilprod/substrate" and
  .goModule.origin.vcs == "git" and
  .goModule.origin.url == "https://github.com/pilprod/substrate" and
  .helmChart.repository == "oci://ghcr.io/pilprod/substrate/helm" and
  .helmChart.name == "substrate"
' "${pin_file}" >/dev/null || fail "pin manifest semantic identity contract is malformed"

pin_status="$(jq -er '.status' "${pin_file}")"
if [[ "${pin_status}" == "blocked" ]]; then
  jq -e '
    (.blocker | type == "string" and length > 0) and
    (.requiredCapabilities | type == "array" and length > 0) and
    (.lastIncompatiblePublicRelease.goModuleVersion | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    (.lastIncompatiblePublicRelease.helmChartVersion | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    .goModule.replacement.version == null and
    .goModule.replacement.sum == null and
    .goModule.replacement.goModSum == null and
    .goModule.origin.commit == null and
    .goModule.origin.ref == null and
    .helmChart.version == null and
    .helmChart.appVersion == null and
    .helmChart.registryDigest == null and
    .helmChart.packageSha256 == null
  ' "${pin_file}" >/dev/null || fail "blocked pin manifest must not contain invented immutable references"
  fail "pin manifest is intentionally blocked: $(jq -er '.blocker' "${pin_file}")"
fi

for command_name in go helm; do
  command -v "${command_name}" >/dev/null 2>&1 || fail "${command_name} is required"
done

test "${GOWORK:-}" = "off" || fail "GOWORK must be explicitly set to off"
test ! -e "${root_dir}/go.work" || fail "repository-local go.work is forbidden"
test ! -e "${root_dir}/go/go.work" || fail "go/go.work is forbidden"

jq -e '
  .status == "ready" and
  (.goModule.replacement.version | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  (.goModule.replacement.sum | test("^h1:[A-Za-z0-9+/]{43}=$")) and
  (.goModule.replacement.goModSum | test("^h1:[A-Za-z0-9+/]{43}=$")) and
  (.goModule.origin.commit | test("^[0-9a-f]{40}$")) and
  (.goModule.origin.ref | test("^refs/tags/v")) and
  (.helmChart.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  (.helmChart.appVersion | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  (.helmChart.registryDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.helmChart.packageSha256 | test("^sha256:[0-9a-f]{64}$")) and
  .goModule.replacement.version == ("v" + .helmChart.version) and
  .goModule.origin.ref == ("refs/tags/" + .goModule.replacement.version) and
  .helmChart.appVersion == .goModule.replacement.version
' "${pin_file}" >/dev/null || fail "pin manifest is malformed or versions are incompatible"

required_path="$(jq -er '.goModule.requiredPath' "${pin_file}")"
replacement_path="$(jq -er '.goModule.replacement.path' "${pin_file}")"
replacement_version="$(jq -er '.goModule.replacement.version' "${pin_file}")"
replacement_sum="$(jq -er '.goModule.replacement.sum' "${pin_file}")"
replacement_go_mod_sum="$(jq -er '.goModule.replacement.goModSum' "${pin_file}")"
origin_vcs="$(jq -er '.goModule.origin.vcs' "${pin_file}")"
origin_url="$(jq -er '.goModule.origin.url' "${pin_file}")"
origin_commit="$(jq -er '.goModule.origin.commit' "${pin_file}")"
origin_ref="$(jq -er '.goModule.origin.ref' "${pin_file}")"

test "$(cd "${root_dir}/go" && go env GOWORK)" = "off" || fail "Go did not disable workspace discovery"
test "$(cd "${root_dir}/go" && go env GOPROXY)" = "https://proxy.golang.org" || fail "GOPROXY must use only the public Go proxy"
test "$(cd "${root_dir}/go" && go env GOSUMDB)" = "sum.golang.org" || fail "GOSUMDB must use the public checksum database"
test -z "$(cd "${root_dir}/go" && go env GOPRIVATE)" || fail "GOPRIVATE must be empty"
test -z "$(cd "${root_dir}/go" && go env GONOPROXY)" || fail "GONOPROXY must be empty"
test -z "$(cd "${root_dir}/go" && go env GONOSUMDB)" || fail "GONOSUMDB must be empty"

selected_module="$(cd "${root_dir}/go" && GOFLAGS=-mod=readonly go list -m -json "${required_path}")"
jq -e \
  --arg required_path "${required_path}" \
  --arg replacement_path "${replacement_path}" \
  --arg replacement_version "${replacement_version}" \
  --arg replacement_sum "${replacement_sum}" \
  --arg replacement_go_mod_sum "${replacement_go_mod_sum}" '
    .Path == $required_path and
    .Replace.Path == $replacement_path and
    .Replace.Version == $replacement_version and
    .Replace.Sum == $replacement_sum and
    .Replace.GoModSum == $replacement_go_mod_sum
  ' <<<"${selected_module}" >/dev/null || fail "go.mod does not select the declared replacement and checksums"

downloaded_module="$(cd "${root_dir}/go" && go mod download -json "${replacement_path}@${replacement_version}")"
jq -e \
  --arg replacement_path "${replacement_path}" \
  --arg replacement_version "${replacement_version}" \
  --arg replacement_sum "${replacement_sum}" \
  --arg replacement_go_mod_sum "${replacement_go_mod_sum}" \
  --arg origin_vcs "${origin_vcs}" \
  --arg origin_url "${origin_url}" \
  --arg origin_commit "${origin_commit}" \
  --arg origin_ref "${origin_ref}" '
    .Path == $replacement_path and
    .Version == $replacement_version and
    .Sum == $replacement_sum and
    .GoModSum == $replacement_go_mod_sum and
    .Origin.VCS == $origin_vcs and
    .Origin.URL == $origin_url and
    .Origin.Hash == $origin_commit and
    .Origin.Ref == $origin_ref and
    (.Error | not)
  ' <<<"${downloaded_module}" >/dev/null || fail "public Go proxy content does not match the declared immutable pin"

chart_repository="$(jq -er '.helmChart.repository' "${pin_file}")"
chart_name="$(jq -er '.helmChart.name' "${pin_file}")"
chart_version="$(jq -er '.helmChart.version' "${pin_file}")"
chart_app_version="$(jq -er '.helmChart.appVersion' "${pin_file}")"
chart_registry_digest="$(jq -er '.helmChart.registryDigest' "${pin_file}")"
chart_package_sha256="$(jq -er '.helmChart.packageSha256' "${pin_file}")"

chart_metadata="$(helm show chart "${chart_repository}/${chart_name}" --version "${chart_version}" 2>&1)"
test "$(awk '$1 == "Digest:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_registry_digest}" || fail "public Helm registry digest does not match the declared pin"
test "$(awk '$1 == "name:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_name}" || fail "public Helm chart name does not match"
test "$(awk '$1 == "version:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_version}" || fail "public Helm chart version does not match"
test "$(awk '$1 == "appVersion:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_app_version}" || fail "public Helm chart appVersion does not match"

chart_tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${chart_tmp_dir}"' EXIT
helm pull "${chart_repository}/${chart_name}" --version "${chart_version}" --destination "${chart_tmp_dir}" >/dev/null
chart_package="${chart_tmp_dir}/${chart_name}-${chart_version}.tgz"
test -f "${chart_package}" || fail "Helm did not download the pinned chart package"
if command -v sha256sum >/dev/null 2>&1; then
  actual_chart_package_sha256="sha256:$(sha256sum "${chart_package}" | awk '{ print $1 }')"
else
  actual_chart_package_sha256="sha256:$(shasum -a 256 "${chart_package}" | awk '{ print $1 }')"
fi
test "${actual_chart_package_sha256}" = "${chart_package_sha256}" || fail "public Helm package content does not match the declared pin"

printf 'immutable public Substrate Go module and Helm chart pin verified\n'
