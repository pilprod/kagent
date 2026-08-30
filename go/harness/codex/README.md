# Codex Harness

The Codex Harness runs the pinned official Codex CLI through `codex app-server`
and exposes each durable thread through Kagent's private A2A runtime interface.
Kagent remains the control plane; Substrate decides where the revision runs.

## Runtime modes

- `in-cluster` is the image default and the adapter has an environment-backed
  Responses provider contract. It is not currently emitted by the compiler:
  Substrate cannot yet carry a Kubernetes Secret reference without resolving
  it into a literal ActorTemplate value, so the compiler rejects that path.
- `external-host` is the prepared ABI for an enrolled host. It requires
  explicit owner-selected `--data-dir`, `--workspace`, `--codex-home`, and
  `--codex-bin` paths and an isolated home authenticated locally with
  `codex login`. It consumes the controller's canonical credential-free v2
  Revision and locally translates only model, effort, service tier, and
  instruction into its private numeric-v1 runtime settings. Executable,
  version, argv, paths, credentials, network policy, and protocol limits remain
  adapter/host-owned. Before binding its private A2A endpoint, the adapter starts the
  pinned app-server over private stdio and proves that the selected named
  permission profile can write the workspace but cannot open `auth.json`
  directly or through a workspace symlink. Any failed proof is fail-closed.

The container image does not contain Node.js. It downloads the official Codex
musl archive for the target architecture, verifies its release SHA-256, and
checks the exact CLI version during the image build and at runtime startup. Its
builder and Alpine runtime bases are also digest-pinned, so a preview rebuild
cannot silently replace either base behind a mutable tag.

## Implemented

- Streaming assistant text and normalized tool activity over A2A
- Durable Codex thread resume
- Cancellation through `turn/interrupt` and process-group cleanup
- Model and reasoning effort from `ModelConfig`
- Declarative skills and direct HTTP MCP servers
- MCP tool allowlists and non-secret literal or ConfigMap-backed headers
- Fail-closed approval handling and external-sandbox policy
- A placement-neutral pre-authenticated configuration contract that never
  copies subscription credentials into the compiled Revision
- A narrow semantic gateway: cluster callers receive A2A turns and events, not
  raw app-server methods such as `fs/*`, `process/*`, `command/exec`,
  `thread/shellCommand`, `config/*`, or `account/*`

The image remains a preview artifact. It is built and scanned by CI, but is not
included in the upstream production tag matrix. The fork-only
`.github/workflows/fork-preview-release.yaml` rail accepts annotated
`v<semver>-<qualifier>.kap.<revision>` tags, rejects existing OCI tags, and
publishes controller, UI, Codex Harness, native adapter archives, chart
archives, attestations, and a digest-qualified `release-evidence.json`. It does
not publish `latest` or Python packages. The Helm chart accepts independent
`controller.image.digest` and `ui.image.digest` values so GKE promotion can use
the exact image manifests recorded by that evidence instead of mutable tags.

## Not yet supported

- Human approval and `input-required` resume
- Shared/dedicated subagents
- Checkpoint fork conformance for Codex threads
- A stable upstream app-server protocol independent of the pinned CLI version
- External-host state migration between enrolled hosts
- Secret delivery through Substrate without persisting literal values in an
  ActorTemplate
- External-host materialization of portable MCP grants, skills, plugins, and
  shared agents; non-empty declarations fail closed in this adapter version
- Admin-owned native `requirements.toml` enforcement for a hardened release
- Native Windows credential-isolation conformance; Windows remains fail-closed
- A deny-by-default host filesystem profile with declarative owner-approved
  toolchain read roots

## External process ABI

The enrolled external host, not a cluster-selected command, supplies the
owner-approved executable and paths plus the compiled
`KAGENT_CONFIG_JSON` and `KAGENT_AGENT_CARD_JSON` values received through its
Substrate assignment:

Before launch, enrollment creates the empty private regular marker file
`.yourown-chat-managed-codex-home` in the dedicated per-slot `CODEX_HOME`.
The adapter refuses an unmarked directory so it cannot accidentally rewrite a
user's general Codex configuration.

```sh
kagent-codex \
  --mode=external-host \
  --data-dir=/var/lib/kagent/actors/INSTANCE_ID \
  --workspace=/absolute/path/to/repository \
  --codex-home=/absolute/path/to/isolated/codex-home \
  --codex-bin=/absolute/owner-pinned/path/to/codex \
  --port=8080
```

The host also provides two independent per-launch values:
`YOUROWN_CHAT_TRANSPORT_TOKEN` keys a mutual HMAC handshake before HTTP/h2c,
while `YOUROWN_CHAT_READINESS_TOKEN` is returned only from `/readyz`. Each
connection uses a three-frame `HELLO -> CHALLENGE -> PROOF` exchange with fresh
client and server nonces and role-separated HMACs. The raw transport token
never crosses the socket, a fake listener cannot obtain the client proof before
proving the server role, and replaying an old proof fails against the fresh
server nonce. The runtime also rejects equal readiness and transport values.
These are host-owned launch inputs, not values accepted from the cluster
assignment. Users and Temporal continue to call Kagent rather than this process
directly.

Native mode is a preview boundary, not the final hardened profile. It keeps
host filesystem reads available so explicitly selected local tools can work,
then applies and runtime-tests exact denies for the Codex subscription files;
it does not claim to hide every unrelated credential belonging to the same OS
user. Until an installer can enforce an admin-owned requirements file and
explicit toolchain roots, run it under a dedicated local account or the Docker
mode when broader host isolation is required.
