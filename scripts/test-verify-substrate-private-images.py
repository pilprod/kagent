#!/usr/bin/env python3
"""Offline tests for the private Substrate Registry verifier."""

from __future__ import annotations

import base64
import copy
import hashlib
import importlib.util
import json
import sys
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


SCRIPT_PATH = Path(__file__).with_name("verify-substrate-private-images.py")
SPEC = importlib.util.spec_from_file_location("verify_substrate_private_images", SCRIPT_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot import {SCRIPT_PATH}")
VERIFIER = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = VERIFIER
SPEC.loader.exec_module(VERIFIER)


def encoded(document: dict[str, Any]) -> bytes:
    return json.dumps(document, sort_keys=True, separators=(",", ":")).encode()


def digest(content: bytes) -> str:
    return f"sha256:{hashlib.sha256(content).hexdigest()}"


def manifest_url(repository: str, manifest_digest: str) -> str:
    return (
        f"https://{VERIFIER.EXPECTED_REGISTRY_HOST}/v2/{repository}/manifests/"
        f"{manifest_digest}"
    )


class FakeResponse:
    def __init__(self, content: bytes, headers: dict[str, str], status: int = 200) -> None:
        self.content = content
        self.headers = headers
        self.status = status

    def read(self, size: int = -1) -> bytes:
        return self.content if size < 0 else self.content[:size]

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *_: Any) -> None:
        return None


class FakeRegistry:
    def __init__(self) -> None:
        self.responses: dict[str, FakeResponse] = {}
        self.requests: list[Any] = []

    def add(
        self,
        repository: str,
        document: dict[str, Any],
        *,
        response_digest: str | None = None,
    ) -> tuple[str, int, str]:
        content = encoded(document)
        manifest_digest = digest(content)
        self.responses[manifest_url(repository, manifest_digest)] = FakeResponse(
            content,
            {
                "Content-Type": document["mediaType"],
                "Docker-Content-Digest": response_digest or manifest_digest,
            },
        )
        return manifest_digest, len(content), manifest_url(repository, manifest_digest)

    def __call__(self, request: Any, timeout: int) -> FakeResponse:
        self.requests.append(request)
        if request.full_url not in self.responses:
            raise urllib.error.URLError("fake registry manifest not found")
        if timeout != 30:
            raise AssertionError(f"unexpected timeout: {timeout}")
        return self.responses[request.full_url]


