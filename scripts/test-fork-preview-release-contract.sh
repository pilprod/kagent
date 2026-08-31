#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${root_dir}/.github/workflows/fork-preview-release.yaml"
generic_workflow="${root_dir}/.github/workflows/tag.yaml"
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
test -f "${generic_workflow}" || fail "generic upstream release workflow is missing"
for path in "${values}" "${harness_types}" "${kagent_compiler}"; do
  test -f "${path}" || fail "release consumer source is missing: ${path}"
done

git -C "${root_dir}" merge-base --is-ancestor "${skills_init_removal}" HEAD ||
  fail "current source does not include the upstream skills-init removal"

python3 - \
  "${workflow}" "${generic_workflow}" "${values}" "${templates}" \
  "${harness_types}" "${kagent_compiler}" <<'PY'
import pathlib
import re
import sys

(
    workflow_path,
    generic_workflow_path,
    values_path,
    templates_path,
    harness_types_path,
    compiler_path,
) = map(
    pathlib.Path, sys.argv[1:]
)
workflow = workflow_path.read_text(encoding="utf-8")
generic_workflow = generic_workflow_path.read_text(encoding="utf-8")
values = values_path.read_text(encoding="utf-8")
harness_types = harness_types_path.read_text(encoding="utf-8")
compiler = compiler_path.read_text(encoding="utf-8")

required_guard_fragments = (
    "name: Retired public fork preview release\n",
    '      - "v*.kap.*"\n',
    "permissions: {}\n",
    "  reject-public-preview:\n",
    "      - name: Require the private app-gcp release rail\n",
    "Public GHCR fork previews are disabled.",
    "private app-gcp Pub/Sub and GAR release rail",
    "          exit 1\n",
)
for fragment in required_guard_fragments:
    if fragment not in workflow:
        raise SystemExit(f"missing retired workflow guard fragment: {fragment}")

for forbidden in (
    "continue-on-error",
    "packages: write",
    "attestations: write",
    "id-token: write",
    "ghcr.io",
    "docker/login-action",
    "docker/setup-buildx-action",
    "gh release",
    "helm push",
):
    if forbidden in workflow:
        raise SystemExit(f"retired public workflow still contains publication capability: {forbidden}")

guard = workflow.index("      - name: Require the private app-gcp release rail")
failure = workflow.index("          exit 1", guard)
if failure <= guard:
    raise SystemExit("retired public workflow does not fail closed")

generic_guard = '[[ "${GITHUB_REPOSITORY}" == "kagent-dev/kagent" ]] || {'
if generic_guard not in generic_workflow:
    raise SystemExit("fork can still invoke the generic upstream public release rail")
if "public upstream releases are disabled in the pilprod/kagent fork" not in generic_workflow:
    raise SystemExit("generic upstream release guard has no explicit fork failure")
if generic_workflow.index(generic_guard) > generic_workflow.index('[[ "${GITHUB_REF_TYPE}"'):
    raise SystemExit("generic upstream release guard does not run first")

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

printf 'Retired public workflow and current chart source contracts passed\n'
