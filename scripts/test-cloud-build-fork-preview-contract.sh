#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
config="${root_dir}/cloudbuild.fork-preview.yaml"
toolbox="${root_dir}/.github/cloud-build/fork-preview-tools.Dockerfile"
image_script="${root_dir}/scripts/cloud-build-fork-preview-images.sh"
chart_script="${root_dir}/scripts/cloud-build-fork-preview-charts.sh"
registry_verifier="${root_dir}/scripts/verify-ghcr-fork-preview.py"
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
  "${image_script}" \
  "${chart_script}" \
  "${registry_verifier}" \
  "${assembler}" \
  "${receipt_finalizer}" \
  "${source_verifier}"; do
  test -f "${path}" || fail "required file is missing: ${path}"
done

python3 - \
  "${config}" "${toolbox}" "${image_script}" \
  "${registry_verifier}" "${assembler}" "${receipt_finalizer}" \
  "${source_verifier}" <<'PY'
import pathlib
import re
import runpy
import sys
import tempfile

config_path, toolbox_path, image_path, registry_path, assembler_path, finalizer_path, source_path = map(
    pathlib.Path, sys.argv[1:]
)
config = config_path.read_text(encoding="utf-8")
toolbox = toolbox_path.read_text(encoding="utf-8")
images = image_path.read_text(encoding="utf-8")
assembler = assembler_path.read_text(encoding="utf-8")
finalizer = finalizer_path.read_text(encoding="utf-8")
source_verifier = source_path.read_text(encoding="utf-8")

required_config = (
    "checkout-reviewed-source",
    "reject-existing-final-refs",
    "build-candidate-images",
    "record-buildkit-image-digests",
    "verify-candidate-image-indexes",
    "package-and-reproduce-charts",
    "recheck-final-image-refs",
    "promote-final-image-aliases",
    "recheck-final-chart-refs",
    "publish-final-charts",
    "verify-all-final-registry-digests",
    "assemble-deployment-evidence",
    "verify-release-receipt",
    "upload-release-receipt",
    "availableSecrets:",
    "GHCR_TOKEN",
    "CLOUD_LOGGING_ONLY",
    "finalize-cloud-build-fork-preview-receipt.py",
)
for fragment in required_config:
    if fragment not in config:
        raise SystemExit(f"missing Cloud Build fragment: {fragment}")

step_ids = re.findall(r"(?m)^  - id: ([a-z0-9-]+)$", config)
ordered_steps = (
    "build-candidate-images",
    "record-buildkit-image-digests",
    "verify-candidate-image-indexes",
    "recheck-final-image-refs",
    "promote-final-image-aliases",
    "verify-all-final-registry-digests",
)
positions = [step_ids.index(step) for step in ordered_steps]
if positions != sorted(positions):
    raise SystemExit(
        "candidate images must be recorded and platform-verified before final promotion"
    )

for forbidden in ("git tag ", "git push ", "gh release "):
    if forbidden in config:
        raise SystemExit(f"Cloud Build must not finalize source/release state: {forbidden}")

if 'git merge-base --is-ancestor "${_SOURCE_COMMIT}" origin/yourown-chat' not in config:
    raise SystemExit("Cloud Build source ancestry check has the wrong direction")
if not re.search(
    r'merge-base --is-ancestor\s+\\?\s*"\$\{source_commit\}"\s+origin/yourown-chat',
    source_verifier,
):
    raise SystemExit("toolbox source ancestry check has the wrong direction")

from_lines = [line for line in toolbox.splitlines() if line.startswith("FROM ")]
if len(from_lines) != 4:
    raise SystemExit("release toolbox must have exactly four pinned stages")
for line in from_lines:
    if not re.search(r"@sha256:[0-9a-f]{64}(?: AS [a-z]+)?$", line):
        raise SystemExit(f"toolbox base is not digest pinned: {line}")

match = re.search(r"components=\(([^)]*)\)", images)
if match is None or match.group(1).split() != [
    "controller",
    "ui",
    "golang-adk",
    "codex-harness",
]:
    raise SystemExit("Cloud Build image set drifted")
if "cloudbuild-${build_id}" not in images:
    raise SystemExit("candidate image aliases are not build-unique")
if "imagetools create" not in images:
    raise SystemExit("final aliases are not promoted from verified candidates")
if "verify-candidates" not in config or "--build-id" not in config:
    raise SystemExit("Cloud Build does not verify its exact candidate indexes")

