#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin_file="${1:-${root_dir}/.github/gke-preview-substrate-pin.json}"
private_image_verifier="${root_dir}/scripts/verify-substrate-private-images.py"
expected_chart_repository="oci://europe-west3-docker.pkg.dev/yourown-chat/kagent-preview/substrate/helm"
expected_registry_host="europe-west3-docker.pkg.dev"
expected_source_tag_object="00a6a684cea3b3feea67461cf79347332ec759ef"
expected_source_tree="2023bb47bda10ab2d974dabcad3008c574b2d010"
expected_chart_tree="9a8cd4e4a49ade2c5e8ea89dc6795d72e5565971"
expected_supported_profiles='["external-control-plane-only"]'
expected_required_components='["agentgateway","ateapi","atecontroller","atenet"]'
expected_auxiliary_components='["releaseVerifier"]'
expected_source_image_refs='{
  "agentgateway": "ghcr.io/kagent-dev/substrate/agentgateway@sha256:068028a256bd63c91fd6e85a471269c014747297b0ffa785feaef6967eb0c429",
  "ateapi": "ghcr.io/pilprod/substrate/ateapi@sha256:8a4cf985f809cc768e32091e39d45bce5f2e95fe43cd67f01d5e60c7df2ea868",
  "atecontroller": "ghcr.io/pilprod/substrate/atecontroller@sha256:0845893ae2ecfd15f580bc410db22c8daae0d6b0388eca67541154a6ec98f554",
  "atenet": "ghcr.io/pilprod/substrate/atenet@sha256:01d96092c93fd623dbe051479a76573da551b56be29121b11b760d9067fc8c4c",
  "releaseVerifier": "ghcr.io/pilprod/substrate/substrate-release-verify@sha256:850d8d8ec018f49486b410a15dd38e965f1fbb4d02f8a8be36d5256f33eef74b"
}'

fail() {
  printf 'Substrate preview pin verification failed: %s\n' "$1" >&2
  exit 1
}

sha256_file() {
  local path="$1"

  if command -v sha256sum >/dev/null 2>&1; then
    printf 'sha256:%s\n' "$(sha256sum "${path}" | awk '{ print $1 }')"
  else
    printf 'sha256:%s\n' "$(shasum -a 256 "${path}" | awk '{ print $1 }')"
  fi
}

command -v jq >/dev/null 2>&1 || fail "jq is required"
test -f "${pin_file}" || fail "pin manifest does not exist: ${pin_file}"

