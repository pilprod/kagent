#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${root_dir}/.github/workflows/fork-preview-release.yaml"
values="${root_dir}/helm/kagent/values.yaml"
templates="${root_dir}/helm/kagent/templates"
harness_types="${root_dir}/go/api/v1alpha3/harness_types.go"
kagent_compiler="${root_dir}/go/core/v2/translator/kagent/compiler.go"
skills_init_removal="059c01b68584dea113ccdf80f2e356c2d051e02a"

fail() {
  printf 'Fork preview release contract test failed: %s\n' "$1" >&2
  exit 1
}

test -f "${workflow}" || fail "release workflow is missing"
for path in "${values}" "${harness_types}" "${kagent_compiler}"; do
  test -f "${path}" || fail "release consumer source is missing: ${path}"
done

git -C "${root_dir}" merge-base --is-ancestor "${skills_init_removal}" HEAD ||
  fail "current source does not include the upstream skills-init removal"

python3 - "${workflow}" "${values}" "${templates}" "${harness_types}" "${kagent_compiler}" <<'PY'
import pathlib
import re
import sys

workflow_path, values_path, templates_path, harness_types_path, compiler_path = map(
    pathlib.Path, sys.argv[1:]
)
workflow = workflow_path.read_text(encoding="utf-8")
values = values_path.read_text(encoding="utf-8")
harness_types = harness_types_path.read_text(encoding="utf-8")
compiler = compiler_path.read_text(encoding="utf-8")

required_workflow_fragments = (
    "          - golang-adk\n",
    'agent="$(cut -d= -f2 release-inputs/image-golang-adk.txt)"',
    '"golang-adk:${agent}"',
    '--arg kagentHarness "ghcr.io/${owner}/kagent/golang-adk@${agent}"',
    'kagentHarness: $kagentHarness',
    '--arg source_repository "https://github.com/${GITHUB_REPOSITORY}"',
    'chart_source_tree="$(git rev-parse "${GITHUB_SHA}:helm/kagent")"',
    'path: "helm/kagent"',
    'tree: $chart_source_tree',
    '--arg skills_init_removal_commit "059c01b68584dea113ccdf80f2e356c2d051e02a"',
    'skills_init_removal_commit: $skills_init_removal_commit',
    'image_refs: {',
    'runtime_images: {',
    'application: {',
    'crds: {',
    'ref: $application_chart',
    'ref: $crds_chart',
)
for fragment in required_workflow_fragments:
    if fragment not in workflow:
        raise SystemExit(f"missing workflow contract fragment: {fragment}")

chart_sources = [values]
chart_sources.extend(
    path.read_text(encoding="utf-8")
    for path in sorted(templates_path.rglob("*"))
    if path.is_file()
)
chart_text = "\n".join(chart_sources)
if "skillsInitImage" in chart_text:
    raise SystemExit("obsolete controller.skillsInitImage was reintroduced")
if re.search(r"(?m)^\s*-?\s*name:\s*skills-init\s*$", chart_text):
    raise SystemExit("obsolete skills-init container was reintroduced")
for orphaned in ("agentImage:", "IMAGE_REGISTRY:", "IMAGE_REPOSITORY:", "IMAGE_TAG:"):
    if orphaned in chart_text:
        raise SystemExit(f"removed deployment-backed image knob was reintroduced: {orphaned}")

if "Pattern=`^[^[:space:]@]+@sha256:[a-f0-9]{64}$`" not in harness_types:
    raise SystemExit("Harness workload image is not required to be digest-qualified")
if "Image:              harness.Spec.Workload.Image" not in compiler:
    raise SystemExit("kagent compiler no longer propagates the exact Harness image")
PY

printf 'Fork preview image and current chart source contracts passed\n'
