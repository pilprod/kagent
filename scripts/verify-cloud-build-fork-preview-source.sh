#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s VERSION SOURCE_COMMIT\n' "$0" >&2
  exit 2
fi

version="$1"
source_commit="$2"
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$ ]]; then
  printf 'fork preview version is invalid: %s\n' "${version}" >&2
  exit 1
fi
if [[ ! "${source_commit}" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'source commit is invalid: %s\n' "${source_commit}" >&2
  exit 1
fi

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd "${script_directory}/.." && pwd -P)"

test "$(go version | awk '{ print $3 }')" = "go1.27.0"
test "$(helm version --short)" = "v3.21.4+g813176c"
test "$(jq --version)" = "jq-1.8.2"
test "$(git -C "${repository_root}" rev-parse HEAD)" = "${source_commit}"
test "$(git -C "${repository_root}" remote get-url origin)" = \
  "https://github.com/pilprod/kagent.git"
git -C "${repository_root}" merge-base --is-ancestor \
  "${source_commit}" origin/yourown-chat
test -z "$(git -C "${repository_root}" status --porcelain)"
test ! -e "${repository_root}/go.work"
test ! -e "${repository_root}/go/go.work"
command -v curl >/dev/null 2>&1

workflow_state="$({
  curl --fail --silent --show-error \
    --proto '=https' \
    --tlsv1.2 \
    --header 'Accept: application/vnd.github+json' \
    --header 'X-GitHub-Api-Version: 2022-11-28' \
    'https://api.github.com/repos/pilprod/kagent/actions/workflows?per_page=100'
})"
jq -e '
  ([.workflows[] |
    select(.id == 346150199 and
      .name == "Retired public fork preview release" and
      .state == "disabled_manually")] | length) == 1 and
  ([.workflows[] |
    select(.id == 340304832 and
      .name == "Tag and Push" and
      .state == "disabled_manually")] | length) == 1
' <<<"${workflow_state}" >/dev/null || {
  printf 'public GitHub release workflows are not disabled_manually\n' >&2
  exit 1
}

test -n "${HELM_REGISTRY_CONFIG:-}"
test -r "${HELM_REGISTRY_CONFIG}"
test -n "${SUBSTRATE_RELEASE_EVIDENCE_URI:-}"
test -n "${SUBSTRATE_RELEASE_EVIDENCE:-}"
test -r "${SUBSTRATE_RELEASE_EVIDENCE}"

"${script_directory}/test-verify-gke-preview-substrate-pin.sh"
GOWORK=off \
GOPROXY=https://proxy.golang.org \
GOSUMDB=sum.golang.org \
GOPRIVATE= \
GONOPROXY= \
GONOSUMDB= \
HELM_REGISTRY_CONFIG="${HELM_REGISTRY_CONFIG}" \
SUBSTRATE_RELEASE_EVIDENCE_URI="${SUBSTRATE_RELEASE_EVIDENCE_URI}" \
SUBSTRATE_RELEASE_EVIDENCE="${SUBSTRATE_RELEASE_EVIDENCE}" \
  "${script_directory}/verify-gke-preview-substrate-pin.sh"