jq -e \
  --arg expected_chart_repository "${expected_chart_repository}" \
  --arg expected_source_tag_object "${expected_source_tag_object}" \
  --arg expected_source_tree "${expected_source_tree}" \
  --arg expected_chart_tree "${expected_chart_tree}" \
  --argjson expected_supported_profiles "${expected_supported_profiles}" \
  --argjson expected_required_components "${expected_required_components}" \
  --argjson expected_auxiliary_components "${expected_auxiliary_components}" \
  --argjson expected_source_image_refs "${expected_source_image_refs}" '
  .schemaVersion == 3 and
  (.status == "blocked" or .status == "ready") and
  (
    (.status == "ready" and
      (keys | sort) == ["goModule", "helmChart", "lastIncompatiblePublicRelease", "requiredCapabilities", "schemaVersion", "status"]) or
    (.status == "blocked" and
      (keys | sort) == ["blocker", "goModule", "helmChart", "lastIncompatiblePublicRelease", "requiredCapabilities", "schemaVersion", "status"])
  ) and
  .requiredCapabilities == [
    "pkg/api/v1alpha1.ActorTemplateSpec.WorkerProvider",
    "pkg/api/v1alpha1.WorkerProviderExternalSlot",
    "api.Control.OpenActorIngress",
    "api.ActorIngressFrame"
  ] and
  (.lastIncompatiblePublicRelease | keys | sort) == ["goModuleVersion", "helmChartVersion"] and
  (.goModule | keys | sort) == ["origin", "replacement", "requiredPath"] and
  .goModule.requiredPath == "github.com/agent-substrate/substrate" and
  (.goModule.replacement | keys | sort) == ["goModSum", "path", "sum", "version"] and
  .goModule.replacement.path == "github.com/pilprod/substrate" and
  (.goModule.origin | keys | sort) == ["commit", "ref", "url", "vcs"] and
  .goModule.origin.vcs == "git" and
  .goModule.origin.url == "https://github.com/pilprod/substrate" and
  (.helmChart | keys | sort) == ["appVersion", "auxiliaryComponents", "crds", "name", "packageSha256", "publication", "registryDigest", "repository", "requiredComponents", "sourceImageRefs", "supportedProfiles", "version"] and
  .helmChart.repository == $expected_chart_repository and
  .helmChart.name == "substrate" and
  .helmChart.supportedProfiles == $expected_supported_profiles and
  .helmChart.requiredComponents == $expected_required_components and
  .helmChart.auxiliaryComponents == $expected_auxiliary_components and
  .helmChart.sourceImageRefs == $expected_source_image_refs and
  (.helmChart.publication | keys | sort) == ["chartTree", "evidenceSha256", "evidenceUri", "sourceCommit", "sourceRef", "sourceTagObject", "sourceTree"] and
  .helmChart.publication.sourceTagObject == $expected_source_tag_object and
  .helmChart.publication.sourceTree == $expected_source_tree and
  .helmChart.publication.chartTree == $expected_chart_tree and
  (.helmChart.crds | keys | sort) == ["appVersion", "name", "packageSha256", "registryDigest", "repository", "version"] and
  .helmChart.crds.repository == $expected_chart_repository and
  .helmChart.crds.name == "substrate-crds"
' "${pin_file}" >/dev/null || fail "pin manifest semantic identity contract is malformed"

