# kagent chart

See the [Helm installation guide](../README.md) for general installation and
upgrade instructions. This page documents chart-specific external runtime
gateway settings. See [External runtime Harnesses](../../docs/external-runtime-harness.md)
for the portable profile boundary and rollout constraints.

## External runtime gateway

The external runtime gateway lets a local Codex or Claude Code connector make
an outbound connection to the in-cluster kagent controller. It is disabled by
default and has no inline token value: create the token Secret in the release
namespace before enabling the gateway.

```bash
kubectl -n kagent create secret generic kagent-external-gateway-token \
  --from-file=token=/secure/path/to/device-token
```

The token must contain 32-4096 visible ASCII bytes. A minimal values file is:

```yaml
controller:
  replicas: 1
  externalGateway:
    enabled: true
    deviceId: workstation-1
    codexSlotId: codex-1
    claudeSlotId: ""
    existingSecret:
      name: kagent-external-gateway-token
      key: token
```

`deviceId` and configured slot IDs must be lowercase DNS labels. Configure at
least one slot. The chart mounts only the selected Secret key, read-only, and
passes its file path to the controller. It does not create a Secret or accept a
token, encoded token, or `values_base64` field in Helm values.

Enabling the gateway creates a dedicated
`<release>-kagent-external-gateway` `ClusterIP` Service on port 8085. It never
inherits `controller.service.type`, so an existing controller `LoadBalancer` or
`NodePort` cannot publish the bearer-token transport. The chart does not add an
Ingress, external load balancer, TLS termination, Cloudflare resource, or
NetworkPolicy. Configure external exposure separately and terminate TLS before
traffic reaches this internal Service.

Gateway sessions currently live in controller memory. The chart therefore
requires `controller.replicas: 1` and switches the Deployment to `Recreate` so
an upgrade cannot briefly run two brokers. An upgrade or restart disconnects
clients, which must reconnect. Changing the Secret contents also requires a
controller rollout because the token is read when the process starts.
