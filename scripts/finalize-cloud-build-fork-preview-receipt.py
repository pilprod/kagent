#!/usr/bin/env python3
"""Bind a Cloud Build identity to an already assembled preview receipt."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re


VERSION_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
BUILD_ID_RE = re.compile(r"^[0-9A-Za-z.-]+$")


def sha256(path: pathlib.Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(block)
    return hasher.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("build_id")
    parser.add_argument("project_id")
    parser.add_argument("source_commit")
    parser.add_argument("version")
    parser.add_argument("release_directory", type=pathlib.Path)
    args = parser.parse_args()

    if not BUILD_ID_RE.fullmatch(args.build_id):
        raise SystemExit("invalid Cloud Build ID")
    if not args.project_id or "/" in args.project_id:
        raise SystemExit("invalid Cloud Build project ID")
    if not COMMIT_RE.fullmatch(args.source_commit):
        raise SystemExit("invalid source commit")
    if not VERSION_RE.fullmatch(args.version):
        raise SystemExit("invalid preview version")

    evidence_path = args.release_directory / "release-evidence.json"
    evidence = json.loads(evidence_path.read_text(encoding="utf-8"))
    if evidence.get("source_commit") != args.source_commit:
        raise SystemExit("release evidence source commit does not match")
    if evidence.get("tag") != f"v{args.version}":
        raise SystemExit("release evidence version does not match")

    receipt = {
        "schemaVersion": 1,
        "builder": "google-cloud-build",
        "build_id": args.build_id,
        "project_id": args.project_id,
        "source_commit": args.source_commit,
        "version": args.version,
    }
    receipt_path = args.release_directory / "cloud-build-receipt.json"
    receipt_path.write_text(
        json.dumps(receipt, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    with (args.release_directory / "SHA256SUMS").open("a", encoding="utf-8") as sums:
        sums.write(f"{sha256(receipt_path)}  {receipt_path.name}\n")

    for line in (args.release_directory / "SHA256SUMS").read_text(
        encoding="utf-8"
    ).splitlines():
        expected, name = line.split("  ", 1)
        if sha256(args.release_directory / name) != expected:
            raise SystemExit(f"checksum verification failed: {name}")
    expected_evidence = (
        args.release_directory / "release-evidence.json.sha256"
    ).read_text(encoding="utf-8").split("  ", 1)[0]
    if sha256(evidence_path) != expected_evidence:
        raise SystemExit("release evidence checksum verification failed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
