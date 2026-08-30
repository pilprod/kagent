# YourOwn.Chat fork preview release

This runbook publishes the ExternalSlot testbed in a fixed order. It does not
authorize a Git push, GitHub setting change, package publication, or GKE apply.
Each of those remains a separately reviewed operation.

The preview contains the kagent controller and UI, the Codex harness image,
the kagent charts, and code-only native Codex and Claude adapters. It does not
publish an image with Claude Code preinstalled. Such an image remains disabled
until the Anthropic Commercial Terms gate is explicitly satisfied.

## Preconditions

1. `.github/gke-preview-substrate-pin.json` has `status: ready` and identifies
   one compatible public `github.com/kagent-dev/substrate` Go module release
   plus the matching `oci://ghcr.io/kagent-dev/substrate/helm/substrate` chart.
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

Publish one official immutable Substrate release containing every capability
listed in `.github/gke-preview-substrate-pin.json`. The Go module and Helm chart
must come from the declared `kagent-dev` identities and share the same semantic
version. Record the module checksums and origin tag/commit together with the
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

- digest-qualified controller, UI, and Codex harness image evidence;
- digest-qualified dependency-free kagent and CRD charts;
- reproducible code-only `kagent-codex-*` and `kagent-claude-*` native archives;
- `SHA256SUMS`, provenance attestations, and `release-evidence.json`.

`release-evidence.json` intentionally has no `claudeHarness` image entry.

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
