#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
verifier="${root_dir}/scripts/verify-gke-preview-substrate-pin.sh"
private_image_verifier_test="${root_dir}/scripts/test-verify-substrate-private-images.py"
pin_file="${root_dir}/.github/gke-preview-substrate-pin.json"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/kagent-substrate-pin-test.XXXXXX")"
trap 'rm -rf -- "${tmp_dir}"' EXIT
expected_chart_repository="oci://europe-west3-docker.pkg.dev/yourown-chat/kagent-preview/substrate/helm"
expected_source_tag_object="00a6a684cea3b3feea67461cf79347332ec759ef"
expected_source_tree="2023bb47bda10ab2d974dabcad3008c574b2d010"
expected_chart_tree="9a8cd4e4a49ade2c5e8ea89dc6795d72e5565971"
expected_evidence_uri="gs://yourown-chat-kagent-preview-evidence-europe-west3/substrate/0.0.22-private.3/release-evidence.json#1234567890"
expected_source_image_refs='{
  "agentgateway": "ghcr.io/kagent-dev/substrate/agentgateway@sha256:068028a256bd63c91fd6e85a471269c014747297b0ffa785feaef6967eb0c429",
  "ateapi": "ghcr.io/pilprod/substrate/ateapi@sha256:8a4cf985f809cc768e32091e39d45bce5f2e95fe43cd67f01d5e60c7df2ea868",
  "atecontroller": "ghcr.io/pilprod/substrate/atecontroller@sha256:0845893ae2ecfd15f580bc410db22c8daae0d6b0388eca67541154a6ec98f554",
  "atenet": "ghcr.io/pilprod/substrate/atenet@sha256:01d96092c93fd623dbe051479a76573da551b56be29121b11b760d9067fc8c4c",
  "releaseVerifier": "ghcr.io/pilprod/substrate/substrate-release-verify@sha256:850d8d8ec018f49486b410a15dd38e965f1fbb4d02f8a8be36d5256f33eef74b"
}'

fail() {
  printf 'Substrate preview pin verifier test failed: %s\n' "$1" >&2
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

expect_auth_failure() {
  local expected="$1"
  local candidate="$2"
  local evidence="$3"
  local evidence_uri="$4"
  local registry_config="${5:-}"
  local output

  if [[ -n "${registry_config}" ]]; then
    if output="$(
      HELM_REGISTRY_CONFIG="${registry_config}" \
        SUBSTRATE_RELEASE_EVIDENCE_URI="${evidence_uri}" \
        SUBSTRATE_RELEASE_EVIDENCE="${evidence}" \
        GOWORK=off \
        GOPROXY=https://proxy.golang.org \
        GOSUMDB=sum.golang.org \
        GOPRIVATE= \
        GONOPROXY= \
        GONOSUMDB= \
        "${verifier}" "${candidate}" 2>&1
    )"; then
      fail "verifier unexpectedly accepted invalid private registry authentication"
    fi
  elif output="$(
    env -u HELM_REGISTRY_CONFIG \
      SUBSTRATE_RELEASE_EVIDENCE_URI="${evidence_uri}" \
      SUBSTRATE_RELEASE_EVIDENCE="${evidence}" \
      GOWORK=off \
      GOPROXY=https://proxy.golang.org \
      GOSUMDB=sum.golang.org \
      GOPRIVATE= \
      GONOPROXY= \
      GONOSUMDB= \
      "${verifier}" "${candidate}" 2>&1
  )"; then
    fail "verifier unexpectedly accepted a missing private registry configuration"
  fi
  grep -F -- "${expected}" <<<"${output}" >/dev/null ||
    fail "expected '${expected}' for the private registry auth contract, got: ${output}"
}