go_digest='sha256:f83f9c10f9c18d7b9a71d241b63b8824de5a0ad6caea4255406e42b4005320fe'
wolfi_digest='sha256:19f7a7b40a11c435311e3784bd134c6b6f19677462440da48f96d5c84eefd669'
alpine_image='alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce'
codex_version='0.151.0'
codex_amd64_sha256='605b4b183f22c645f5def63a5b7191767407fb66a6feaec4eaf10b5b7e0058f6'
codex_arm64_sha256='c1cf2baf375e261c1469381a52dc2c8fd05b6fb45cfff83fed0988fd6c5369b6'
claude_version='2.1.236'
claude_amd64_sha256='979acf4877fa3dca24d6d15043022b5005cc8502fdb4c68992dd91651af31731'
claude_arm64_sha256='56fe9241267b0187538a9cf7dafa0df12d94fd27789fa7207a59c2a0b4121b8f'
grep -Fx "ARG GO_BUILDER_DIGEST=${go_digest}" "${repository_root}/go/Dockerfile"
grep -Fx "ARG ALPINE_IMAGE=${alpine_image}" "${repository_root}/go/Dockerfile"
grep -Fx "ARG WOLFI_BASE_DIGEST=${wolfi_digest}" "${repository_root}/ui/Dockerfile"
grep -Fx "ARG GO_BUILDER_DIGEST=${go_digest}" "${repository_root}/go/harness/codex/Dockerfile"
grep -Fx "ARG ALPINE_IMAGE=${alpine_image}" "${repository_root}/go/harness/codex/Dockerfile"
grep -Fx "ARG CODEX_VERSION=${codex_version}" "${repository_root}/go/harness/codex/Dockerfile"
grep -Fx "ARG CODEX_AMD64_ARCHIVE_SHA256=${codex_amd64_sha256}" "${repository_root}/go/harness/codex/Dockerfile"
grep -Fx "ARG CODEX_ARM64_ARCHIVE_SHA256=${codex_arm64_sha256}" "${repository_root}/go/harness/codex/Dockerfile"
grep -Fx "ARG GO_BUILDER_DIGEST=${go_digest}" "${repository_root}/go/harness/claude/Dockerfile"
grep -Fx "ARG ALPINE_IMAGE=${alpine_image}" "${repository_root}/go/harness/claude/Dockerfile"
grep -Fx "ARG CLAUDE_CODE_VERSION=${claude_version}" "${repository_root}/go/harness/claude/Dockerfile"
grep -Fx "ARG CLAUDE_CODE_AMD64_SHA256=${claude_amd64_sha256}" "${repository_root}/go/harness/claude/Dockerfile"
grep -Fx "ARG CLAUDE_CODE_ARM64_SHA256=${claude_arm64_sha256}" "${repository_root}/go/harness/claude/Dockerfile"
if grep -E '^FROM .*:(latest|[0-9]+([.][0-9]+)*)[[:space:]]' \
  "${repository_root}/go/Dockerfile" \
  "${repository_root}/ui/Dockerfile" \
  "${repository_root}/go/harness/codex/Dockerfile" \
  "${repository_root}/go/harness/claude/Dockerfile"; then
  printf 'release Dockerfile has an unpinned base image\n' >&2
  exit 1
fi

"${script_directory}/test-fork-preview-release-contract.sh"
"${script_directory}/test-cloud-build-fork-preview-contract.sh"

export GOWORK=off
export GOFLAGS=-mod=readonly
export GOPROXY=https://proxy.golang.org
export GOSUMDB=sum.golang.org
export GOPRIVATE=
export GONOPROXY=
export GONOSUMDB=
(cd "${repository_root}/go" && go mod download)
envtest_assets="$(cd "${repository_root}/go" && make -s envtest-path | tail -n1)"
test -n "${envtest_assets}"
test -x "${envtest_assets}/kube-apiserver"
test -x "${envtest_assets}/etcd"
export KUBEBUILDER_ASSETS="${envtest_assets}"
(cd "${repository_root}/go" && go test \
  ./harness/codex/... \
  ./harness/claude/... \
  ./harness/runtime/... \
  ./core/v2/translator/codingagent \
  ./core/v2/translator/codex \
  ./core/v2/translator/claude \
  ./core/v2/translator/kagent \
  ./core/v2/controller \
  ./api/v1alpha2 \
  ./api/v1alpha3)
(cd "${repository_root}/go" && go vet \
  ./harness/codex/... \
  ./harness/claude/... \
  ./harness/runtime/... \
  ./core/v2/translator/codingagent \
  ./core/v2/translator/codex \
  ./core/v2/translator/claude \
  ./core/v2/translator/kagent)
