# External runtime Harnesses

An external `Harness` compiles portable agent behavior for a Codex or Claude
Code runtime connected through the reverse external gateway. kagent persists an
immutable revision, but it does not create a `WorkerPool`, `ActorTemplate`, Pod,
or other in-cluster compute for that revision.

This foundation must be released together with profile dispatch and a strict
local-host consumer. Until those pieces are present, `ExternalRuntimePrepared`
means only that the revision was compiled and persisted; it does not mean that
a compatible runtime slot is online or that the instruction has been applied.

## Minimal Codex example

```yaml
apiVersion: kagent.dev/v1alpha3
kind: Harness
metadata:
  name: codex
  namespace: agents
spec:
  codex: {}
  allowedAgentTemplates:
    selector:
      matchLabels:
        runtime.kagent.dev/codex: "true"
---
apiVersion: kagent.dev/v1alpha3
kind: AgentTemplate
metadata:
  name: reviewer
  namespace: agents
  labels:
    runtime.kagent.dev/codex: "true"
spec:
  # The v1alpha3 wire format still requires this field. External compilers do
  # not resolve the reference or copy model credentials into the revision.
  modelConfig:
    name: unused-for-external-runtime
  description: Review code changes
  systemPrompt: Review the requested change and report concrete findings.
```

Use `claude: {}` instead of `codex: {}` for Claude Code. Exactly one runtime
variant is allowed. `workload`, `substrate`, and `env` are required for the
in-cluster kagent runtime and forbidden for external runtimes.

## Portable profile boundary

The persisted external profile contains only:

```json
{"version":"v1","instruction":"...","tools":[]}
```

Model, reasoning effort, speed, filesystem access, credentials, executable
paths, and sandbox settings remain local Agent Card policy. Cluster
`ModelConfig` values, MCP URLs, headers, TLS material, and Secret values are not
copied into the profile or revision provenance.

External v1 profiles currently reject AgentTemplate skills, plugins, shared
agent tools, MCP headers/TLS, and empty MCP allowlists. MCP entries contain only
the logical `RemoteMCPServer` name and allowed tool names; a local host must map
that name to an explicitly configured local endpoint and fail closed when the
mapping or isolation support is unavailable.

## Rollout constraints

- Enable the reverse gateway only with one controller replica; the Helm chart
  enforces `Recreate` because sessions are currently process-local.
- Release migration 19 with the matching controller binary. Do not run mixed
  migration-18 and migration-19 controller replicas: the older generated reader
  uses `SELECT *` and does not understand the added profile column.
- Adding backend identity to the revision digest produces one new revision for
  each existing in-cluster pair on first reconciliation. Capacity-plan that
  one-time recompilation.
- Execute the migration 19 PostgreSQL round-trip tests before merging or
  deploying this feature stack.
