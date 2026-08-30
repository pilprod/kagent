# YourOwn.Chat fork preview release

This runbook publishes the ExternalSlot testbed in a fixed order. It does not
authorize a Git push, GitHub setting change, package publication, or GKE apply.
Each of those remains a separately reviewed operation.

The preview contains the kagent controller and UI, the declarative Go agent
runtime, the Codex harness image, the kagent charts, and code-only native Codex
and Claude adapters. It does not publish an image with Claude Code preinstalled.
Such an image remains disabled until the Anthropic Commercial Terms gate is
explicitly satisfied.

## Preconditions

1. `.github/gke-preview-substrate-pin.json` has `status: ready` and identifies
   the exact public `github.com/pilprod/substrate` v0.0.22 Go module release
   plus the matching `oci://ghcr.io/pilprod/substrate/helm/substrate` chart.
   The manifest is intentionally blocked while the last public release lacks
   `ExternalSlot` and `OpenActorIngress`; null pin fields must not be invented.
2. Both kagent and the Local Agent Host use that exact immutable public
   Substrate version. Local `go.work` files, filesystem replacements, private
   proxies, and fork substitutions are development-only and forbidden in
   release jobs.
3. GitHub immutable releases are enabled for `pilprod/kagent` and
   `pilprod/yourown-chat-local-agent-host`.
4. The source commits, release coordinates, and target GKE bundle have been
   reviewed independently. A successful test run is not deployment approval.

## 1. Publish and pin compatible Substrate artifacts

Publish the reviewed immutable `pilprod/substrate` v0.0.22 release containing
every capability listed in `.github/gke-preview-substrate-pin.json`. The Go
module and Helm chart must come from the exact declared fork identities and
share the same semantic version. Record the module checksums and origin
tag/commit together with the
chart registry digest and package SHA-256, then change the manifest to
`status: ready`.

Run `scripts/test-verify-gke-preview-substrate-pin.sh`, followed by
`scripts/verify-gke-preview-substrate-pin.sh` with `GOWORK=off`, the public Go
proxy, and the public checksum database. A locally published fork preview may
still be used for development, but it cannot unlock this consumer release
rail. Do not publish kagent or the Local Agent Host while the manifest remains
blocked.

## 2. Publish the kagent fork preview

Create an annotated tag matching:

```text
v<major>.<minor>.<patch>-<label>.kap.<sequence>
```

The tag-owned `fork-preview-release.yaml` rail is exclusive for `.kap` tags;
the generic upstream tag workflow excludes them. Before publishing, the rail
proves the exact repository and annotated tag, the public Substrate pin and its
checksums, pinned base images and provider CLI archives, release-critical
tests, and the GitHub immutable-release setting.

The resulting prerelease contains:

- digest-qualified controller, UI, declarative Go agent runtime, and Codex
  harness image evidence;
- digest-qualified dependency-free kagent and CRD charts;
- reproducible code-only `kagent-codex-*` and `kagent-claude-*` native archives;
- `SHA256SUMS`, provenance attestations, and `release-evidence.json`.

The schema v3 release evidence records the declarative `golang-adk` runtime under
`runtime_images.kagentHarness`. Its only v2 API consumer is the immutable
`Harness.spec.workload.image` field; the controller propagates that exact
digest-qualified reference into the compiled `Revision` and then the Substrate
`ActorTemplate`. `controller.agentImage` and its `IMAGE_*` environment values
belonged to the removed deployment-backed Agent API and are intentionally absent
from this chart. `image_refs` contains only the chart-installed `controller` and
`ui`; Codex remains separate under `runtime_images.codexHarness`. Neither runtime
is activated merely by installing the chart. The application and CRD chart
entries carry explicit versions and digest-qualified `oci://` references. The
evidence also records the exact `source_repository`, `source_commit`, and
`helm/kagent` Git tree. Its chart-source contract proves that upstream commit
`059c01b68584dea113ccdf80f2e356c2d051e02a` removed the obsolete
`controller.skillsInitImage` value and `skills-init` container; neither is
reintroduced by this fork. `release-evidence.json` intentionally has no
`runtime_images.claudeHarness` entry.

### Docker-less Cloud Build fallback

Use `cloudbuild.fork-preview.yaml` only when the reviewed GitHub Actions rail
cannot run. The submitter does not need Docker: Cloud Build checks out the
exact public commit, creates a digest-pinned Go/Helm/jq tool image, installs
QEMU, and uses Buildx on the remote worker. It publishes the four multi-platform
images and two OCI charts required by the deployment evidence; it does not
publish the optional native adapter archives or GitHub artifact attestations
made by the primary workflow. BuildKit still attaches maximum-mode provenance
and SBOM manifests to each multi-platform image, while the GCS checksum bundle
and Cloud Build identity form the fallback publication receipt.