expect_evidence_failure() {
  local expected="$1"
  local candidate="$2"
  local evidence="${3:-}"
  local evidence_uri="${4:-}"
  local -a environment=(
    -u SUBSTRATE_RELEASE_EVIDENCE_URI
    -u SUBSTRATE_RELEASE_EVIDENCE
  )
  local output

  if [[ -n "${evidence_uri}" ]]; then
    environment+=("SUBSTRATE_RELEASE_EVIDENCE_URI=${evidence_uri}")
  fi
  if [[ -n "${evidence}" ]]; then
    environment+=("SUBSTRATE_RELEASE_EVIDENCE=${evidence}")
  fi
  if output="$(
    env "${environment[@]}" \
      GOWORK=off \
      GOPROXY=https://proxy.golang.org \
      GOSUMDB=sum.golang.org \
      GOPRIVATE= \
      GONOPROXY= \
      GONOSUMDB= \
      "${verifier}" "${candidate}" 2>&1
  )"; then
    fail "verifier unexpectedly accepted invalid private release evidence"
  fi
  grep -F -- "${expected}" <<<"${output}" >/dev/null ||
    fail "expected '${expected}' for the private evidence contract, got: ${output}"
}

test -x "${verifier}" || fail "verifier is not executable"
test -f "${pin_file}" || fail "pin manifest does not exist"
python3 "${private_image_verifier_test}" >/dev/null ||
  fail "private Substrate image verifier unit tests failed"

jq -e \
  --arg repository "${expected_chart_repository}" \
  --arg source_commit "e9ed68e587b56df2aa2a7f0267a744598c4d48b4" \
  --arg source_tag_object "${expected_source_tag_object}" \
  --arg source_tree "${expected_source_tree}" \
  --arg chart_tree "${expected_chart_tree}" \
  --argjson source_image_refs "${expected_source_image_refs}" '
    .schemaVersion == 3 and
    (.status == "blocked" or .status == "ready") and
    .helmChart.repository == $repository and
    .helmChart.supportedProfiles == ["external-control-plane-only"] and
    .helmChart.requiredComponents == ["agentgateway", "ateapi", "atecontroller", "atenet"] and
    .helmChart.auxiliaryComponents == ["releaseVerifier"] and
    .helmChart.sourceImageRefs == $source_image_refs and
    .helmChart.version == "0.0.22-private.3" and
    .helmChart.appVersion == "v0.0.22" and
    .helmChart.crds.repository == $repository and
    .helmChart.crds.name == "substrate-crds" and
    .helmChart.crds.version == "0.0.22-private.3" and
    .helmChart.crds.appVersion == "v0.0.22" and
    .helmChart.publication.sourceCommit == $source_commit and
    .helmChart.publication.sourceCommit == .goModule.origin.commit and
    .helmChart.publication.sourceRef == .goModule.origin.ref and
    .helmChart.publication.sourceTagObject == $source_tag_object and
    .helmChart.publication.sourceTree == $source_tree and
    .helmChart.publication.chartTree == $chart_tree and
    (
      if .status == "blocked" then
        (.blocker | type == "string" and length > 0) and
        .helmChart.registryDigest == null and
        .helmChart.packageSha256 == null and
        .helmChart.crds.registryDigest == null and
        .helmChart.crds.packageSha256 == null and
        .helmChart.publication.evidenceUri == null and
        .helmChart.publication.evidenceSha256 == null
      else
        (has("blocker") | not) and
        (.helmChart.registryDigest | test("^sha256:[0-9a-f]{64}$")) and
        (.helmChart.packageSha256 | test("^sha256:[0-9a-f]{64}$")) and
        (.helmChart.crds.registryDigest | test("^sha256:[0-9a-f]{64}$")) and
        (.helmChart.crds.packageSha256 | test("^sha256:[0-9a-f]{64}$")) and
        (.helmChart.publication.evidenceUri |
          test("^gs://yourown-chat-kagent-preview-evidence-europe-west3/substrate/0\\.0\\.22-private\\.3/release-evidence\\.json#[1-9][0-9]*$")) and
        (.helmChart.publication.evidenceSha256 | test("^sha256:[0-9a-f]{64}$"))
      end
    )
  ' "${pin_file}" >/dev/null ||
  fail "private GAR mirror contract drifted"

if grep -E 'GHCR_(TOKEN|USERNAME)|helm registry login|docker login' \
  "${pin_file}" "${verifier}"; then
  fail "Substrate pin verification must not depend on registry credentials or an inline registry login"
fi

blocked_fixture="${tmp_dir}/blocked-fixture.json"
jq '
  .status = "blocked" |
  .blocker = "private Artifact Registry publication evidence is pending" |
  .helmChart.registryDigest = null |
  .helmChart.packageSha256 = null |
  .helmChart.crds.registryDigest = null |
  .helmChart.crds.packageSha256 = null |
  .helmChart.publication.evidenceUri = null |
  .helmChart.publication.evidenceSha256 = null
