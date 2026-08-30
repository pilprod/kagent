#!/usr/bin/env python3
"""Fail-closed GHCR checks for the manual Cloud Build preview rail."""

from __future__ import annotations

import argparse
import base64
import json
import os
import pathlib
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


OWNER = "pilprod"
IMAGE_COMPONENTS = ("controller", "ui", "golang-adk", "codex-harness")
CHART_COMPONENTS = ("kagent", "kagent-crds")
ACCEPT = ", ".join(
    (
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    )
)
INDEX_MEDIA_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}
EXPECTED_RUNTIME_PLATFORMS = ["linux/amd64", "linux/arm64"]
VERSION_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$")
BUILD_ID_RE = re.compile(r"^[0-9A-Za-z.-]+$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


def fail(message: str) -> "NoReturn":
    raise SystemExit(f"GHCR fork preview verification failed: {message}")


def repositories(category: str) -> list[tuple[str, str]]:
    selected: list[tuple[str, str]] = []
    if category in ("all", "images"):
        selected.extend(
            (component, f"{OWNER}/kagent/{component}")
            for component in IMAGE_COMPONENTS
        )
    if category in ("all", "charts"):
        selected.extend(
            (component, f"{OWNER}/kagent/helm/{component}")
            for component in CHART_COMPONENTS
        )
    return selected


class Registry:
    def __init__(self, username: str, password: str) -> None:
        if not username or not password:
            fail("GHCR_USERNAME and GHCR_TOKEN must both be set")
        self._basic = base64.b64encode(
            f"{username}:{password}".encode("utf-8")
        ).decode("ascii")

    def _pull_token(self, repository: str) -> str:
        query = urllib.parse.urlencode(
            {
                "service": "ghcr.io",
                "scope": f"repository:{repository}:pull",
            }
        )
        request = urllib.request.Request(
            f"https://ghcr.io/token?{query}",
            headers={"Authorization": f"Basic {self._basic}"},
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.load(response)
        except (OSError, ValueError, urllib.error.HTTPError) as error:
            fail(f"could not acquire a read token for {repository}: {error}")
        token = payload.get("token")
        if not isinstance(token, str) or not token:
            fail(f"GHCR returned no read token for {repository}")
        return token

    def manifest(
        self, repository: str, reference: str
    ) -> tuple[str, dict[str, object]] | None:
        token = self._pull_token(repository)
        encoded_reference = urllib.parse.quote(reference, safe=":@")
        request = urllib.request.Request(
            f"https://ghcr.io/v2/{repository}/manifests/{encoded_reference}",
            headers={"Authorization": f"Bearer {token}", "Accept": ACCEPT},
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                body = response.read()
                digest = response.headers.get("Docker-Content-Digest", "")
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            fail(
                f"could not inspect ghcr.io/{repository}:{reference} "
                f"(registry status {error.code})"
            )
        except OSError as error:
            fail(f"could not inspect ghcr.io/{repository}:{reference}: {error}")
        if not DIGEST_RE.fullmatch(digest):
            fail(
                f"registry returned an invalid digest for "
                f"ghcr.io/{repository}:{reference}: {digest!r}"
            )
        try:
            payload = json.loads(body)
        except (UnicodeDecodeError, ValueError) as error:
            fail(
                f"registry returned an invalid manifest for "
                f"ghcr.io/{repository}:{reference}: {error}"
            )
        if not isinstance(payload, dict):
            fail(
                f"registry returned a non-object manifest for "
                f"ghcr.io/{repository}:{reference}"
            )
        return digest, payload


def assert_image_platforms(manifest: dict[str, object], reference: str) -> None:
    if manifest.get("mediaType") not in INDEX_MEDIA_TYPES:
        fail(f"{reference} is not a multi-platform image index")
    descriptors = manifest.get("manifests")
    if not isinstance(descriptors, list):
        fail(f"{reference} image index has no manifest descriptors")

    runtime_platforms: list[str] = []
    unexpected_platforms: list[str] = []
    for descriptor in descriptors:
        if not isinstance(descriptor, dict):
            fail(f"{reference} image index has a malformed descriptor")
        platform = descriptor.get("platform")
        if not isinstance(platform, dict):
            fail(f"{reference} image index descriptor has no platform")
        operating_system = platform.get("os")
        architecture = platform.get("architecture")
        if not isinstance(operating_system, str) or not isinstance(
            architecture, str
        ):
            fail(f"{reference} image index descriptor has an invalid platform")
        value = f"{operating_system}/{architecture}"
        if value in EXPECTED_RUNTIME_PLATFORMS:
            runtime_platforms.append(value)
        elif value != "unknown/unknown":
            unexpected_platforms.append(value)

    if sorted(runtime_platforms) != EXPECTED_RUNTIME_PLATFORMS:
        fail(
            f"{reference} runtime platforms are {sorted(runtime_platforms)!r}, "
            f"expected {EXPECTED_RUNTIME_PLATFORMS!r}"
        )
    if unexpected_platforms:
        fail(
            f"{reference} has unexpected runtime platforms: "
            f"{sorted(unexpected_platforms)!r}"
        )


def read_expected(path: pathlib.Path, component: str) -> str:
    try:
        line = path.read_text(encoding="utf-8").strip()
    except OSError as error:
        fail(f"could not read digest evidence {path}: {error}")
    prefix = f"{component}="
    if not line.startswith(prefix):
        fail(f"digest evidence has the wrong component key: {path}")
    digest = line.removeprefix(prefix)
    if not DIGEST_RE.fullmatch(digest):
        fail(f"digest evidence is malformed: {path}")
    return digest


def verify_candidate_images(
    registry: Registry,
    version: str,
    build_id: str,
    evidence_directory: pathlib.Path,
) -> None:
    """Verify immutable candidate indexes before any final tag can be written."""
    if not BUILD_ID_RE.fullmatch(build_id):
        fail(f"invalid Cloud Build ID: {build_id}")
    candidate_tag = f"{version}-cloudbuild-{build_id}"
    for component, repository in repositories("images"):
        expected = read_expected(
            evidence_directory / f"image-{component}.txt", component
        )

        # Inspect the content-addressed index first. The candidate tag is mutable,
        # but the recorded BuildKit digest is the exact object later promoted.
        by_digest = registry.manifest(repository, expected)
        digest_ref = f"ghcr.io/{repository}@{expected}"
        if by_digest is None:
            fail(f"recorded candidate digest is absent: {digest_ref}")
        digest, manifest = by_digest
        if digest != expected:
            fail(f"{digest_ref} resolves to {digest!r}, expected {expected}")
        assert_image_platforms(manifest, digest_ref)

        by_tag = registry.manifest(repository, candidate_tag)
        tag_ref = f"ghcr.io/{repository}:{candidate_tag}"
        if by_tag is None:
            fail(f"candidate alias is absent: {tag_ref}")
        if by_tag[0] != expected:
            fail(f"{tag_ref} resolves to {by_tag[0]!r}, expected {expected}")
        print(f"verified candidate: {digest_ref}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("absent", "verify-candidates", "verify"))
    parser.add_argument("version")
    parser.add_argument("--category", choices=("all", "images", "charts"), default="all")
    parser.add_argument("--evidence-directory", type=pathlib.Path)
    parser.add_argument("--build-id")
    args = parser.parse_args()

    if not VERSION_RE.fullmatch(args.version):
        fail(f"invalid fork preview version: {args.version}")
    if args.mode in ("verify-candidates", "verify") and args.evidence_directory is None:
        fail(f"--evidence-directory is required in {args.mode} mode")
    if args.mode == "verify-candidates" and args.build_id is None:
        fail("--build-id is required in verify-candidates mode")

    registry = Registry(
        os.environ.get("GHCR_USERNAME", ""), os.environ.get("GHCR_TOKEN", "")
    )
    if args.mode == "verify-candidates":
        verify_candidate_images(
            registry, args.version, args.build_id, args.evidence_directory
        )
        return 0

    for component, repository in repositories(args.category):
        resolved = registry.manifest(repository, args.version)
        ref = f"ghcr.io/{repository}:{args.version}"
        if args.mode == "absent":
            if resolved is not None:
                fail(f"refusing to overwrite existing ref {ref}@{resolved[0]}")
            print(f"absent: {ref}")
            continue

        if resolved is None:
            fail(f"expected published ref is absent: {ref}")
        digest, manifest = resolved

        evidence_name = (
            f"image-{component}.txt"
            if component in IMAGE_COMPONENTS
            else f"chart-{component}.txt"
        )
        expected = read_expected(args.evidence_directory / evidence_name, component)
        if digest != expected:
            fail(f"{ref} resolves to {digest!r}, expected {expected}")
        if component in IMAGE_COMPONENTS:
            assert_image_platforms(manifest, ref)
        print(f"verified: {ref}@{digest}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
