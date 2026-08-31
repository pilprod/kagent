#!/usr/bin/env python3
"""Verify the live private GAR image indexes bound by release evidence."""

from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import json
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable


EXPECTED_REGISTRY_HOST = "europe-west3-docker.pkg.dev"
EXPECTED_RELEASE_PREFIX = (
    f"{EXPECTED_REGISTRY_HOST}/yourown-chat/kagent-preview/substrate"
)
EXPECTED_COMPONENTS = ("agentgateway", "ateapi", "atecontroller", "atenet")
EXPECTED_PLATFORMS = {
    "linux_amd64": ("linux", "amd64"),
    "linux_arm64": ("linux", "arm64"),
}

OCI_INDEX = "application/vnd.oci.image.index.v1+json"
DOCKER_INDEX = "application/vnd.docker.distribution.manifest.list.v2+json"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
DOCKER_MANIFEST = "application/vnd.docker.distribution.manifest.v2+json"
OCI_IMAGE_CONFIG = "application/vnd.oci.image.config.v1+json"
ATTESTATION_ARTIFACT = "application/vnd.docker.attestation.manifest.v1+json"
ATTESTATION_REFERENCE_TYPE = "attestation-manifest"
REFERENCE_DIGEST_ANNOTATION = "vnd.docker.reference.digest"
REFERENCE_TYPE_ANNOTATION = "vnd.docker.reference.type"
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
MAX_MANIFEST_BYTES = 16 * 1024 * 1024


class VerificationError(RuntimeError):
    """A fail-closed release verification failure."""


class RejectRegistryRedirects(urllib.request.HTTPRedirectHandler):
    """Keep the short-lived GAR Authorization header on its exact origin."""

    def redirect_request(
        self,
        request: urllib.request.Request,
        file_pointer: Any,
        code: int,
        message: str,
        headers: Any,
        new_url: str,
    ) -> None:
        raise VerificationError(
            f"registry redirects are forbidden: HTTP {code} to {new_url}"
        )


_NO_REDIRECT_OPENER = urllib.request.build_opener(RejectRegistryRedirects()).open


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise VerificationError(message)


def _load_json(path: Path, label: str) -> dict[str, Any]:
    try:
        document = json.loads(path.read_bytes())
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise VerificationError(f"cannot read {label}: {error}") from error
    _require(isinstance(document, dict), f"{label} must be a JSON object")
    return document


def _decode_registry_auth(config: dict[str, Any]) -> str:
    auths = config.get("auths")
    _require(isinstance(auths, dict), "registry config auths must be an object")
    host_entry = auths.get(EXPECTED_REGISTRY_HOST)
    _require(
        isinstance(host_entry, dict),
        f"registry config must contain auths.{EXPECTED_REGISTRY_HOST}",
    )
    encoded = host_entry.get("auth")
    _require(isinstance(encoded, str) and encoded, "registry auth must be non-empty")
    try:
        decoded_bytes = base64.b64decode(encoded, validate=True)
        decoded = decoded_bytes.decode("utf-8")
    except (binascii.Error, UnicodeDecodeError) as error:
        raise VerificationError("registry auth is not valid base64 UTF-8") from error
    username, separator, password = decoded.partition(":")
    _require(separator == ":", "registry auth must encode Basic credentials")
    _require(
        username == "oauth2accesstoken" and bool(password),
        "registry auth must contain a short-lived oauth2accesstoken credential",
    )
    return encoded


def _sha256_digest(content: bytes) -> str:
    return f"sha256:{hashlib.sha256(content).hexdigest()}"


def _json_object(content: bytes, label: str) -> dict[str, Any]:
    try:
        document = json.loads(content)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise VerificationError(f"{label} is not valid JSON") from error
    _require(isinstance(document, dict), f"{label} must be a JSON object")
    return document


def _valid_digest(value: Any) -> bool:
    return isinstance(value, str) and DIGEST_RE.fullmatch(value) is not None


