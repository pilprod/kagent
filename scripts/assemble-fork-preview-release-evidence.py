#!/usr/bin/env python3
"""Assemble deployment-facing kagent fork preview evidence from verified digests."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import shutil
import subprocess


VERSION_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SKILLS_INIT_REMOVAL = "059c01b68584dea113ccdf80f2e356c2d051e02a"


def fail(message: str) -> "NoReturn":
    raise SystemExit(f"fork preview evidence assembly failed: {message}")


def digest_file(directory: pathlib.Path, kind: str, component: str) -> str:
    path = directory / f"{kind}-{component}.txt"
    try:
        line = path.read_text(encoding="utf-8").strip()
    except OSError as error:
        fail(f"could not read {path}: {error}")
    prefix = f"{component}="
    if not line.startswith(prefix):
        fail(f"wrong component key in {path}")
    digest = line.removeprefix(prefix)
    if not DIGEST_RE.fullmatch(digest):
        fail(f"invalid digest in {path}")
    return digest


def sha256(path: pathlib.Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(block)
    return hasher.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("version")
    parser.add_argument("source_commit")
    parser.add_argument("evidence_directory", type=pathlib.Path)
    parser.add_argument("chart_directory", type=pathlib.Path)
    parser.add_argument("output_directory", type=pathlib.Path)
    args = parser.parse_args()

    if not VERSION_RE.fullmatch(args.version):
        fail(f"invalid fork preview version: {args.version}")
    if not COMMIT_RE.fullmatch(args.source_commit):
        fail(f"invalid source commit: {args.source_commit}")

    repository_root = pathlib.Path(__file__).resolve().parent.parent
    actual_head = subprocess.check_output(
        ("git", "-C", str(repository_root), "rev-parse", "HEAD"), text=True
    ).strip()
    if actual_head != args.source_commit:
        fail(f"source checkout is {actual_head}, expected {args.source_commit}")
    chart_tree = subprocess.check_output(
        (
            "git",
            "-C",
            str(repository_root),
            "rev-parse",
            f"{args.source_commit}:helm/kagent",
        ),
        text=True,
    ).strip()
    if not COMMIT_RE.fullmatch(chart_tree):
        fail(f"invalid helm/kagent tree identity: {chart_tree}")

    controller = digest_file(args.evidence_directory, "image", "controller")
    ui = digest_file(args.evidence_directory, "image", "ui")
    agent = digest_file(args.evidence_directory, "image", "golang-adk")
    codex = digest_file(args.evidence_directory, "image", "codex-harness")
    application = digest_file(args.evidence_directory, "chart", "kagent")
    crds = digest_file(args.evidence_directory, "chart", "kagent-crds")

    evidence = {
        "schemaVersion": 2,
        "channel": "preview",
        "tag": f"v{args.version}",
        "source_repository": "https://github.com/pilprod/kagent",
        "source_commit": args.source_commit,
        "chart_source": {
            "path": "helm/kagent",
            "tree": chart_tree,
            "skills_init_removal_commit": SKILLS_INIT_REMOVAL,
        },
        "image_refs": {
            "controller": f"ghcr.io/pilprod/kagent/controller@{controller}",
            "ui": f"ghcr.io/pilprod/kagent/ui@{ui}",
        },
        "runtime_images": {
            "kagentHarness": f"ghcr.io/pilprod/kagent/golang-adk@{agent}",
            "codexHarness": f"ghcr.io/pilprod/kagent/codex-harness@{codex}"
        },
        "charts": {
            "application": {
                "ref": f"oci://ghcr.io/pilprod/kagent/helm/kagent@{application}",
                "version": args.version,
            },
            "crds": {
                "ref": f"oci://ghcr.io/pilprod/kagent/helm/kagent-crds@{crds}",
                "version": args.version,
            },
        },
    }

    args.output_directory.mkdir(parents=True, exist_ok=False)
    evidence_path = args.output_directory / "release-evidence.json"
    evidence_path.write_text(
        json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    evidence_digest = sha256(evidence_path)
    (args.output_directory / "release-evidence.json.sha256").write_text(
        f"{evidence_digest}  release-evidence.json\n", encoding="utf-8"
    )

    checksums: list[str] = []
    for chart in ("kagent", "kagent-crds"):
        name = f"{chart}-{args.version}.tgz"
        source = args.chart_directory / name
        if not source.is_file():
            fail(f"missing chart archive: {source}")
        destination = args.output_directory / name
        shutil.copyfile(source, destination)
        checksums.append(f"{sha256(destination)}  {name}")
    checksums.append(f"{evidence_digest}  release-evidence.json")
    (args.output_directory / "SHA256SUMS").write_text(
        "\n".join(checksums) + "\n", encoding="utf-8"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
