# Claude Harness

The Claude Harness runs Claude Code as a native Kagent runtime. It compiles an
`AgentTemplate` into a credential-free portable configuration for an enrolled
Substrate `ExternalSlot` and exposes each turn through Kagent's A2A API.

## Working

- [x] Anthropic `ModelConfig` model selection
- [x] Streaming text, tool calls, and tool results over A2A
- [x] Task cancellation
- [x] Durable Claude session resume between turns
- [x] Claude Code built-in tools
- [x] Owner-selected absolute Claude executable, home, workspace, and state paths
- [x] Independent per-launch readiness and mutual-transport tokens

## Planned / not yet supported

- [ ] Human-in-the-loop tool approval with deferred tool calls and session resume
- [ ] Checkpoint and fork continuity for Claude sessions
- [ ] Enforced selection of individual tools from an MCP server
- [ ] Dedicated subagents running in separate AgentInstances
- [ ] Skills, MCP tools, and nested subagents on local subagents
- [ ] Configuring Claude Code permission mode and trust boundary in Harness CRD
- [ ] Portable v2 materialization for MCP, skills, plugins, and shared agents

## Example Usage

Use the credential-free bundle in
[`examples/external-slot-testbed`](../../../examples/external-slot-testbed/README.md).
Its Claude `Harness` deliberately has no `spec.substrate`: the Claude compiler
selects `ExternalSlot`, while the enrolled host owns the executable, absolute
paths, and launch tokens. The cluster-supplied portable v2 config cannot choose
argv, filesystem paths, or credentials.

The Claude renderer requires a digest-qualified `images.claudeHarness` value in
release evidence. A Claude-enabled image is intentionally not published until
the Anthropic Commercial Terms gate is explicitly satisfied.

## External-host configuration boundary

External-host mode deliberately does not pass `--bare`: current Claude Code
releases skip subscription OAuth and system-keychain reads in bare mode. The
owner must instead enroll a dedicated, private `CLAUDE_CONFIG_DIR`; its
authentication state and any machine-managed Claude policy remain trusted local
inputs and are never supplied by the cluster.

The invocation passes an empty `--setting-sources` value plus one immutable
Harness-generated `--settings` file. This excludes user, project, and local
settings and their `CLAUDE.md` context while retaining subscription login.
Hooks are disabled by that explicit settings file, MCP discovery is restricted
by `--strict-mcp-config` paired with an activation-scoped, owner-only
`{"mcpServers":{}}` file, automatic memory and first-party claude.ai MCP servers
are disabled in the process environment, and only Harness-generated
`--plugin-dir` paths are added. Mandatory machine-managed policy still applies
by design; an administrator who controls that policy is inside the local owner
trust boundary.