class ReleaseFixture:
    def __init__(self) -> None:
        self.registry = FakeRegistry()
        self.index_documents: dict[str, dict[str, Any]] = {}
        self.repositories: dict[str, str] = {}
        self.evidence: dict[str, Any] = {
            "publication": {"release_prefix": VERIFIER.EXPECTED_RELEASE_PREFIX},
            "images": {},
            "platform_image_digests": {},
        }
        self.registry_config = {
            "auths": {
                VERIFIER.EXPECTED_REGISTRY_HOST: {
                    "auth": base64.b64encode(
                        b"oauth2accesstoken:short-lived-test-token"
                    ).decode()
                }
            }
        }
        for component in VERIFIER.EXPECTED_COMPONENTS:
            self._add_component(component, with_attestations=component == "agentgateway")

    def _image_manifest(self, component: str, architecture: str) -> dict[str, Any]:
        config_content = encoded({"component": component, "architecture": architecture})
        layer_content = f"{component}-{architecture}".encode()
        return {
            "schemaVersion": 2,
            "mediaType": VERIFIER.OCI_MANIFEST,
            "config": {
                "mediaType": "application/vnd.oci.image.config.v1+json",
                "digest": digest(config_content),
                "size": len(config_content),
            },
            "layers": [
                {
                    "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
                    "digest": digest(layer_content),
                    "size": len(layer_content),
                }
            ],
        }

    def _attestation_manifest(self, subject: dict[str, Any]) -> dict[str, Any]:
        layer = b"offline attestation statement"
        return {
            "schemaVersion": 2,
            "mediaType": VERIFIER.OCI_MANIFEST,
            "artifactType": VERIFIER.ATTESTATION_ARTIFACT,
            "config": {
                "mediaType": "application/vnd.oci.empty.v1+json",
                "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
                "size": 2,
                "data": "e30=",
            },
            "layers": [
                {
                    "mediaType": "application/vnd.in-toto+json",
                    "digest": digest(layer),
                    "size": len(layer),
                    "annotations": {
                        "in-toto.io/predicate-type": "https://slsa.dev/provenance/v1"
                    },
                }
            ],
            "subject": {
                "mediaType": subject["mediaType"],
                "digest": subject["digest"],
                "size": subject["size"],
            },
        }

    def _add_component(self, component: str, with_attestations: bool) -> None:
        repository = f"yourown-chat/kagent-preview/substrate/{component}"
        self.repositories[component] = repository
        descriptors: list[dict[str, Any]] = []
        platform_digests: dict[str, str] = {}
        for platform_key, architecture in (
            ("linux_amd64", "amd64"),
            ("linux_arm64", "arm64"),
        ):
            child_digest, child_size, _ = self.registry.add(
                repository, self._image_manifest(component, architecture)
            )
            platform = {"architecture": architecture, "os": "linux"}
            if architecture == "arm64":
                platform["variant"] = "v8"
            descriptor = {
                "mediaType": VERIFIER.OCI_MANIFEST,
                "digest": child_digest,
                "size": child_size,
                "platform": platform,
            }
            descriptors.append(descriptor)
            platform_digests[platform_key] = child_digest
            if with_attestations:
                attestation_digest, attestation_size, _ = self.registry.add(
                    repository, self._attestation_manifest(descriptor)
                )
                descriptors.append(
                    {
                        "mediaType": VERIFIER.OCI_MANIFEST,
                        "digest": attestation_digest,
                        "size": attestation_size,
                        "annotations": {
                            VERIFIER.REFERENCE_DIGEST_ANNOTATION: child_digest,
                            VERIFIER.REFERENCE_TYPE_ANNOTATION: VERIFIER.ATTESTATION_REFERENCE_TYPE,
                        },
                        "platform": {"architecture": "unknown", "os": "unknown"},
                    }
                )
        index = {
            "schemaVersion": 2,
            "mediaType": VERIFIER.OCI_INDEX,
            "manifests": descriptors,
        }
        self.index_documents[component] = index
        self._publish_index(component)
        self.evidence["platform_image_digests"][component] = platform_digests

    def _publish_index(self, component: str) -> None:
        index_digest, _, _ = self.registry.add(
            self.repositories[component], self.index_documents[component]
        )
        self.evidence["images"][component] = {
            "ref": f"{VERIFIER.EXPECTED_RELEASE_PREFIX}/{component}@{index_digest}",
            "digest": index_digest,
        }

    def mutate_index(self, component: str, mutation: Any) -> None:
        index = copy.deepcopy(self.index_documents[component])
        mutation(index)
        self.index_documents[component] = index
        self._publish_index(component)

    def mutate_attestation_child(self, component: str, mutation: Any) -> None:
        index = copy.deepcopy(self.index_documents[component])
        descriptor = next(
            item
            for item in index["manifests"]
            if item.get("platform") == {"architecture": "unknown", "os": "unknown"}
        )
        old_url = manifest_url(self.repositories[component], descriptor["digest"])
        child = json.loads(self.registry.responses[old_url].content)
        mutation(child)
        child_digest, child_size, _ = self.registry.add(self.repositories[component], child)
        descriptor["digest"] = child_digest
        descriptor["size"] = child_size
        self.index_documents[component] = index
        self._publish_index(component)