' "${pin_file}" >"${blocked_fixture}"
expect_failure "pin manifest is intentionally blocked" "${blocked_fixture}"

ready_candidate="${tmp_dir}/ready.json"
evidence="${tmp_dir}/release-evidence.json"
registry_digest="sha256:$(printf 'a%.0s' {1..64})"
package_sha256="sha256:$(printf 'b%.0s' {1..64})"
crds_registry_digest="sha256:$(printf 'c%.0s' {1..64})"
crds_package_sha256="sha256:$(printf 'd%.0s' {1..64})"
chart_ref="${expected_chart_repository}/substrate@${registry_digest}"
crds_chart_ref="${expected_chart_repository}/substrate-crds@${crds_registry_digest}"
jq -n \
  --arg source_commit "e9ed68e587b56df2aa2a7f0267a744598c4d48b4" \
  --arg source_tag_object "${expected_source_tag_object}" \
  --arg source_tree "${expected_source_tree}" \
  --arg chart_tree "${expected_chart_tree}" \
  --arg release_prefix "europe-west3-docker.pkg.dev/yourown-chat/kagent-preview/substrate" \
  --arg release_version "0.0.22-private.3" \
  --arg chart_ref "${chart_ref}" \
  --arg registry_digest "${registry_digest}" \
  --arg package_sha256 "${package_sha256}" \
  --arg crds_chart_ref "${crds_chart_ref}" \
  --arg crds_registry_digest "${crds_registry_digest}" \
  --arg crds_package_sha256 "${crds_package_sha256}" \
  --argjson source_image_refs "${expected_source_image_refs}" '
    {
      schema_version: "yourown.chat/substrate-private-gar-release/v2",
      deployment_class: "dev-to-approved-prod",
      production_eligible: true,
      source: {
        repository: "https://github.com/pilprod/substrate",
        commit: $source_commit,
        tag: "v0.0.22",
        tag_object: $source_tag_object,
        tree: $source_tree,
        chart_tree: $chart_tree
      },
      publication: {
        project_id: "yourown-chat",
        location: "europe-west3",
        registry_visibility: "private",
        build_mode: "copied_exact",
        release_version: $release_version,
        release_prefix: $release_prefix
      },
      supported_profiles: ["external-control-plane-only"],
      required_components: ["agentgateway", "ateapi", "atecontroller", "atenet"],
      auxiliary_components: ["releaseVerifier"],
      copy_provenance: {
        source_image_refs: $source_image_refs
      },
      images: ($source_image_refs | to_entries | map({
        key: .key,
        value: {
          ref: ($release_prefix + "/" + (if .key == "releaseVerifier" then "substrate-release-verify" else .key end) + "@" + (.value | split("@") | last)),
          digest: (.value | split("@") | last)
        }
      }) | from_entries),
      platform_image_digests: {
        agentgateway: {linux_amd64: $registry_digest, linux_arm64: $registry_digest},
        ateapi: {linux_amd64: $registry_digest, linux_arm64: $registry_digest},
        atecontroller: {linux_amd64: $registry_digest, linux_arm64: $registry_digest},
        atenet: {linux_amd64: $registry_digest, linux_arm64: $registry_digest},
        releaseVerifier: {linux_amd64: $registry_digest, linux_arm64: $registry_digest}
      },
      charts: {
        application: {
          release_name: "substrate",
          ref: $chart_ref,
          version: $release_version,
          digest: $registry_digest,
          package_sha256: $package_sha256
        },
        crds: {
          release_name: "substrate-crds",
          ref: $crds_chart_ref,
          version: $release_version,
          digest: $crds_registry_digest,
          package_sha256: $crds_package_sha256
        }
      },
      helm_set_values: {
        "image.registry": $release_prefix,
        "image.digests.ateapi": ($source_image_refs.ateapi | split("@") | last),
        "image.digests.atecontroller": ($source_image_refs.atecontroller | split("@") | last),
        "image.digests.atenet": ($source_image_refs.atenet | split("@") | last),
        "images.agentgateway": ($release_prefix + "/agentgateway@" + ($source_image_refs.agentgateway | split("@") | last))
      },
      scan_policy: {
        platforms: ["linux/amd64", "linux/arm64"],
        blocked_severities: ["HIGH", "CRITICAL"],
        scanner: "Google Artifact Analysis On-Demand Scanning"
      }
    }
  ' >"${evidence}"
