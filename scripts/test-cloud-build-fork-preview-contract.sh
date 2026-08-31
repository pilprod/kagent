#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
config="${root_dir}/cloudbuild.fork-preview.yaml"
toolbox="${root_dir}/.github/cloud-build/fork-preview-tools.Dockerfile"
chart_script="${root_dir}/scripts/cloud-build-fork-preview-charts.sh"
assembler="${root_dir}/scripts/assemble-fork-preview-release-evidence.py"
receipt_finalizer="${root_dir}/scripts/finalize-cloud-build-fork-preview-receipt.py"
source_verifier="${root_dir}/scripts/verify-cloud-build-fork-preview-source.sh"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/kagent-cloud-build-contract.XXXXXX")"
trap 'rm -rf -- "${tmp_dir}"' EXIT

fail() {
  printf 'Cloud Build fork preview contract test failed: %s\n' "$1" >&2
  exit 1
}

for path in \
  "${config}" \
  "${toolbox}" \
  "${chart_script}" \
  "${assembler}" \
  "${receipt_finalizer}" \
  "${source_verifier}"; do
  test -f "${path}" || fail "required file is missing: ${path}"
done

python3 - \
  "${config}" "${toolbox}" "${chart_script}" \
  "${assembler}" "${receipt_finalizer}" "${source_verifier}" <<'PY'
import pathlib
import re
import sys

config_path, toolbox_path, chart_path, assembler_path, finalizer_path, source_path = map(
    pathlib.Path, sys.argv[1:]
)
config = config_path.read_text(encoding="utf-8")
toolbox = toolbox_path.read_text(encoding="utf-8")
chart_script = chart_path.read_text(encoding="utf-8")
assembler = assembler_path.read_text(encoding="utf-8")
finalizer = finalizer_path.read_text(encoding="utf-8")
source_verifier = source_path.read_text(encoding="utf-8")

required_config = (
    "timeout: 60s\n",
    "logging: CLOUD_LOGGING_ONLY",
    "reject-retired-public-ghcr-rail",
    "This public GHCR fallback is retired.",
    "private app-gcp Pub/Sub and GAR release rail",
    "        exit 1\n",
)
for fragment in required_config:
    if fragment not in config:
        raise SystemExit(f"missing retired Cloud Build guard fragment: {fragment}")

step_ids = re.findall(r"(?m)^  - id: ([a-z0-9-]+)$", config)
if step_ids != ["reject-retired-public-ghcr-rail"]:
    raise SystemExit(f"retired public Cloud Build config must contain one rejecting step: {step_ids}")
for forbidden in (
    "allowFailure",
    "allowExitCodes",
    "availableSecrets:",
    "secretEnv:",
    "GHCR_TOKEN",
    "ghcr.io",
    "docker login",
    "imagetools",
    "helm push",
    "gcloud storage cp",
):
    if forbidden in config:
        raise SystemExit(f"retired public Cloud Build config still contains publication capability: {forbidden}")

if not re.search(
    r'merge-base --is-ancestor\s+\\?\s*"\$\{source_commit\}"\s+origin/yourown-chat',
    source_verifier,
):
    raise SystemExit("private app-gcp source verifier has the wrong ancestry direction")
for fragment in (
    "actions/workflows?per_page=100",
    ".id == 346150199",
    '.name == "Fork immutable preview release"',
    ".id == 340304832",
    '.name == "Tag and Push"',
    '.state == "disabled_manually"',
    "public GitHub release workflows are not disabled_manually",
):
    if fragment not in source_verifier:
        raise SystemExit(f"private release source verifier lost workflow-state gate: {fragment}")

from_lines = [line for line in toolbox.splitlines() if line.startswith("FROM ")]
if len(from_lines) != 4:
    raise SystemExit("private release toolbox must have exactly four pinned stages")
for line in from_lines:
    if not re.search(r"@sha256:[0-9a-f]{64}(?: AS [a-z]+)?$", line):
        raise SystemExit(f"toolbox base is not digest pinned: {line}")

for fragment in (
    'if [[ "${action}" != "package" ]]',
    "package-fork-preview-charts.sh",
    "  cmp \\",
    "public chart publication is retired",
):
    if fragment not in chart_script:
        raise SystemExit(f"private chart packager lost reproducibility control: {fragment}")
for forbidden in ("GHCR_TOKEN", "GHCR_USERNAME", "ghcr.io", "helm push"):
    if forbidden in chart_script:
        raise SystemExit(f"private chart packager retained public publication capability: {forbidden}")