pin_status="$(jq -er '.status' "${pin_file}")"
if [[ "${pin_status}" == "blocked" ]]; then
  jq -e '
    (.blocker | type == "string" and length > 0) and
    (.requiredCapabilities | type == "array" and length > 0) and
    (.lastIncompatiblePublicRelease.goModuleVersion | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    (.lastIncompatiblePublicRelease.helmChartVersion | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    (.goModule.replacement.version | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    (.goModule.replacement.sum | test("^h1:[A-Za-z0-9+/]{43}=$")) and
    (.goModule.replacement.goModSum | test("^h1:[A-Za-z0-9+/]{43}=$")) and
    (.goModule.origin.commit | test("^[0-9a-f]{40}$")) and
    (.goModule.origin.ref | test("^refs/tags/v")) and
    (.helmChart.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+-private\\.[1-9][0-9]*$")) and
    (.helmChart.appVersion | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    (.helmChart.crds.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+-private\\.[1-9][0-9]*$")) and
    (.helmChart.crds.appVersion | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    .helmChart.registryDigest == null and
    .helmChart.packageSha256 == null and
    .helmChart.crds.registryDigest == null and
    .helmChart.crds.packageSha256 == null and
    .helmChart.publication.evidenceUri == null and
    .helmChart.publication.evidenceSha256 == null and
    .goModule.origin.ref == ("refs/tags/" + .goModule.replacement.version) and
    ("v" + (.helmChart.version | split("-")[0])) == .goModule.replacement.version and
    .helmChart.appVersion == .goModule.replacement.version and
    .helmChart.crds.version == .helmChart.version and
    .helmChart.crds.appVersion == .helmChart.appVersion and
    .helmChart.publication.sourceCommit == .goModule.origin.commit and
    .helmChart.publication.sourceRef == .goModule.origin.ref
  ' "${pin_file}" >/dev/null || fail "blocked pin manifest must bind the pending private mirror to the immutable source"
  fail "pin manifest is intentionally blocked: $(jq -er '.blocker' "${pin_file}")"
fi

jq -e '
  .status == "ready" and
  (has("blocker") | not) and
  (.goModule.replacement.version | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  (.goModule.replacement.sum | test("^h1:[A-Za-z0-9+/]{43}=$")) and
  (.goModule.replacement.goModSum | test("^h1:[A-Za-z0-9+/]{43}=$")) and
  (.goModule.origin.commit | test("^[0-9a-f]{40}$")) and
  (.goModule.origin.ref | test("^refs/tags/v")) and
  (.helmChart.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+-private\\.[1-9][0-9]*$")) and
  (.helmChart.appVersion | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  (.helmChart.crds.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+-private\\.[1-9][0-9]*$")) and
  (.helmChart.crds.appVersion | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  (.helmChart.registryDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.helmChart.packageSha256 | test("^sha256:[0-9a-f]{64}$")) and
  (.helmChart.crds.registryDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.helmChart.crds.packageSha256 | test("^sha256:[0-9a-f]{64}$")) and
  (.helmChart.publication.evidenceUri | test("^gs://[^/]+/.+#[1-9][0-9]*$")) and
  (.helmChart.publication.evidenceSha256 | test("^sha256:[0-9a-f]{64}$")) and
  .goModule.origin.ref == ("refs/tags/" + .goModule.replacement.version) and
  ("v" + (.helmChart.version | split("-")[0])) == .goModule.replacement.version and
  .helmChart.appVersion == .goModule.replacement.version and
  .helmChart.crds.version == .helmChart.version and
  .helmChart.crds.appVersion == .helmChart.appVersion and
  .helmChart.publication.sourceCommit == .goModule.origin.commit and
  .helmChart.publication.sourceRef == .goModule.origin.ref
' "${pin_file}" >/dev/null || fail "pin manifest is malformed or versions are incompatible"

chart_repository="$(jq -er '.helmChart.repository' "${pin_file}")"
chart_name="$(jq -er '.helmChart.name' "${pin_file}")"
chart_version="$(jq -er '.helmChart.version' "${pin_file}")"
chart_app_version="$(jq -er '.helmChart.appVersion' "${pin_file}")"
chart_registry_digest="$(jq -er '.helmChart.registryDigest' "${pin_file}")"
chart_package_sha256="$(jq -er '.helmChart.packageSha256' "${pin_file}")"
crds_chart_repository="$(jq -er '.helmChart.crds.repository' "${pin_file}")"
crds_chart_name="$(jq -er '.helmChart.crds.name' "${pin_file}")"
crds_chart_version="$(jq -er '.helmChart.crds.version' "${pin_file}")"
crds_chart_app_version="$(jq -er '.helmChart.crds.appVersion' "${pin_file}")"
crds_chart_registry_digest="$(jq -er '.helmChart.crds.registryDigest' "${pin_file}")"
crds_chart_package_sha256="$(jq -er '.helmChart.crds.packageSha256' "${pin_file}")"
evidence_uri="$(jq -er '.helmChart.publication.evidenceUri' "${pin_file}")"
evidence_sha256="$(jq -er '.helmChart.publication.evidenceSha256' "${pin_file}")"
source_tag_object="$(jq -er '.helmChart.publication.sourceTagObject' "${pin_file}")"
origin_commit="$(jq -er '.goModule.origin.commit' "${pin_file}")"
origin_ref="$(jq -er '.goModule.origin.ref' "${pin_file}")"

expected_evidence_uri="gs://yourown-chat-kagent-preview-evidence-europe-west3/substrate/${chart_version}/release-evidence.json"
evidence_generation="${evidence_uri##*#}"
test "${evidence_uri%#*}" = "${expected_evidence_uri}" ||
  fail "private Substrate evidence URI does not match the release coordinate"
[[ "${evidence_generation}" =~ ^[1-9][0-9]*$ ]] ||
  fail "private Substrate evidence URI must include an immutable GCS generation"

release_evidence_uri="${SUBSTRATE_RELEASE_EVIDENCE_URI:-}"
test -n "${release_evidence_uri}" ||
  fail "SUBSTRATE_RELEASE_EVIDENCE_URI is required for a ready private mirror pin"
test "${release_evidence_uri}" = "${evidence_uri}" ||
  fail "SUBSTRATE_RELEASE_EVIDENCE_URI does not match the declared pin"

release_evidence="${SUBSTRATE_RELEASE_EVIDENCE:-}"
test -n "${release_evidence}" ||
  fail "SUBSTRATE_RELEASE_EVIDENCE is required for a ready private mirror pin"
test -f "${release_evidence}" ||
  fail "SUBSTRATE_RELEASE_EVIDENCE does not exist: ${release_evidence}"
test -r "${release_evidence}" ||
  fail "SUBSTRATE_RELEASE_EVIDENCE is not readable: ${release_evidence}"
test "$(sha256_file "${release_evidence}")" = "${evidence_sha256}" ||
  fail "private Substrate evidence SHA-256 does not match the declared pin"

expected_chart_ref="${chart_repository}/${chart_name}@${chart_registry_digest}"
expected_crds_chart_ref="${crds_chart_repository}/${crds_chart_name}@${crds_chart_registry_digest}"
expected_release_prefix="${chart_repository#oci://}"
expected_release_prefix="${expected_release_prefix%/helm}"
origin_tag="${origin_ref#refs/tags/}"
jq -e \
  --arg source_commit "${origin_commit}" \
  --arg source_tag "${origin_tag}" \
  --arg source_tag_object "${source_tag_object}" \
  --arg release_version "${chart_version}" \
  --arg release_prefix "${expected_release_prefix}" \
  --arg chart_ref "${expected_chart_ref}" \
  --arg chart_digest "${chart_registry_digest}" \
  --arg package_sha256 "${chart_package_sha256}" \
  --arg crds_chart_ref "${expected_crds_chart_ref}" \
  --arg crds_chart_digest "${crds_chart_registry_digest}" \
  --arg crds_package_sha256 "${crds_chart_package_sha256}" \
  --arg source_tree "${expected_source_tree}" \
  --arg chart_tree "${expected_chart_tree}" \
  --argjson expected_supported_profiles "${expected_supported_profiles}" \
  --argjson expected_required_components "${expected_required_components}" \
  --argjson expected_auxiliary_components "${expected_auxiliary_components}" \
  --argjson expected_source_image_refs "${expected_source_image_refs}" '
    def repository_name($component):
      if $component == "releaseVerifier" then "substrate-release-verify" else $component end;
    .schema_version == "yourown.chat/substrate-private-gar-release/v2" and
    (keys | sort) == ["auxiliary_components", "charts", "copy_provenance", "deployment_class", "helm_set_values", "images", "platform_image_digests", "production_eligible", "publication", "required_components", "scan_policy", "schema_version", "source", "supported_profiles"] and
    .deployment_class == "dev-to-approved-prod" and
    .production_eligible == true and
    (.source | keys | sort) == ["chart_tree", "commit", "repository", "tag", "tag_object", "tree"] and
    .source.repository == "https://github.com/pilprod/substrate" and
    .source.commit == $source_commit and
    .source.tag == $source_tag and
    .source.tag_object == $source_tag_object and
    .source.tree == $source_tree and
    .source.chart_tree == $chart_tree and
    (.publication | keys | sort) == ["build_mode", "location", "project_id", "registry_visibility", "release_prefix", "release_version"] and
    .publication.project_id == "yourown-chat" and
    .publication.location == "europe-west3" and
    .publication.registry_visibility == "private" and
    .publication.build_mode == "copied_exact" and
    .publication.release_version == $release_version and
    .publication.release_prefix == $release_prefix and
    .supported_profiles == $expected_supported_profiles and
    .required_components == $expected_required_components and
    .auxiliary_components == $expected_auxiliary_components and
    (.copy_provenance | keys) == ["source_image_refs"] and
    .copy_provenance.source_image_refs == $expected_source_image_refs and
    (.charts | keys) == ["application", "crds"] and
    (.charts.application | keys | sort) == ["digest", "package_sha256", "ref", "release_name", "version"] and
    .charts.application.release_name == "substrate" and
    .charts.application.ref == $chart_ref and
    .charts.application.version == $release_version and
    .charts.application.digest == $chart_digest and
    .charts.application.package_sha256 == $package_sha256 and
    (.charts.crds | keys | sort) == ["digest", "package_sha256", "ref", "release_name", "version"] and
    .charts.crds.release_name == "substrate-crds" and
    .charts.crds.ref == $crds_chart_ref and
    .charts.crds.version == $release_version and
    .charts.crds.digest == $crds_chart_digest and
    .charts.crds.package_sha256 == $crds_package_sha256 and
    .helm_set_values == {
      "image.registry": $release_prefix,
      "image.digests.ateapi": .images.ateapi.digest,
      "image.digests.atecontroller": .images.atecontroller.digest,
      "image.digests.atenet": .images.atenet.digest,
      "images.agentgateway": .images.agentgateway.ref
    } and
    (.scan_policy | keys | sort) == ["blocked_severities", "platforms", "scanner"] and
    .scan_policy.platforms == ["linux/amd64", "linux/arm64"] and
    .scan_policy.blocked_severities == ["HIGH", "CRITICAL"] and
    .scan_policy.scanner == "Google Artifact Analysis On-Demand Scanning" and
    (
      .images as $images |
      .platform_image_digests as $platform_image_digests |
      ($images | keys) == (($expected_required_components + $expected_auxiliary_components) | sort) and
      ($platform_image_digests | keys) == (($expected_required_components + $expected_auxiliary_components) | sort) and
      ([
        ($expected_required_components + $expected_auxiliary_components)[] as $component |
        ($images[$component] | keys) == ["digest", "ref"] and
        ($images[$component].digest | test("^sha256:[0-9a-f]{64}$")) and
        $images[$component].digest ==
          ($expected_source_image_refs[$component] | split("@") | last) and
        $images[$component].ref ==
          ($release_prefix + "/" + repository_name($component) + "@" + $images[$component].digest) and
        ($platform_image_digests[$component] | keys) == ["linux_amd64", "linux_arm64"] and
        ($platform_image_digests[$component].linux_amd64 | test("^sha256:[0-9a-f]{64}$")) and
        ($platform_image_digests[$component].linux_arm64 | test("^sha256:[0-9a-f]{64}$"))
      ] | all)
    )
  ' "${release_evidence}" >/dev/null ||
  fail "private Substrate evidence content does not match the declared pin"

helm_registry_config="${HELM_REGISTRY_CONFIG:-}"
test -n "${helm_registry_config}" ||
  fail "HELM_REGISTRY_CONFIG is required for the private Artifact Registry chart"
test -f "${helm_registry_config}" ||
  fail "HELM_REGISTRY_CONFIG does not exist: ${helm_registry_config}"
test -r "${helm_registry_config}" ||
  fail "HELM_REGISTRY_CONFIG is not readable: ${helm_registry_config}"
jq -e --arg registry_host "${expected_registry_host}" '
  ((.auths[$registry_host].auth? // "") | try @base64d catch "") |
  startswith("oauth2accesstoken:") and
  (length > ("oauth2accesstoken:" | length))
' "${helm_registry_config}" >/dev/null ||
  fail "HELM_REGISTRY_CONFIG must contain short-lived GCP authentication for ${expected_registry_host}"

for command_name in go helm python3; do
  command -v "${command_name}" >/dev/null 2>&1 || fail "${command_name} is required"
done
test -f "${private_image_verifier}" || fail "private Substrate image verifier does not exist"

test "${GOWORK:-}" = "off" || fail "GOWORK must be explicitly set to off"
test ! -e "${root_dir}/go.work" || fail "repository-local go.work is forbidden"
test ! -e "${root_dir}/go/go.work" || fail "go/go.work is forbidden"

python3 "${private_image_verifier}" \
  --evidence "${release_evidence}" \
  --registry-config "${helm_registry_config}" ||
  fail "live private Substrate GAR image verification failed"

required_path="$(jq -er '.goModule.requiredPath' "${pin_file}")"
replacement_path="$(jq -er '.goModule.replacement.path' "${pin_file}")"
replacement_version="$(jq -er '.goModule.replacement.version' "${pin_file}")"
replacement_sum="$(jq -er '.goModule.replacement.sum' "${pin_file}")"
replacement_go_mod_sum="$(jq -er '.goModule.replacement.goModSum' "${pin_file}")"
origin_vcs="$(jq -er '.goModule.origin.vcs' "${pin_file}")"
origin_url="$(jq -er '.goModule.origin.url' "${pin_file}")"

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

chart_metadata="$(helm show chart "${chart_repository}/${chart_name}" --version "${chart_version}" 2>&1)"
test "$(awk '$1 == "Digest:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_registry_digest}" || fail "private Helm registry digest does not match the declared pin"
test "$(awk '$1 == "name:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_name}" || fail "private Helm chart name does not match"
test "$(awk '$1 == "version:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_version}" || fail "private Helm chart version does not match"
test "$(awk '$1 == "appVersion:" { print $2; exit }' <<<"${chart_metadata}")" = "${chart_app_version}" || fail "private Helm chart appVersion does not match"

crds_chart_metadata="$(helm show chart "${crds_chart_repository}/${crds_chart_name}" --version "${crds_chart_version}" 2>&1)"
test "$(awk '$1 == "Digest:" { print $2; exit }' <<<"${crds_chart_metadata}")" = "${crds_chart_registry_digest}" || fail "private CRD Helm registry digest does not match the declared pin"
test "$(awk '$1 == "name:" { print $2; exit }' <<<"${crds_chart_metadata}")" = "${crds_chart_name}" || fail "private CRD Helm chart name does not match"
test "$(awk '$1 == "version:" { print $2; exit }' <<<"${crds_chart_metadata}")" = "${crds_chart_version}" || fail "private CRD Helm chart version does not match"
test "$(awk '$1 == "appVersion:" { print $2; exit }' <<<"${crds_chart_metadata}")" = "${crds_chart_app_version}" || fail "private CRD Helm chart appVersion does not match"

chart_tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${chart_tmp_dir}"' EXIT
helm pull "${chart_repository}/${chart_name}" --version "${chart_version}" --destination "${chart_tmp_dir}" >/dev/null
chart_package="${chart_tmp_dir}/${chart_name}-${chart_version}.tgz"
test -f "${chart_package}" || fail "Helm did not download the pinned chart package"
actual_chart_package_sha256="$(sha256_file "${chart_package}")"
test "${actual_chart_package_sha256}" = "${chart_package_sha256}" || fail "private Helm package content does not match the declared pin"

helm pull "${crds_chart_repository}/${crds_chart_name}" --version "${crds_chart_version}" --destination "${chart_tmp_dir}" >/dev/null
crds_chart_package="${chart_tmp_dir}/${crds_chart_name}-${crds_chart_version}.tgz"
test -f "${crds_chart_package}" || fail "Helm did not download the pinned CRD chart package"
actual_crds_chart_package_sha256="$(sha256_file "${crds_chart_package}")"
test "${actual_crds_chart_package_sha256}" = "${crds_chart_package_sha256}" || fail "private CRD Helm package content does not match the declared pin"

printf 'immutable public Substrate Go module and private Helm chart pins verified\n'