evidence_sha256="$(sha256_file "${evidence}")"
jq \
  --arg registry_digest "${registry_digest}" \
  --arg package_sha256 "${package_sha256}" \
  --arg crds_registry_digest "${crds_registry_digest}" \
  --arg crds_package_sha256 "${crds_package_sha256}" \
  --arg evidence_uri "${expected_evidence_uri}" \
  --arg evidence_sha256 "${evidence_sha256}" '
  .status = "ready" |
  del(.blocker) |
  .helmChart.registryDigest = $registry_digest |
  .helmChart.packageSha256 = $package_sha256 |
  .helmChart.crds.registryDigest = $crds_registry_digest |
  .helmChart.crds.packageSha256 = $crds_package_sha256 |
  .helmChart.publication.evidenceUri = $evidence_uri |
  .helmChart.publication.evidenceSha256 = $evidence_sha256
' "${pin_file}" >"${ready_candidate}"

expect_evidence_failure \
  "SUBSTRATE_RELEASE_EVIDENCE_URI is required for a ready private mirror pin" \
  "${ready_candidate}" \
  "${evidence}"

expect_evidence_failure \
  "SUBSTRATE_RELEASE_EVIDENCE is required for a ready private mirror pin" \
  "${ready_candidate}" \
  "" \
  "${expected_evidence_uri}"

expect_evidence_failure \
  "SUBSTRATE_RELEASE_EVIDENCE_URI does not match the declared pin" \
  "${ready_candidate}" \
  "${evidence}" \
  "${expected_evidence_uri%#*}#9999999999"

wrong_evidence_sha_candidate="${tmp_dir}/wrong-evidence-sha.json"
jq '.helmChart.publication.evidenceSha256 = ("sha256:" + ("d" * 64))' \
  "${ready_candidate}" >"${wrong_evidence_sha_candidate}"
expect_evidence_failure \
  "private Substrate evidence SHA-256 does not match the declared pin" \
  "${wrong_evidence_sha_candidate}" \
  "${evidence}" \
  "${expected_evidence_uri}"

wrong_evidence_uri_candidate="${tmp_dir}/wrong-evidence-uri.json"
jq '.helmChart.publication.evidenceUri = "gs://wrong-bucket/substrate/0.0.22-private.3/release-evidence.json#1234567890"' \
  "${ready_candidate}" >"${wrong_evidence_uri_candidate}"
expect_evidence_failure \
  "private Substrate evidence URI does not match the release coordinate" \
  "${wrong_evidence_uri_candidate}" \
  "${evidence}" \
  "gs://wrong-bucket/substrate/0.0.22-private.3/release-evidence.json#1234567890"

ready_with_blocker="${tmp_dir}/ready-with-blocker.json"
jq '.blocker = "stale publication blocker"' \
  "${ready_candidate}" >"${ready_with_blocker}"
expect_failure \
  "pin manifest semantic identity contract is malformed" \
  "${ready_with_blocker}"

declare -a ready_pin_mutations=(
  '.helmChart.version = "0.0.22-private.4"'
  '.helmChart.appVersion = "v0.0.21"'
  '.helmChart.registryDigest = null'
  '.helmChart.packageSha256 = null'
  '.helmChart.crds.version = "0.0.22-private.4"'
  '.helmChart.crds.appVersion = "v0.0.21"'
  '.helmChart.crds.registryDigest = null'
  '.helmChart.crds.packageSha256 = null'
)
for index in "${!ready_pin_mutations[@]}"; do
  candidate="${tmp_dir}/tampered-ready-pin-${index}.json"
  jq "${ready_pin_mutations[$index]}" "${ready_candidate}" >"${candidate}"
  expect_failure "pin manifest is malformed or versions are incompatible" "${candidate}"
done