def _valid_size(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


class RegistryClient:
    """Small Registry HTTP API client with injected transport for unit tests."""

    def __init__(
        self,
        encoded_basic_auth: str,
        opener: Callable[..., Any] = _NO_REDIRECT_OPENER,
    ) -> None:
        self._authorization = f"Basic {encoded_basic_auth}"
        self._opener = opener

    def manifest(
        self,
        repository: str,
        digest: str,
        accepted_media_types: tuple[str, ...],
    ) -> tuple[dict[str, Any], bytes, str]:
        _require(_valid_digest(digest), f"invalid requested manifest digest: {digest}")
        encoded_repository = urllib.parse.quote(repository, safe="/")
        encoded_digest = urllib.parse.quote(digest, safe=":")
        url = (
            f"https://{EXPECTED_REGISTRY_HOST}/v2/{encoded_repository}/manifests/"
            f"{encoded_digest}"
        )
        request = urllib.request.Request(
            url,
            headers={
                "Accept": ", ".join(accepted_media_types),
                "Accept-Encoding": "identity",
                "Authorization": self._authorization,
                "User-Agent": "kagent-private-substrate-verifier/1",
            },
            method="GET",
        )
        try:
            with self._opener(request, timeout=30) as response:
                status = getattr(response, "status", None)
                if status is None:
                    status = response.getcode()
                _require(status == 200, f"registry returned HTTP {status} for {repository}")
                content = response.read(MAX_MANIFEST_BYTES + 1)
                _require(
                    len(content) <= MAX_MANIFEST_BYTES,
                    f"registry manifest exceeds size limit for {repository}",
                )
                response_digest = response.headers.get("Docker-Content-Digest")
                content_type = (response.headers.get("Content-Type") or "").split(";", 1)[0]
        except urllib.error.HTTPError as error:
            raise VerificationError(
                f"registry returned HTTP {error.code} for {repository}"
            ) from error
        except urllib.error.URLError as error:
            raise VerificationError(f"registry request failed for {repository}: {error.reason}") from error

        _require(
            response_digest == digest,
            f"registry digest header mismatch for {repository}@{digest}",
        )
        _require(
            _sha256_digest(content) == digest,
            f"registry manifest body digest mismatch for {repository}@{digest}",
        )
        document = _json_object(content, f"manifest {repository}@{digest}")
        media_type = document.get("mediaType")
        _require(
            media_type in accepted_media_types,
            f"unexpected manifest media type for {repository}@{digest}",
        )
        _require(
            content_type == media_type,
            f"registry Content-Type mismatch for {repository}@{digest}",
        )
        return document, content, media_type


def _validate_blob_descriptor(descriptor: Any, label: str) -> None:
    _require(isinstance(descriptor, dict), f"{label} must be an object")
    _require(isinstance(descriptor.get("mediaType"), str), f"{label} mediaType is missing")
    _require(_valid_digest(descriptor.get("digest")), f"{label} digest is invalid")
    _require(_valid_size(descriptor.get("size")), f"{label} size is invalid")


def _validate_image_manifest(document: dict[str, Any], label: str) -> None:
    _require(document.get("schemaVersion") == 2, f"{label} schemaVersion is invalid")
    _validate_blob_descriptor(document.get("config"), f"{label} config")
    layers = document.get("layers")
    _require(isinstance(layers, list), f"{label} layers must be an array")
    for index, layer in enumerate(layers):
        _validate_blob_descriptor(layer, f"{label} layer {index}")


def _validate_annotations(value: Any, label: str) -> None:
    _require(isinstance(value, dict), f"{label} must be an object")
    for key, annotation in value.items():
        _require(
            isinstance(key, str)
            and bool(key)
            and isinstance(annotation, str)
            and bool(annotation),
            f"{label} must contain non-empty string keys and values",
        )


def _validate_attestation_manifest(
    document: dict[str, Any],
    subject_digest: str,
    subject_descriptor: dict[str, Any],
    label: str,
) -> None:
    _require(
        set(document) == {
            "schemaVersion",
            "mediaType",
            "artifactType",
            "config",
            "layers",
            "subject",
        },
        f"{label} has unexpected fields",
    )
    _require(document.get("schemaVersion") == 2, f"{label} schemaVersion is invalid")
    _require(document.get("mediaType") == OCI_MANIFEST, f"{label} mediaType is invalid")
    _require(document.get("artifactType") == ATTESTATION_ARTIFACT, f"{label} artifactType is invalid")
    config = document.get("config")
    _require(isinstance(config, dict), f"{label} config must be an object")
    _require(
        set(config) == {"mediaType", "digest", "size", "data"}
        and config.get("mediaType") == "application/vnd.oci.empty.v1+json"
        and config.get("digest")
        == "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
        and config.get("size") == 2
        and config.get("data") == "e30=",
        f"{label} config is not the canonical empty OCI config",
    )
    layers = document.get("layers")
    _require(isinstance(layers, list) and bool(layers), f"{label} layers are invalid")
    for index, layer in enumerate(layers):
        _validate_blob_descriptor(layer, f"{label} layer {index}")
    subject = document.get("subject")
    _require(isinstance(subject, dict), f"{label} subject must be an object")
    _require(
        set(subject) == {"mediaType", "digest", "size"}
        and subject.get("mediaType") == subject_descriptor.get("mediaType")
        and subject.get("digest") == subject_digest
        and subject.get("size") == subject_descriptor.get("size"),
        f"{label} subject does not bind the referenced platform manifest",
    )


def _platform_key(platform: Any) -> str | None:
    if not isinstance(platform, dict):
        return None
    if platform.get("os") == "linux" and platform.get("architecture") == "amd64":
        _require(
            set(platform) == {"os", "architecture"},
            "linux/amd64 descriptor contains unexpected platform fields",
        )
        return "linux_amd64"
    if platform.get("os") == "linux" and platform.get("architecture") == "arm64":
        allowed = {"os", "architecture"}
        if "variant" in platform:
            _require(platform.get("variant") == "v8", "linux/arm64 variant must be v8")
            allowed.add("variant")
        _require(
            set(platform) == allowed,
            "linux/arm64 descriptor contains unexpected platform fields",
        )
        return "linux_arm64"
    return None


def _verify_component(
    client: RegistryClient,
    component: str,
    image: Any,
    expected_platform_digests: Any,
) -> None:
    _require(isinstance(image, dict), f"evidence image {component} must be an object")
    _require(set(image) == {"ref", "digest"}, f"evidence image {component} has unexpected fields")
    index_digest = image.get("digest")
    _require(_valid_digest(index_digest), f"evidence image digest is invalid for {component}")
    expected_ref = f"{EXPECTED_RELEASE_PREFIX}/{component}@{index_digest}"
    _require(image.get("ref") == expected_ref, f"evidence image ref is invalid for {component}")
    _require(
        isinstance(expected_platform_digests, dict)
        and set(expected_platform_digests) == set(EXPECTED_PLATFORMS),
        f"evidence platform digest set is invalid for {component}",
    )
    for platform_key, digest in expected_platform_digests.items():
        _require(_valid_digest(digest), f"evidence {platform_key} digest is invalid for {component}")
    _require(
        len(set(expected_platform_digests.values())) == len(EXPECTED_PLATFORMS),
        f"evidence platform digests must be distinct for {component}",
    )

    repository = f"yourown-chat/kagent-preview/substrate/{component}"
    index, _, _ = client.manifest(repository, index_digest, (OCI_INDEX, DOCKER_INDEX))
    required_index_fields = {"schemaVersion", "mediaType", "manifests"}
    _require(
        required_index_fields <= set(index)
        and set(index) <= required_index_fields | {"annotations"},
        f"live index has unexpected fields for {component}",
    )
    if "annotations" in index:
        _validate_annotations(index["annotations"], f"live index annotations for {component}")
    _require(index.get("schemaVersion") == 2, f"live index schemaVersion is invalid for {component}")
    descriptors = index.get("manifests")
    _require(isinstance(descriptors, list), f"live index manifests must be an array for {component}")

    platform_descriptors: dict[str, dict[str, Any]] = {}
    attestation_descriptors: list[tuple[dict[str, Any], str]] = []
    descriptor_digests: set[str] = set()
    for descriptor_index, descriptor in enumerate(descriptors):
        label = f"{component} descriptor {descriptor_index}"
        _require(isinstance(descriptor, dict), f"{label} must be an object")
        _require(_valid_digest(descriptor.get("digest")), f"{label} digest is invalid")
        _require(_valid_size(descriptor.get("size")), f"{label} size is invalid")
        digest = descriptor["digest"]
        _require(digest not in descriptor_digests, f"{component} contains duplicate child digests")
        descriptor_digests.add(digest)

        platform = descriptor.get("platform")
        platform_key = _platform_key(platform)
        if platform_key is not None:
            allowed_descriptor_fields = {"mediaType", "digest", "size", "platform"}
            if "artifactType" in descriptor:
                _require(
                    descriptor.get("mediaType") == OCI_MANIFEST
                    and descriptor.get("artifactType") == OCI_IMAGE_CONFIG,
                    f"{label} artifactType is invalid",
                )
                allowed_descriptor_fields.add("artifactType")
            _require(
                set(descriptor) == allowed_descriptor_fields,
                f"{label} has unexpected fields",
            )
            _require(
                descriptor.get("mediaType") in (OCI_MANIFEST, DOCKER_MANIFEST),
                f"{label} mediaType is invalid",
            )
            _require(platform_key not in platform_descriptors, f"duplicate {platform_key} child for {component}")
            _require(
                digest == expected_platform_digests[platform_key],
                f"live {platform_key} digest does not match evidence for {component}",
            )
            platform_descriptors[platform_key] = descriptor
            continue

        _require(
            platform == {"architecture": "unknown", "os": "unknown"},
            f"unsupported child platform in live index for {component}",
        )
        _require(
            set(descriptor) == {"mediaType", "digest", "size", "annotations", "platform"},
            f"{label} attestation descriptor has unexpected fields",
        )
        annotations = descriptor.get("annotations")
        _require(
            descriptor.get("mediaType") == OCI_MANIFEST
            and isinstance(annotations, dict)
            and set(annotations)
            == {REFERENCE_DIGEST_ANNOTATION, REFERENCE_TYPE_ANNOTATION}
            and annotations.get(REFERENCE_TYPE_ANNOTATION) == ATTESTATION_REFERENCE_TYPE
            and annotations.get(REFERENCE_DIGEST_ANNOTATION)
            in set(expected_platform_digests.values()),
            f"{label} is not a valid unknown/unknown attestation descriptor",
        )
        attestation_descriptors.append(
            (descriptor, annotations[REFERENCE_DIGEST_ANNOTATION])
        )

    _require(
        set(platform_descriptors) == set(EXPECTED_PLATFORMS),
        f"live index must contain exactly linux/amd64 and linux/arm64 for {component}",
    )

    for platform_key, descriptor in platform_descriptors.items():
        child_digest = descriptor["digest"]
        child, child_content, _ = client.manifest(
            repository,
            child_digest,
            (descriptor["mediaType"],),
        )
        _require(
            len(child_content) == descriptor["size"],
            f"live {platform_key} child size mismatch for {component}",
        )
        _validate_image_manifest(child, f"live {platform_key} child for {component}")

    by_digest = {descriptor["digest"]: descriptor for descriptor in platform_descriptors.values()}
    for descriptor, subject_digest in attestation_descriptors:
        child, child_content, _ = client.manifest(
            repository,
            descriptor["digest"],
            (OCI_MANIFEST,),
        )
        _require(
            len(child_content) == descriptor["size"],
            f"live attestation child size mismatch for {component}",
        )
        _validate_attestation_manifest(
            child,
            subject_digest,
            by_digest[subject_digest],
            f"live attestation child for {component}",
        )


def verify(
    evidence: dict[str, Any],
    registry_config: dict[str, Any],
    opener: Callable[..., Any] = _NO_REDIRECT_OPENER,
) -> None:
    publication = evidence.get("publication")
    _require(isinstance(publication, dict), "evidence publication must be an object")
    _require(
        publication.get("release_prefix") == EXPECTED_RELEASE_PREFIX,
        "evidence release prefix is not the private Substrate GAR repository",
    )
    images = evidence.get("images")
    platform_digests = evidence.get("platform_image_digests")
    expected_keys = set(EXPECTED_COMPONENTS)
    _require(isinstance(images, dict) and set(images) == expected_keys, "evidence image set is not exact")
    _require(
        isinstance(platform_digests, dict) and set(platform_digests) == expected_keys,
        "evidence platform image digest set is not exact",
    )
    encoded_auth = _decode_registry_auth(registry_config)
    client = RegistryClient(encoded_auth, opener=opener)
    for component in EXPECTED_COMPONENTS:
        _verify_component(client, component, images[component], platform_digests[component])


def _parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Verify private Substrate GAR indexes and child manifests"
    )
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--registry-config", required=True, type=Path)
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(sys.argv[1:] if argv is None else argv)
    try:
        evidence = _load_json(args.evidence, "release evidence")
        registry_config = _load_json(args.registry_config, "registry config")
        verify(evidence, registry_config)
    except VerificationError as error:
        print(f"private Substrate image verification failed: {error}", file=sys.stderr)
        return 1
    print("private Substrate GAR image indexes and child manifests verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