class PrivateImageVerifierTests(unittest.TestCase):
    def test_default_transport_rejects_registry_redirects(self) -> None:
        handler = VERIFIER.RejectRegistryRedirects()
        request = urllib.request.Request(
            f"https://{VERIFIER.EXPECTED_REGISTRY_HOST}/v2/repository/manifests/"
            + "sha256:"
            + "a" * 64,
            headers={"Authorization": "Basic secret"},
        )
        with self.assertRaisesRegex(
            VERIFIER.VerificationError, "registry redirects are forbidden"
        ):
            handler.redirect_request(
                request,
                None,
                302,
                "Found",
                {"Location": "http://attacker.invalid/token"},
                "http://attacker.invalid/token",
            )

    def test_accepts_exact_live_indexes_and_fetches_every_child(self) -> None:
        fixture = ReleaseFixture()
        VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

        expected_requests = 4 + 8 + 2
        self.assertEqual(expected_requests, len(fixture.registry.requests))
        for request in fixture.registry.requests:
            self.assertEqual(
                "Basic "
                + fixture.registry_config["auths"][VERIFIER.EXPECTED_REGISTRY_HOST]["auth"],
                request.get_header("Authorization"),
            )
            self.assertEqual("identity", request.get_header("Accept-encoding"))

    def test_rejects_non_exact_component_set_before_network(self) -> None:
        fixture = ReleaseFixture()
        fixture.evidence["images"]["extra"] = copy.deepcopy(
            fixture.evidence["images"]["ateapi"]
        )
        with self.assertRaisesRegex(VERIFIER.VerificationError, "image set is not exact"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)
        self.assertEqual([], fixture.registry.requests)

    def test_accepts_valid_index_annotations(self) -> None:
        fixture = ReleaseFixture()

        def add_annotations(index: dict[str, Any]) -> None:
            index["annotations"] = {
                "org.opencontainers.image.base.digest": "sha256:" + "a" * 64,
                "org.opencontainers.image.base.name": "example.invalid/base:stable",
            }

        fixture.mutate_index("ateapi", add_annotations)
        VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_accepts_oci_image_config_artifact_type(self) -> None:
        fixture = ReleaseFixture()

        def add_artifact_type(index: dict[str, Any]) -> None:
            for descriptor in index["manifests"]:
                if descriptor["platform"]["os"] == "linux":
                    descriptor["artifactType"] = VERIFIER.OCI_IMAGE_CONFIG

        fixture.mutate_index("ateapi", add_artifact_type)
        VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_unexpected_artifact_type(self) -> None:
        fixture = ReleaseFixture()

        def add_artifact_type(index: dict[str, Any]) -> None:
            index["manifests"][0]["artifactType"] = "application/example"

        fixture.mutate_index("ateapi", add_artifact_type)
        with self.assertRaisesRegex(VERIFIER.VerificationError, "artifactType is invalid"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_unexpected_index_field(self) -> None:
        fixture = ReleaseFixture()

        def add_unexpected_field(index: dict[str, Any]) -> None:
            index["subject"] = {"digest": "sha256:" + "a" * 64}

        fixture.mutate_index("ateapi", add_unexpected_field)
        with self.assertRaisesRegex(VERIFIER.VerificationError, "unexpected fields"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_credential_helper_without_basic_auth(self) -> None:
        fixture = ReleaseFixture()
        registry_config = {"credHelpers": {VERIFIER.EXPECTED_REGISTRY_HOST: "gcloud"}}
        with self.assertRaisesRegex(VERIFIER.VerificationError, "auths must be an object"):
            VERIFIER.verify(fixture.evidence, registry_config, opener=fixture.registry)
        self.assertEqual([], fixture.registry.requests)

    def test_rejects_static_basic_credentials(self) -> None:
        fixture = ReleaseFixture()
        fixture.registry_config["auths"][VERIFIER.EXPECTED_REGISTRY_HOST]["auth"] = (
            base64.b64encode(b"static-user:static-password").decode()
        )
        with self.assertRaisesRegex(VERIFIER.VerificationError, "short-lived oauth2accesstoken"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_live_index_digest_header_mismatch(self) -> None:
        fixture = ReleaseFixture()
        component = "agentgateway"
        index_digest = fixture.evidence["images"][component]["digest"]
        response = fixture.registry.responses[
            manifest_url(fixture.repositories[component], index_digest)
        ]
        response.headers["Docker-Content-Digest"] = "sha256:" + "f" * 64
        with self.assertRaisesRegex(VERIFIER.VerificationError, "digest header mismatch"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_platform_digest_not_equal_to_evidence(self) -> None:
        fixture = ReleaseFixture()
        fixture.evidence["platform_image_digests"]["agentgateway"]["linux_amd64"] = (
            "sha256:" + "f" * 64
        )
        with self.assertRaisesRegex(VERIFIER.VerificationError, "does not match evidence"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_unexpected_platform_descriptor(self) -> None:
        fixture = ReleaseFixture()

        def add_s390x(index: dict[str, Any]) -> None:
            child_digest, child_size, _ = fixture.registry.add(
                fixture.repositories["agentgateway"],
                fixture._image_manifest("agentgateway", "s390x"),
            )
            index["manifests"].append(
                {
                    "mediaType": VERIFIER.OCI_MANIFEST,
                    "digest": child_digest,
                    "size": child_size,
                    "platform": {"architecture": "s390x", "os": "linux"},
                }
            )

        fixture.mutate_index("agentgateway", add_s390x)
        with self.assertRaisesRegex(VERIFIER.VerificationError, "unsupported child platform"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_unknown_platform_without_exact_attestation_annotations(self) -> None:
        fixture = ReleaseFixture()

        def remove_reference_type(index: dict[str, Any]) -> None:
            descriptor = next(
                item
                for item in index["manifests"]
                if item.get("platform")
                == {"architecture": "unknown", "os": "unknown"}
            )
            del descriptor["annotations"][VERIFIER.REFERENCE_TYPE_ANNOTATION]

        fixture.mutate_index("agentgateway", remove_reference_type)
        with self.assertRaisesRegex(VERIFIER.VerificationError, "not a valid unknown/unknown"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_attestation_child_with_wrong_subject(self) -> None:
        fixture = ReleaseFixture()

        def change_subject(child: dict[str, Any]) -> None:
            child["subject"]["digest"] = "sha256:" + "f" * 64

        fixture.mutate_attestation_child("agentgateway", change_subject)
        with self.assertRaisesRegex(VERIFIER.VerificationError, "subject does not bind"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)

    def test_rejects_child_body_not_matching_descriptor_digest(self) -> None:
        fixture = ReleaseFixture()
        child_digest = fixture.evidence["platform_image_digests"]["agentgateway"]["linux_amd64"]
        response = fixture.registry.responses[
            manifest_url(fixture.repositories["agentgateway"], child_digest)
        ]
        response.content += b"\n"
        with self.assertRaisesRegex(VERIFIER.VerificationError, "body digest mismatch"):
            VERIFIER.verify(fixture.evidence, fixture.registry_config, opener=fixture.registry)


if __name__ == "__main__":
    unittest.main(verbosity=2)