declare -a evidence_mutations=(
  '.schema_version = "yourown.chat/substrate-private-gar-release/v1"'
  '.unexpected = true'
  '.deployment_class = "preview-only"'
  '.production_eligible = false'
  '.supported_profiles = ["standard"]'
  '.supported_profiles += ["standard"]'
  '.required_components = ["ateapi", "atecontroller", "atenet"]'
  '.required_components += ["ateom-microvm"]'
  'del(.auxiliary_components[0])'
  '.auxiliary_components += ["ateom-microvm"]'
  '.auxiliary_components = ["release-verifier"]'
  '.source.commit = ("f" * 40)'
  '.source.tag = "v0.0.21"'
  '.source.tag_object = ("f" * 40)'
  '.source.tree = ("f" * 40)'
  '.source.chart_tree = ("f" * 40)'
  '.source.unexpected = true'
  '.publication.build_mode = "rebuilt"'
  '.publication.release_version = "0.0.22-private.4"'
  '.publication.unexpected = true'
  '.charts.application.ref = "oci://example.invalid/substrate@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
  '.charts.application.release_name = "not-substrate"'
  '.charts.application.version = "0.0.22-private.4"'
  '.charts.application.digest = ("sha256:" + ("e" * 64))'
  '.charts.application.package_sha256 = ("sha256:" + ("f" * 64))'
  'del(.charts.crds)'
  '.charts.extra = .charts.crds'
  '.charts.crds.release_name = "not-substrate-crds"'
  '.charts.crds.ref = "oci://example.invalid/substrate-crds@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"'
  '.charts.crds.version = "0.0.22-private.4"'
  '.charts.crds.digest = ("sha256:" + ("e" * 64))'
  '.charts.crds.package_sha256 = ("sha256:" + ("f" * 64))'
  '.charts.crds.unexpected = true'
  'del(.helm_set_values["image.registry"])'
  '.helm_set_values.extra = "unreviewed"'
  '.helm_set_values["image.registry"] = "example.invalid/substrate"'
  '.helm_set_values["image.digests.ateapi"] = ("sha256:" + ("f" * 64))'
  '.helm_set_values["images.agentgateway"] = .copy_provenance.source_image_refs.agentgateway'
  '.helm_set_values["image.digests.releaseVerifier"] = .images.releaseVerifier.digest'
  '.scan_policy.platforms = ["linux/amd64"]'
  '.scan_policy.blocked_severities = ["CRITICAL"]'
  '.scan_policy.scanner = "unreviewed"'
  '.scan_policy.unexpected = true'
  'del(.copy_provenance.source_image_refs.agentgateway)'
  'del(.copy_provenance.source_image_refs.releaseVerifier)'
  '.copy_provenance.source_image_refs["ateom-microvm"] = .copy_provenance.source_image_refs.ateapi'
  '.copy_provenance.source_image_refs.ateapi = "ghcr.io/pilprod/substrate/ateapi@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
  'del(.images.agentgateway)'
  'del(.images.releaseVerifier)'
  '.images["ateom-microvm"] = .images.ateapi'
  '.images.ateapi.ref = "europe-west3-docker.pkg.dev/yourown-chat/kagent-preview/substrate/not-ateapi@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
  '.images.releaseVerifier.ref = .copy_provenance.source_image_refs.releaseVerifier'
  '.images.releaseVerifier.ref = "europe-west3-docker.pkg.dev/yourown-chat/kagent-preview/substrate/substrate-release-verify:0.0.22-private.3"'
  '.images.releaseVerifier.ref = "us-docker.pkg.dev/yourown-chat/kagent-preview/substrate/substrate-release-verify@sha256:850d8d8ec018f49486b410a15dd38e965f1fbb4d02f8a8be36d5256f33eef74b"'
  '.images.releaseVerifier.ref = "europe-west3-docker.pkg.dev/yourown-chat/kagent-preview/substrate/releaseVerifier@sha256:850d8d8ec018f49486b410a15dd38e965f1fbb4d02f8a8be36d5256f33eef74b"'
  '.images.ateapi.digest = "sha256:invalid"'
  '.images.ateapi.digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" | .images.ateapi.ref = "europe-west3-docker.pkg.dev/yourown-chat/kagent-preview/substrate/ateapi@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
  'del(.platform_image_digests.agentgateway.linux_arm64)'
  'del(.platform_image_digests.releaseVerifier)'
  '.platform_image_digests["ateom-microvm"] = .platform_image_digests.ateapi'
)
for index in "${!evidence_mutations[@]}"; do
  tampered_evidence="${tmp_dir}/tampered-evidence-${index}.json"
  tampered_candidate="${tmp_dir}/tampered-evidence-pin-${index}.json"
  jq "${evidence_mutations[$index]}" "${evidence}" >"${tampered_evidence}"
  tampered_evidence_sha256="$(sha256_file "${tampered_evidence}")"
  jq --arg evidence_sha256 "${tampered_evidence_sha256}" \
    '.helmChart.publication.evidenceSha256 = $evidence_sha256' \
    "${ready_candidate}" >"${tampered_candidate}"
  expect_evidence_failure \
    "private Substrate evidence content does not match the declared pin" \
    "${tampered_candidate}" \
    "${tampered_evidence}" \
    "${expected_evidence_uri}"