for fragment in (
    '"image_refs"',
    '"controller"',
    '"ui"',
    '"runtime_images"',
    '"kagentHarness"',
    '"codexHarness"',
    '"application"',
    '"crds"',
    '"skills_init_removal_commit"',
):
    if fragment not in assembler:
        raise SystemExit(f"evidence assembler lost field: {fragment}")
if "claudeHarness" in assembler:
    raise SystemExit("private release evidence must not activate Claude Harness")
if '"google-cloud-build"' not in finalizer:
    raise SystemExit("Cloud Build receipt identity is missing")
for fragment in (
    'f"v{version}"',
    'f"gcp-v{version}"',
    '"artifact_tag": artifact_tag',
    '"source_tag": args.source_tag',
):
    if fragment not in finalizer:
        raise SystemExit(f"Cloud Build source-tag contract is missing: {fragment}")
PY

mkdir -p "${tmp_dir}/digests" "${tmp_dir}/charts"
digest="sha256:$(printf test | sha256sum | awk '{ print $1 }')"
for component in controller ui golang-adk codex-harness; do
  printf '%s=%s\n' "${component}" "${digest}" \
    > "${tmp_dir}/digests/image-${component}.txt"
done
for component in kagent kagent-crds; do
  printf '%s=%s\n' "${component}" "${digest}" \
    > "${tmp_dir}/digests/chart-${component}.txt"
  printf 'chart fixture\n' > \
    "${tmp_dir}/charts/${component}-0.0.0-test.kap.1.tgz"
done

head="$(git -C "${root_dir}" rev-parse HEAD)"
python3 "${assembler}" \
  0.0.0-test.kap.1 \
  "${head}" \
  "${tmp_dir}/digests" \
  "${tmp_dir}/charts" \
  "${tmp_dir}/release"
python3 "${receipt_finalizer}" \
  test-build-1 \
  test-project \
  "${head}" \
  0.0.0-test.kap.1 \
  gcp-v0.0.0-test.kap.1 \
  "${tmp_dir}/release"
(cd "${tmp_dir}/release" && sha256sum --check SHA256SUMS)
(cd "${tmp_dir}/release" && sha256sum --check release-evidence.json.sha256)

python3 - \
  "${tmp_dir}/release/release-evidence.json" \
  "${tmp_dir}/release/cloud-build-receipt.json" \
  "${head}" <<'PY'
import json
import pathlib
import sys

evidence = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
receipt = json.loads(pathlib.Path(sys.argv[2]).read_text(encoding="utf-8"))
head = sys.argv[3]

if evidence["schemaVersion"] != 3 or evidence["source_commit"] != head:
    raise SystemExit("wrong deployment evidence identity")
if sorted(evidence["image_refs"]) != ["controller", "ui"]:
    raise SystemExit("wrong deployment image set")
if sorted(evidence["runtime_images"]) != ["codexHarness", "kagentHarness"]:
    raise SystemExit("wrong separately activated runtime image set")
if sorted(evidence["charts"]) != ["application", "crds"]:
    raise SystemExit("wrong chart set")
if receipt["schemaVersion"] != 2 or receipt["source_commit"] != head:
    raise SystemExit("wrong Cloud Build receipt identity")
if receipt["version"] != "0.0.0-test.kap.1":
    raise SystemExit("wrong receipt artifact version")
if receipt["artifact_tag"] != "v0.0.0-test.kap.1":
    raise SystemExit("wrong receipt artifact tag")
if receipt["source_tag"] != "gcp-v0.0.0-test.kap.1":
    raise SystemExit("wrong receipt source tag")
PY

for rejected_tag in \
  gcp-v0.0.0-test.kap.2 \
  release-0.0.0-test.kap.1; do
  suffix="$(printf '%s' "${rejected_tag}" | tr -c '[:alnum:]' '-')"
  rejected_release="${tmp_dir}/rejected-${suffix}"
  python3 "${assembler}" \
    0.0.0-test.kap.1 \
    "${head}" \
    "${tmp_dir}/digests" \
    "${tmp_dir}/charts" \
    "${rejected_release}"
  if python3 "${receipt_finalizer}" \
    test-build-rejected \
    test-project \
    "${head}" \
    0.0.0-test.kap.1 \
    "${rejected_tag}" \
    "${rejected_release}" >/dev/null 2>&1; then
    fail "receipt finalizer accepted invalid source tag ${rejected_tag}"
  fi
done

printf 'Private app-gcp helpers and retired public Cloud Build guard passed\n'