The fallback deliberately does not create or push a Git tag and does not call
the GitHub Release API. OCI publication is not transactional. Every final ref
is checked for absence before publication; images are built under unique
candidate aliases first, their BuildKit digests are recorded, and the exact
digest-addressed indexes are required to contain only `linux/amd64` and
`linux/arm64` runtime manifests before any final alias can be written. Only then
are they carbon-copied to the final version aliases. Charts are packaged twice
and byte-compared before final image promotion, then pushed last. If any final ref
is left behind by a failed run, do not overwrite or delete it: inspect the
receipt and issue a new preview sequence.

One-time GCP setup requires a dedicated user-managed Cloud Build service
account, a dedicated evidence bucket, and an exact Secret Manager version
containing a GitHub personal access token (classic) with only
`write:packages`. Grant the build service account:

- `roles/logging.logWriter` in the build project;
- `roles/secretmanager.secretAccessor` on that one secret;
- `roles/storage.objectCreator` on that one evidence bucket.

The human submitter needs permission to create and inspect Cloud Builds and
`roles/iam.serviceAccountUser` on the build service account. The caller also
needs `roles/storage.objectViewer` on the evidence bucket for the post-build
download. The Cloud Build worker needs outbound HTTPS access to GitHub, GHCR,
the public Go proxy/checksum database, Docker Hub, `cgr.dev`, and the provider
CLI release endpoints. Package visibility is a separate GitHub setting: make
the published packages public, or configure GKE image-pull credentials, before
deployment.

Set the reviewed coordinates without reusing common system environment names:

```sh
export KAGENT_PREVIEW_PROJECT='YOUR_GCP_PROJECT'
export KAGENT_PREVIEW_REGION='YOUR_CLOUD_BUILD_REGION'
export KAGENT_PREVIEW_BUILD_SA="kagent-preview-publisher@${KAGENT_PREVIEW_PROJECT}.iam.gserviceaccount.com"
export KAGENT_PREVIEW_EVIDENCE_BUCKET='YOUR_DEDICATED_BUCKET'
export KAGENT_PREVIEW_GHCR_SECRET_VERSION="projects/${KAGENT_PREVIEW_PROJECT}/secrets/kagent-ghcr-write/versions/EXACT_VERSION"
export KAGENT_PREVIEW_VERSION='0.0.0-external-slot.kap.1'
export KAGENT_PREVIEW_COMMIT='REVIEWED_FULL_40_CHARACTER_COMMIT'
```

Authenticate interactively when required, then fail closed on source and
GitHub release state before submitting:

```sh
gcloud auth login
gcloud config set project "${KAGENT_PREVIEW_PROJECT}"
gcloud auth print-access-token >/dev/null
gh auth status

git fetch origin yourown-chat
test "$(git rev-parse HEAD)" = "${KAGENT_PREVIEW_COMMIT}"
git merge-base --is-ancestor "${KAGENT_PREVIEW_COMMIT}" origin/yourown-chat
test -z "$(git status --porcelain)"
test -z "$(git ls-remote --tags origin "refs/tags/v${KAGENT_PREVIEW_VERSION}")"
test "$(gh api repos/pilprod/kagent/immutable-releases --jq .enabled)" = true
if gh release view "v${KAGENT_PREVIEW_VERSION}" >/dev/null 2>&1; then
  printf 'release already exists\n' >&2
  exit 1
fi
```

Submit no local source. The config clones and verifies the exact remote commit,
so ignored or dirty workstation files cannot enter an artifact:

```sh
KAGENT_PREVIEW_BUILD_ID="$(
  gcloud builds submit \
    --async \
    --no-source \
    --project "${KAGENT_PREVIEW_PROJECT}" \
    --region "${KAGENT_PREVIEW_REGION}" \
    --config cloudbuild.fork-preview.yaml \
    --service-account "projects/${KAGENT_PREVIEW_PROJECT}/serviceAccounts/${KAGENT_PREVIEW_BUILD_SA}" \
    --substitutions "_VERSION=${KAGENT_PREVIEW_VERSION},_SOURCE_COMMIT=${KAGENT_PREVIEW_COMMIT},_GHCR_USERNAME=pilprod,_GHCR_SECRET_VERSION=${KAGENT_PREVIEW_GHCR_SECRET_VERSION},_EVIDENCE_BUCKET=${KAGENT_PREVIEW_EVIDENCE_BUCKET}" \
    --format 'value(id)'
)"
test -n "${KAGENT_PREVIEW_BUILD_ID}"
gcloud builds log --stream "${KAGENT_PREVIEW_BUILD_ID}" \
  --project "${KAGENT_PREVIEW_PROJECT}" \
  --region "${KAGENT_PREVIEW_REGION}"
test "$(
  gcloud builds describe "${KAGENT_PREVIEW_BUILD_ID}" \
    --project "${KAGENT_PREVIEW_PROJECT}" \
    --region "${KAGENT_PREVIEW_REGION}" \
    --format 'value(status)'
)" = SUCCESS
```