done

expect_auth_failure \
  "HELM_REGISTRY_CONFIG is required for the private Artifact Registry chart" \
  "${ready_candidate}" \
  "${evidence}" \
  "${expected_evidence_uri}"

invalid_registry_config="${tmp_dir}/invalid-registry-config.json"
printf '{"auths":{"europe-west3-docker.pkg.dev":{"auth":"%s"}}}\n' \
  "$(printf 'static-user:static-password' | base64 | tr -d '\n')" \
  >"${invalid_registry_config}"
chmod 0600 "${invalid_registry_config}"
expect_auth_failure \
  "HELM_REGISTRY_CONFIG must contain short-lived GCP authentication" \
  "${ready_candidate}" \
  "${evidence}" \
  "${expected_evidence_uri}" \
  "${invalid_registry_config}"

empty_token_registry_config="${tmp_dir}/empty-token-registry-config.json"
printf '{"auths":{"europe-west3-docker.pkg.dev":{"auth":"%s"}}}\n' \
  "$(printf 'oauth2accesstoken:' | base64 | tr -d '\n')" \
  >"${empty_token_registry_config}"
chmod 0600 "${empty_token_registry_config}"
expect_auth_failure \
  "HELM_REGISTRY_CONFIG must contain short-lived GCP authentication" \
  "${ready_candidate}" \
  "${evidence}" \
  "${expected_evidence_uri}" \
  "${empty_token_registry_config}"

valid_registry_config="${tmp_dir}/valid-registry-config.json"
printf '{"auths":{"europe-west3-docker.pkg.dev":{"auth":"%s"}}}\n' \
  "$(printf 'oauth2accesstoken:short-lived-test-token' | base64 | tr -d '\n')" \
  >"${valid_registry_config}"
chmod 0600 "${valid_registry_config}"
if output="$(
  HELM_REGISTRY_CONFIG="${valid_registry_config}" \
    SUBSTRATE_RELEASE_EVIDENCE_URI="${expected_evidence_uri}" \
    SUBSTRATE_RELEASE_EVIDENCE="${evidence}" \
    GOWORK=invalid \
    GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org \
    GOPRIVATE= \
    GONOPROXY= \
    GONOSUMDB= \
    "${verifier}" "${ready_candidate}" 2>&1
)"; then
  fail "verifier unexpectedly passed the non-release Go workspace contract"
fi
grep -F -- "GOWORK must be explicitly set to off" <<<"${output}" >/dev/null ||
  fail "verifier did not accept the short-lived GCP registry configuration: ${output}"

fake_bin="${tmp_dir}/fake-bin"
mkdir -p "${fake_bin}"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'printf "offline fake python invoked: %s\\n" "$*" >&2' \
  'exit 73' \
  >"${fake_bin}/python3"
chmod 0700 "${fake_bin}/python3"
if output="$(
  PATH="${fake_bin}:${PATH}" \
    HELM_REGISTRY_CONFIG="${valid_registry_config}" \
    SUBSTRATE_RELEASE_EVIDENCE_URI="${expected_evidence_uri}" \
    SUBSTRATE_RELEASE_EVIDENCE="${evidence}" \
    GOWORK=off \
    GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org \
    GOPRIVATE= \
    GONOPROXY= \
    GONOSUMDB= \
    "${verifier}" "${ready_candidate}" 2>&1
)"; then
  fail "verifier unexpectedly skipped the live private image verification"
