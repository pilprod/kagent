# ExternalSlot coding-agent testbed

This bundle creates one credential-free `Harness` + `ModelConfig` +
`AgentTemplate` set for Codex or Claude. The resources describe agent behavior;
they do not select a worker, container launcher, process command, workspace,
home directory, executable, or credential. The matching compiler chooses
`ExternalSlot`, and an enrolled host supplies those owner-controlled runtime
details through the private Substrate assignment path.

The `kagent-testbed` namespace and an ExternalSlot-capable Substrate control
plane must already exist. Apply the CRDs before these resources. Use the
[fork preview release runbook](../../docs/yourown-chat-fork-preview-release.md)
to publish and pin Substrate, kagent, and the Local Agent Host first.

## Render and apply

Render from immutable release evidence. The renderer rejects tags and any
reference that is not pinned by a SHA-256 image digest. It requires `jq`:

```sh
./examples/external-slot-testbed/render.sh codex ./release-evidence.json \
  > /tmp/kagent-testbed-codex.yaml
kubectl apply -f /tmp/kagent-testbed-codex.yaml
```

Use `claude` instead of `codex` only with release evidence containing a
digest-qualified `images.claudeHarness` field. The fork preview release does
not publish a Claude-enabled image until the Anthropic Commercial Terms gate
has been explicitly satisfied; do not replace that missing evidence with a
mutable tag or an unrecorded local build.

After applying, wait until the selected `AgentTemplate` reports `Ready=True`
for its Harness before creating an `AgentInstance` through Kagent. Temporal and
other callers invoke the instance through Kagent's A2A endpoint; they never
connect to the local process directly.

## Trust boundary

- The manifests contain no API key, Secret reference, bearer token, or
  `spec.substrate` placement policy.
- `ModelConfig` selects only a provider-compatible model. Codex also selects
  reasoning effort and service tier. Subscription authentication remains in an
  isolated owner-controlled CLI home on the enrolled host.
- The cluster-provided portable config cannot choose argv or filesystem paths.
  The host supplies absolute executable, workspace, home, and state paths.
- The host supplies independent readiness and mutual-transport tokens for each
  launch. Neither token belongs in these Kubernetes objects.
- Declarative MCP, skills, plugins, and shared agents remain fail-closed until
  the enrolled host proves the corresponding materializer and capability path.

The Go compiler test renders both templates from fixture evidence, strictly
decodes every Kubernetes object, compiles each pair into an immutable Revision,
and asserts `ExternalSlot` placement with no Kubernetes worker or snapshot
policy.
