#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${root_dir}/.github/workflows/fork-preview-release.yaml"
values="${root_dir}/helm/kagent/values.yaml"
templates="${root_dir}/helm/kagent/templates"
skills_init_removal="059c01b68584dea113ccdf80f2e356c2d051e02a"

fail() {
  printf 'Fork preview release contract test failed: %s\n' "$1" >&2
  exit 1
}

test -f "${workflow}" || fail "release workflow is missing"
test -f "${values}" || fail "kagent values are missing"

git -C "${root_dir}" merge-base --is-ancestor "${skills_init_removal}" HEAD ||
  fail "current source does not include the upstream skills-init removal"

python3 - "${workflow}" "${values}" "${templates}" <<'PY'
import pathlib
import re
import sys

workflow_path = pathlib.Path(sys.argv[1])
values_path = pathlib.Path(sys.argv[2])
templates_path = pathlib.Path(sys.argv[3])
workflow = workflow_path.read_text(encoding="utf-8")
values = values_path.read_text(encoding="utf-8")

required_workflow_fragments = (
    "          - golang-adk\n",
    'agent="$(cut -d= -f2 release-inputs/image-golang-adk.txt)"',
    '"golang-adk:${agent}"',
    '--arg agent "ghcr.io/${owner}/kagent/golang-adk@${agent}"',
    'agent: $agent',
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

controller = re.search(r"(?ms)^controller:\n(?P<body>.*?)(?=^[^ \n])", values)
if controller is None:
    raise SystemExit("controller values block is missing")
agent_image = re.search(
    r"(?ms)^  agentImage:\n(?P<body>(?:^    .*\n?)*)", controller.group("body")
)
if agent_image is None:
    raise SystemExit("controller.agentImage values block is missing")
if not re.search(
    r"(?m)^    repository:\s*kagent-dev/kagent/golang-adk\s*$",
    agent_image.group("body"),
):
    raise SystemExit("controller.agentImage no longer points at golang-adk")

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
PY

printf 'Fork preview image and current chart source contracts passed\n'