fi
grep -F -- "offline fake python invoked: ${root_dir}/scripts/verify-substrate-private-images.py" \
  <<<"${output}" >/dev/null ||
  fail "verifier did not invoke the private image verifier: ${output}"
grep -F -- "--evidence ${evidence} --registry-config ${valid_registry_config}" \
  <<<"${output}" >/dev/null ||
  fail "verifier did not bind image verification to evidence and registry auth: ${output}"

application_package_fixture="${tmp_dir}/application-chart.tgz"
crds_package_fixture="${tmp_dir}/crds-chart.tgz"
printf 'application chart fixture\n' >"${application_package_fixture}"
printf 'crds chart fixture\n' >"${crds_package_fixture}"
live_application_package_sha256="$(sha256_file "${application_package_fixture}")"
live_crds_package_sha256="$(sha256_file "${crds_package_fixture}")"
live_evidence="${tmp_dir}/live-release-evidence.json"
jq \
  --arg application_sha "${live_application_package_sha256}" \
  --arg crds_sha "${live_crds_package_sha256}" '
    .charts.application.package_sha256 = $application_sha |
    .charts.crds.package_sha256 = $crds_sha
  ' "${evidence}" >"${live_evidence}"
live_evidence_sha256="$(sha256_file "${live_evidence}")"
live_candidate="${tmp_dir}/live-ready.json"
jq \
  --arg application_sha "${live_application_package_sha256}" \
  --arg crds_sha "${live_crds_package_sha256}" \
  --arg evidence_sha "${live_evidence_sha256}" '
    .helmChart.packageSha256 = $application_sha |
    .helmChart.crds.packageSha256 = $crds_sha |
    .helmChart.publication.evidenceSha256 = $evidence_sha
  ' "${ready_candidate}" >"${live_candidate}"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'printf "offline fake python accepted: %s\\n" "$*" >&2' \
  'exit 0' \
  >"${fake_bin}/python3"
fake_helm_log="${tmp_dir}/fake-helm.log"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'printf "%s\\n" "$*" >>"${FAKE_HELM_LOG}"' \
  'operation="$1"' \
  'if [[ "${operation}" == "show" ]]; then' \
  '  ref="$3"' \
  '  if [[ "${ref}" == */substrate-crds ]]; then' \
  '    printf "Digest: %s\\nname: substrate-crds\\nversion: 0.0.22-private.3\\nappVersion: v0.0.22\\n" "${FAKE_CRDS_DIGEST}"' \
  '  else' \
  '    printf "Digest: %s\\nname: substrate\\nversion: 0.0.22-private.3\\nappVersion: v0.0.22\\n" "${FAKE_APPLICATION_DIGEST}"' \
  '  fi' \
  '  exit 0' \
  'fi' \
  'if [[ "${operation}" == "pull" ]]; then' \
  '  ref="$2"' \
  '  destination=""' \
  '  while [[ "$#" -gt 0 ]]; do' \
  '    if [[ "$1" == "--destination" ]]; then destination="$2"; break; fi' \
  '    shift' \
  '  done' \
  '  [[ -n "${destination}" ]]' \
  '  if [[ "${ref}" == */substrate-crds ]]; then' \
  '    cp "${FAKE_CRDS_PACKAGE}" "${destination}/substrate-crds-0.0.22-private.3.tgz"' \
  '  else' \
  '    cp "${FAKE_APPLICATION_PACKAGE}" "${destination}/substrate-0.0.22-private.3.tgz"' \
  '  fi' \
  '  exit 0' \
  'fi' \
  'exit 64' \
  >"${fake_bin}/helm"
chmod 0700 "${fake_bin}/helm" "${fake_bin}/python3"

