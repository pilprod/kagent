# Kagent Helm Chart

These Helm charts install kagent-crds and kagent. The kagent-crds chart must be installed first.

## Installation

### Using Helm

```bash
# First, install the required CRDs
helm install kagent-crds ./helm/kagent-crds/  --namespace kagent

# Then install kagent with default provider
# --set providers.default=openAI is enabled by default, but you need to provide your OpenAI API key
helm install kagent ./helm/kagent/ --namespace kagent --set providers.openAI.apiKey=your-openai-api-key

# Or with optional providers if you prefer local ollama provider or anthropic
helm install kagent ./helm/kagent/ --namespace kagent --set providers.default=ollama
helm install kagent ./helm/kagent/ --namespace kagent --set providers.default=openAI       --set providers.openAI.apiKey=your-openai-api-key
helm install kagent ./helm/kagent/ --namespace kagent --set providers.default=anthropic    --set providers.anthropic.apiKey=your-anthropic-api-key
helm install kagent ./helm/kagent/ --namespace kagent --set providers.default=azureOpenAI  --set providers.azureOpenAI.apiKey=your-openai-api-key
```

### Using Make

```bash
# export your openAI key
export OPENAI_API_KEY=your-openai-api-key
export ANTHROPIC_API_KEY=your-anthropic-api-key
export AZURE_OPENAI_API_KEY=your-azure-api-key

# install the kagent charts with openAI provider 
make KAGENT_DEFAULT_MODEL_PROVIDER=openAI helm-install

# install charts with anthropic provider
make KAGENT_DEFAULT_MODEL_PROVIDER=anthropic helm-install

# install charts with azureOpenAI provider
make KAGENT_DEFAULT_MODEL_PROVIDER=azureOpenAI helm-install

# install charts with ollama provider
make KAGENT_DEFAULT_MODEL_PROVIDER=ollama helm-install
```

The Make target regenerates protobuf bindings, rebuilds all local images, and
rolls the controller and UI before installing. The UI uses the controller's
native gRPC application API on port `8084`; controller port `8083` remains for
A2A, MCP, ACP, and operational HTTP endpoints.

### External Substrate TLS on GKE

The default `controller.substrate.tls.mode=projected` preserves the upstream
PodCertificate and ClusterTrustBundle integration. On GKE clusters where those
beta volume sources are unavailable, use `existingSecret` and provision the TLS
Secret before installing kagent. Keep the bundled `substrate.enabled=false`;
this profile connects to the separately installed external control plane:

```yaml
controller:
  substrate:
    enabled: true
    ateApiEndpoint: dns:///api.ate-system.svc:443
    tls:
      mode: existingSecret
      serverName: api.ate-system.svc
      existingSecret:
        name: kagent-ate-client-tls
        serverCAKey: server-ca.pem
        clientCredentialBundleKey: client-credential-bundle.pem
```

This chart accepts only the Secret name and data-key mappings. It never accepts
PEM data in values and never creates or copies the Secret. The named Secret must
exist in the kagent release namespace and contain:

- `serverCAKey`: a PEM CA bundle that verifies the ate-api serving certificate;
- `clientCredentialBundleKey`: the client private key plus certificate chain in
  a single PEM credential bundle accepted by `tls.LoadX509KeyPair`.

Both files are mounted read-only with mode `0440`. In this mode the chart adds
the controller image's non-root group (`65532`) as the default pod `fsGroup`;
an explicitly configured controller/global pod `fsGroup` takes precedence.
The two data keys must differ so the private-key credential bundle cannot also
be mounted as a trust bundle. The chart also rejects invalid Kubernetes Secret
names and keys.

`serverName` is mandatory in this mode and is restricted to an internal
Kubernetes Service DNS name (`<service>.<namespace>.svc` or its
`.cluster.local` form). The ate-api server leaf certificate must contain that
exact DNS SAN. The client leaf must be valid for TLS client authentication,
chain to the CA trusted by ate-api, and contain the governed workload identity
as a URI SAN. For the chart-created controller ServiceAccount the normal SPIFFE
identity is
`spiffe://cluster.local/ns/<release-namespace>/sa/<release-name>-controller`.
ate-api treats the first URI SAN as the mTLS principal; no bearer-token fallback
is added by this chart.

Kubernetes updates mounted Secret volumes atomically. The kagent client reloads
the client leaf/key for each new TLS handshake, so new connections can use a
rotated credential bundle without restarting. It reads the server CA pool only
when the controller starts. Rotate trust roots with an overlap window and roll
out the kagent controller after changing `serverCAKey`; existing connections
retain the credentials from their established handshake.

In `existingSecret` mode the controller Deployment contains neither a
`podCertificate` nor a `clusterTrustBundle` projected source and has no
`certificates.k8s.io` dependency. Rendering fails if the bundled Substrate
subchart is enabled, because that stack still depends on the beta certificate
APIs. This setting changes only kagent's connection to an already installed
external Substrate control plane.

### Using kagent cli

```bash
## make sure have env variable with your API_KEY
export OPENAI_API_KEY=your-openai-api-key
export ANTHROPIC_API_KEY=your-anthropic-api-key
export AZURE_OPENAI_API_KEY=your-azure-api-key

#default provider is openAI but you can select from the list 
export KAGENT_DEFAULT_MODEL_PROVIDER=ollama
export KAGENT_DEFAULT_MODEL_PROVIDER=azureOpenAI
export KAGENT_DEFAULT_MODEL_PROVIDER=anthropic

# use local helm chart to install kagent with openAI provider
export KAGENT_DEFAULT_MODEL_PROVIDER=openAI
export KAGENT_HELM_REPO=./helm/
make kagent-cli-install

# use local helm chart to install kagent with ollama provider
export KAGENT_DEFAULT_MODEL_PROVIDER=ollama
export KAGENT_HELM_REPO=./helm/
make kagent-cli-install

```

## Upgrading

When upgrading, make sure to upgrade both charts:

```bash
# First, upgrade the CRDs
helm upgrade kagent-crds ./helm/kagent-crds/  --namespace kagent

# Then upgrade Kagent
helm upgrade kagent ./helm/kagent/ --namespace kagent
```

## Uninstallation

To properly uninstall Kagent:

```bash
# First, uninstall Kagent
helm uninstall kagent --namespace kagent

# To completely remove all resources including CRDs (optional):
helm uninstall kagent-crds --namespace kagent
```

**Note**: Uninstalling the CRDs chart will delete all custom resources of those types across all namespaces.

## Why Separate CRDs?

Helm has a limitation where CRDs are installed but not removed during uninstallation. 
By separating CRDs into their own chart, we can:

1. Allow proper version control of CRDs
2. Enable users to choose when to remove CRDs (which is destructive)
3. Follow Helm best practices
