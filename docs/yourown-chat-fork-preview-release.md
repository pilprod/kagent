# YourOwn.Chat private kagent preview release

This runbook publishes the ExternalSlot testbed through the private Google
Cloud release rail. It does not authorize a Git push, Terraform apply, package
publication, or GKE promotion; each remains a separately reviewed operation.

The preview contains the kagent controller and UI, the declarative Go agent
runtime, the Codex harness image, the kagent charts, and code-only native Codex
and Claude adapters. It does not publish an image with Claude Code preinstalled.
That image remains disabled until the Anthropic Commercial Terms gate is
explicitly satisfied.

## Release boundary

The only supported kagent preview publisher is the Pub/Sub-triggered Cloud
Build owned by the `app-gcp` Stack. It writes immutable images and OCI charts to
the private Artifact Registry repository and writes release evidence to the
private evidence bucket.

The historical `v*.kap.*` GitHub Actions workflow and the checked-in
`cloudbuild.fork-preview.yaml` public fallback are guard-only files. They fail
unconditionally and contain no GHCR credential or publication capability. Do
not submit either one. Private releases use annotated `gcp-v*.kap.*` source
tags.

GitHub evaluates a tag-triggered workflow from the tagged commit, so a HEAD
guard cannot protect old commits by itself. The repository-level workflow state
is therefore part of the private-only boundary. Before creating any tag, verify
that both public publishers remain `disabled_manually` in `pilprod/kagent`:

- `Retired public fork preview release` (workflow ID `346150199`);
- `Tag and Push` (workflow ID `340304832`).

Re-enabling either workflow invalidates the private-only release precondition,
including for tags that point to historical commits. PR and CI workflows remain
enabled. The private Cloud Build source gate reads this repository state again
and fails before building if either public publisher has been re-enabled.

## 1. Verify the immutable Substrate dependency

`.github/gke-preview-substrate-pin.json` must have `status: ready` and bind all
of the following to one reviewed source release:

- public `github.com/pilprod/substrate` Go module `v0.0.22`, with its checksum,
  annotated source tag, source commit, source tree, and chart tree;
- private application and CRD charts at version `0.0.22-private.3`, including
  the registry digest and package SHA-256 of each chart;
- generation-qualified private release evidence URI and its SHA-256;
- profile `external-control-plane-only`;
- exactly four runtime components (`agentgateway`, `ateapi`, `atecontroller`,
  and `atenet`) plus the auxiliary `releaseVerifier`, with a private copied
  index for both `linux/amd64` and `linux/arm64`.

Pin schema 3 consumes producer evidence schema
`yourown.chat/substrate-private-gar-release/v2`. The verifier checks the public
Go proxy and checksum database, annotated source tag, both exact private charts,
the exact evidence object and Helm values, scan policy, source-to-private copy
provenance, and all five private GAR image indexes. For every image it also
fetches the two declared child manifests and verifies their digests; only valid
`unknown/unknown` attestation descriptors may accompany the two runtime
platforms. `releaseVerifier` is a deployment-time verification image. It is not
a Substrate runtime component, chart workload, or Helm image-digest value.

The release job supplies two short-lived inputs:

- `SUBSTRATE_RELEASE_EVIDENCE` and its identical generation-qualified
  `SUBSTRATE_RELEASE_EVIDENCE_URI`;
- `HELM_REGISTRY_CONFIG`, containing a short-lived Google access token for
  `europe-west3-docker.pkg.dev`.

No GitHub token, static registry password, local `go.work`, filesystem replace,
or private Go proxy is accepted by this dependency gate. Public GHCR refs are
provenance only; the verifier does not pull them or accept them as deployment
coordinates.

Run the semantic suite first, then the live verifier:

```sh
scripts/test-verify-gke-preview-substrate-pin.sh
GOWORK=off \
GOPROXY=https://proxy.golang.org \
GOSUMDB=sum.golang.org \
GOPRIVATE= \
GONOPROXY= \
GONOSUMDB= \
HELM_REGISTRY_CONFIG=/path/to/short-lived-registry-config.json \
SUBSTRATE_RELEASE_EVIDENCE=/path/to/release-evidence.json \
SUBSTRATE_RELEASE_EVIDENCE_URI='gs://bucket/substrate/version/release-evidence.json#generation' \
  scripts/verify-gke-preview-substrate-pin.sh
```

## 2. Publish the kagent fork preview

Merge the reviewed source to the fork branch first. Create an annotated tag on
the merge commit with this coordinate (the current successor is
`gcp-v0.0.0-external-slot.kap.4`):

```text
gcp-v<major>.<minor>.<patch>-<label>.kap.<sequence>
```

Never move or reuse `gcp-v0.0.0-external-slot.kap.3`; its failed publication
coordinate remains immutable.

Apply the exact merge commit and generation-qualified Substrate evidence URI in
`app-gcp`. After the Terraform plan shows only the expected publisher update,
publish the exact tag to the configured Pub/Sub topic. The private trigger then:

1. proves the repository, annotated tag, branch reachability, and clean source;
2. verifies the pinned Substrate evidence, live private chart, and live private
   image manifests;
3. runs the release-critical Go, chart, and contract suites;
4. builds the controller, UI, `golang-adk`, and Codex harness images for
   `linux/amd64` and `linux/arm64` under build-unique candidate coordinates;
5. scans candidates and acquires an immutable generation-zero release lock;
6. promotes digest-verified images and reproducible charts to final private GAR
   coordinates;
7. writes generation-qualified deployment evidence and a Cloud Build receipt.

The evidence schema records only chart-installed `controller` and `ui` under
`image_refs`. The declarative runtime and Codex harness remain separately
activated under `runtime_images.kagentHarness` and
`runtime_images.codexHarness`. `runtime_images.claudeHarness` is intentionally
absent. Every workload image is digest-qualified and propagated unchanged from
`Harness.spec.workload.image` into the compiled Substrate `ActorTemplate`.

If a build fails after acquiring the immutable lock, do not overwrite or delete
the partial release. Review its receipt and issue a new `.kap.<sequence>` tag.

## 3. Local Agent Host release

The Local Agent Host is released independently after its exact Substrate pin
passes with `GOWORK=off`. Its native release creates Node-free Linux and macOS
archives for amd64 and arm64. The archives contain only the Go host and control
binaries; provider CLIs and credentials are never bundled.

The host authenticates outbound to kagent and starts Codex or Claude as a local
process or container when instructed by kagent. Temporal calls only the
in-cluster kagent AgentInstance/A2A endpoint; it never calls the Local Agent
Host, Codex, or Claude directly.

## 4. Promote the GKE candidate

Consume only the generation-qualified private release evidence and its
digest-qualified image/chart references. Render the ExternalSlot testbed from
that evidence:

```sh
./examples/external-slot-testbed/render.sh codex ./release-evidence.json \
  > /tmp/kagent-codex-testbed.yaml
```

The `app-gcp` release pipeline promotes the reviewed candidate to the GKE
environment. The final bundle supplies the namespace, endpoints, database
bindings, WorkerPool image, and flow declarations; the kagent release does not
invent those infrastructure values. Promote `dev.kagent.yourown.chat` first,
run the local Codex agent smoke test without port forwarding, and promote the
same immutable release to `kagent.yourown.chat` only after that verification.