if ! output="$(
  PATH="${fake_bin}:${PATH}" \
    FAKE_HELM_LOG="${fake_helm_log}" \
    FAKE_APPLICATION_DIGEST="${registry_digest}" \
    FAKE_CRDS_DIGEST="${crds_registry_digest}" \
    FAKE_APPLICATION_PACKAGE="${application_package_fixture}" \
    FAKE_CRDS_PACKAGE="${crds_package_fixture}" \
    HELM_REGISTRY_CONFIG="${valid_registry_config}" \
    SUBSTRATE_RELEASE_EVIDENCE_URI="${expected_evidence_uri}" \
    SUBSTRATE_RELEASE_EVIDENCE="${live_evidence}" \
    GOWORK=off \
    GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org \
    GOPRIVATE= \
    GONOPROXY= \
    GONOSUMDB= \
    "${verifier}" "${live_candidate}" 2>&1
)"; then
  fail "verifier rejected the complete dual-chart live contract: ${output}"
fi
for expected_helm_call in \
  "show chart ${expected_chart_repository}/substrate --version 0.0.22-private.3" \
  "show chart ${expected_chart_repository}/substrate-crds --version 0.0.22-private.3" \
  "pull ${expected_chart_repository}/substrate --version 0.0.22-private.3" \
  "pull ${expected_chart_repository}/substrate-crds --version 0.0.22-private.3"; do
  grep -F -- "${expected_helm_call}" "${fake_helm_log}" >/dev/null ||
    fail "live Helm verification did not execute: ${expected_helm_call}"
done

blocked_candidate="${tmp_dir}/blocked.json"
jq '
  .helmChart.publication.sourceCommit = ("f" * 40)
' "${blocked_fixture}" >"${blocked_candidate}"
expect_failure \
  "blocked pin manifest must bind the pending private mirror" \
  "${blocked_candidate}"

blocked_with_crds_digest="${tmp_dir}/blocked-with-crds-digest.json"
jq '.helmChart.crds.registryDigest = ("sha256:" + ("f" * 64))' \
  "${blocked_fixture}" >"${blocked_with_crds_digest}"
expect_failure \
  "blocked pin manifest must bind the pending private mirror" \
  "${blocked_with_crds_digest}"

declare -a mutations=(
  '.unexpected = true'
  '.schemaVersion = 2'
  '.requiredCapabilities[1] = "pkg/api/v1alpha1.WorkerProviderContainer"'
  '.lastIncompatiblePublicRelease.unexpected = true'
  '.goModule.unexpected = true'
  '.goModule.requiredPath = "example.invalid/substrate"'
  '.goModule.replacement.unexpected = true'
  '.goModule.replacement.path = "github.com/example/substrate"'
  '.goModule.origin.unexpected = true'
  '.goModule.origin.url = "https://github.com/example/substrate"'
  '.helmChart.repository = "oci://example.invalid/substrate/helm"'
  '.helmChart.name = "not-substrate"'
  '.helmChart.unexpected = true'
  '.helmChart.supportedProfiles = ["standard"]'
  '.helmChart.requiredComponents += ["ateom-microvm"]'
  'del(.helmChart.auxiliaryComponents[0])'
  '.helmChart.auxiliaryComponents += ["ateom-microvm"]'
  '.helmChart.auxiliaryComponents = ["release-verifier"]'
  '.helmChart.sourceImageRefs.ateapi = "ghcr.io/pilprod/substrate/ateapi@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
  'del(.helmChart.sourceImageRefs.releaseVerifier)'
  '.helmChart.sourceImageRefs.releaseVerifier = "ghcr.io/pilprod/substrate/releaseVerifier@sha256:850d8d8ec018f49486b410a15dd38e965f1fbb4d02f8a8be36d5256f33eef74b"'
  '.helmChart.publication.sourceTagObject = ("f" * 40)'
  '.helmChart.publication.sourceTree = ("f" * 40)'
  '.helmChart.publication.chartTree = ("f" * 40)'
  '.helmChart.publication.unexpected = true'
  '.helmChart.crds.repository = "oci://example.invalid/substrate/helm"'
  '.helmChart.crds.name = "not-substrate-crds"'
  '.helmChart.crds.unexpected = true'
  'del(.helmChart.crds)'
)

for index in "${!mutations[@]}"; do
  candidate="${tmp_dir}/tampered-${index}.json"
  jq "${mutations[$index]}" "${pin_file}" >"${candidate}"
  expect_failure "pin manifest semantic identity contract is malformed" "${candidate}"
done

printf 'Substrate preview pin verifier semantic identity tests passed\n'