registry = runpy.run_path(str(registry_path))
if registry["repositories"]("images") != [
    ("controller", "pilprod/kagent/controller"),
    ("ui", "pilprod/kagent/ui"),
    ("golang-adk", "pilprod/kagent/golang-adk"),
    ("codex-harness", "pilprod/kagent/codex-harness"),
]:
    raise SystemExit("GHCR image verification set drifted")
if registry["repositories"]("charts") != [
    ("kagent", "pilprod/kagent/helm/kagent"),
    ("kagent-crds", "pilprod/kagent/helm/kagent-crds"),
]:
    raise SystemExit("GHCR chart verification set drifted")

good_index = {
    "mediaType": "application/vnd.oci.image.index.v1+json",
    "manifests": [
        {"platform": {"os": "linux", "architecture": "amd64"}},
        {"platform": {"os": "unknown", "architecture": "unknown"}},
        {"platform": {"os": "linux", "architecture": "arm64"}},
    ],
}
registry["assert_image_platforms"](good_index, "fixture")
bad_index = {
    "mediaType": "application/vnd.oci.image.index.v1+json",
    "manifests": [
        {"platform": {"os": "linux", "architecture": "amd64"}},
    ],
}
try:
    registry["assert_image_platforms"](bad_index, "fixture")
except SystemExit:
    pass
else:
    raise SystemExit("GHCR verifier accepted an incomplete platform set")

test_digest = "sha256:" + "a" * 64
with tempfile.TemporaryDirectory() as directory:
    evidence_directory = pathlib.Path(directory)
    for component, _ in registry["repositories"]("images"):
        (evidence_directory / f"image-{component}.txt").write_text(
            f"{component}={test_digest}\n", encoding="utf-8"
        )

    class IncompleteCandidateRegistry:
        def __init__(self):
            self.references = []

        def manifest(self, repository, reference):
            self.references.append((repository, reference))
            return test_digest, bad_index

    incomplete = IncompleteCandidateRegistry()
    try:
        registry["verify_candidate_images"](
            incomplete,
            "0.0.0-test.kap.1",
            "build-1",
            evidence_directory,
        )
    except SystemExit:
        pass
    else:
        raise SystemExit("candidate verifier accepted an incomplete platform set")
    if incomplete.references[0][1] != test_digest:
        raise SystemExit("candidate verifier did not inspect the recorded digest first")

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
    raise SystemExit("Cloud Build evidence must not activate Claude Harness")
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
python3 - "${tmp_dir}/release/release-evidence.json" "${head}" <<'PY'
import json
import pathlib
import sys

evidence = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
if evidence["schemaVersion"] != 3:
    raise SystemExit("wrong evidence schema")
if evidence["source_commit"] != sys.argv[2]:
    raise SystemExit("wrong source commit")
if sorted(evidence["image_refs"]) != ["controller", "ui"]:
    raise SystemExit("wrong deployment image set")
if sorted(evidence["runtime_images"]) != ["codexHarness", "kagentHarness"]:
    raise SystemExit("wrong separately activated runtime image set")
if sorted(evidence["charts"]) != ["application", "crds"]:
    raise SystemExit("wrong chart set")
PY

python3 - "${tmp_dir}/release/cloud-build-receipt.json" "${head}" <<'PY'
import json
import pathlib
import sys

receipt = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
if receipt["schemaVersion"] != 2:
    raise SystemExit("wrong Cloud Build receipt schema")
if receipt["source_commit"] != sys.argv[2]:
    raise SystemExit("wrong receipt source commit")
if receipt["version"] != "0.0.0-test.kap.1":
    raise SystemExit("wrong receipt artifact version")
if receipt["artifact_tag"] != "v0.0.0-test.kap.1":
    raise SystemExit("wrong receipt artifact tag")
if receipt["source_tag"] != "gcp-v0.0.0-test.kap.1":
    raise SystemExit("wrong receipt source tag")
PY

python3 "${assembler}" \
  0.0.0-test.kap.1 \
  "${head}" \
  "${tmp_dir}/digests" \
  "${tmp_dir}/charts" \
  "${tmp_dir}/legacy-release"
python3 "${receipt_finalizer}" \
  test-build-legacy \
  test-project \
  "${head}" \
  0.0.0-test.kap.1 \
  v0.0.0-test.kap.1 \
  "${tmp_dir}/legacy-release"

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

printf 'Docker-less Cloud Build fork preview contracts passed\n'