Download the build-specific receipt and verify it before creating any source
tag. Use a fresh temporary directory; the Cloud Build output contains
digest-qualified chart-installed `controller` and `ui` refs, the separately
activated `kagentHarness` and `codexHarness` runtime refs, both OCI chart refs
and versions, chart archives, checksums, and the Cloud Build receipt:

```sh
KAGENT_PREVIEW_RECEIPT_DIR="$(mktemp -d)"
gcloud storage cp \
  "gs://${KAGENT_PREVIEW_EVIDENCE_BUCKET}/kagent/${KAGENT_PREVIEW_VERSION}/${KAGENT_PREVIEW_BUILD_ID}/*" \
  "${KAGENT_PREVIEW_RECEIPT_DIR}/"
(cd "${KAGENT_PREVIEW_RECEIPT_DIR}" && sha256sum --check SHA256SUMS)
(cd "${KAGENT_PREVIEW_RECEIPT_DIR}" && sha256sum --check release-evidence.json.sha256)
jq -e \
  --arg commit "${KAGENT_PREVIEW_COMMIT}" \
  --arg version "${KAGENT_PREVIEW_VERSION}" '
    .schemaVersion == 3 and
    .source_repository == "https://github.com/pilprod/kagent" and
    .source_commit == $commit and
    .tag == ("v" + $version) and
    (.image_refs | keys == ["controller", "ui"]) and
    (.runtime_images | keys == ["codexHarness", "kagentHarness"]) and
    (.charts | keys == ["application", "crds"]) and
    .charts.application.version == $version and
    .charts.crds.version == $version
  ' "${KAGENT_PREVIEW_RECEIPT_DIR}/release-evidence.json" >/dev/null
```

Only after that receipt and its six registry digests have been reviewed, create
the annotated tag at the already-published source commit. The tag is the last
source coordinate, followed by a draft prerelease that is published only after
its uploads are complete:

```sh
git fetch origin yourown-chat
git merge-base --is-ancestor "${KAGENT_PREVIEW_COMMIT}" origin/yourown-chat
test -z "$(git ls-remote --tags origin "refs/tags/v${KAGENT_PREVIEW_VERSION}")"
git tag -a "v${KAGENT_PREVIEW_VERSION}" "${KAGENT_PREVIEW_COMMIT}" \
  -m "kagent fork preview v${KAGENT_PREVIEW_VERSION}"
git push origin "refs/tags/v${KAGENT_PREVIEW_VERSION}"

gh release create "v${KAGENT_PREVIEW_VERSION}" \
  --repo pilprod/kagent \
  --verify-tag \
  --draft \
  --prerelease \
  --title "kagent fork preview v${KAGENT_PREVIEW_VERSION}" \
  --notes 'Immutable YourOwn.Chat preview; deploy only digest-qualified refs from release-evidence.json.'
gh release upload "v${KAGENT_PREVIEW_VERSION}" \
  --repo pilprod/kagent \
  "${KAGENT_PREVIEW_RECEIPT_DIR}"/*
gh release edit "v${KAGENT_PREVIEW_VERSION}" \
  --repo pilprod/kagent --draft=false
```

Pushing the tag may still enqueue the billing-blocked GitHub Actions workflow;
its overwrite guards will reject the already-published OCI version. The Cloud
Build receipt, not that failed duplicate run, is the publication evidence for
this fallback.

## 3. Publish the Local Agent Host

After its exact Substrate pin passes with `GOWORK=off`, create an annotated
`v<major>.<minor>.<patch>` tag on a commit reachable from the repository's
default branch. `native-release.yml` builds reproducible Node-free archives for
Linux and macOS, amd64 and arm64. The archives contain only the Go host and
control binaries; provider CLIs and credentials are never bundled.

## 4. Assemble the GKE candidate

Verify the checksums and provenance of both release manifests. Render the
ExternalSlot testbed only from kagent `release-evidence.json`:

```sh
./examples/external-slot-testbed/render.sh codex ./release-evidence.json \
  > /tmp/kagent-codex-testbed.yaml
```

Use the app-gcp pin helper to validate the Substrate handoff and produce only
the immutable part of the vendor bundle. Complete the reviewed namespaces,
endpoints, database bindings, WorkerPool image, and flow declarations
separately; the helper deliberately refuses to invent them.

Temporal talks only to the in-cluster kagent AgentInstance/A2A endpoint. It
does not call the Local Agent Host, Codex, or Claude directly. Apply the final
digest-pinned bundle to GKE only after a separate infrastructure approval.
