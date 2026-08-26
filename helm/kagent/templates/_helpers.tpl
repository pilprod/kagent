{{/*
Create a default fully qualified app name.
*/}}
{{- define "kagent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- if not .Values.nameOverride }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kagent.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kagent.selectorLabels" . }}
{{- if .Chart.Version }}
app.kubernetes.io/version: {{ .Chart.Version | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kagent
{{- with .Values.labels }}
{{ toYaml . | nindent 0 }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kagent.selectorLabels" -}}
app.kubernetes.io/name: {{ default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*Default model name*/}}
{{- define "kagent.defaultModelConfigName" -}}
default-model-config
{{- end }}

{{/*
Expand the namespace of the release.
Allows overriding it for multi-namespace deployments in combined charts.
*/}}
{{- define "kagent.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Namespaces where Substrate ate-api-server needs read access to Secrets and ConfigMaps
referenced by generated ActorTemplates (install namespace plus rbac.namespaces).
*/}}
{{- define "kagent.substrate.envSourceNamespaces" -}}
{{- $installNs := include "kagent.namespace" . -}}
{{- $extra := .Values.rbac.namespaces | default list -}}
{{- $all := append $extra $installNs | uniq | sortAlpha -}}
{{- join "," $all -}}
{{- end }}

{{/*
Watch namespaces - transforms list of namespaces cached by the controller into comma-separated string.
Precedence: controller.watchNamespaces (explicit override) > rbac.namespaces > empty (watch all).
*/}}
{{- define "kagent.watchNamespaces" -}}
{{- if .Values.controller.watchNamespaces -}}
  {{- .Values.controller.watchNamespaces | uniq | join "," -}}
{{- else if and .Values.rbac .Values.rbac.namespaces -}}
  {{- .Values.rbac.namespaces | uniq | join "," -}}
{{- end -}}
{{- end -}}

{{/*
Guards on the rbac block
*/}}
{{- define "kagent.rbac.validate" -}}
{{- if and .Values.rbac (hasKey .Values.rbac "clusterScoped") -}}
{{- fail "rbac.clusterScoped has been removed. Leave rbac.namespaces empty for cluster-scoped RBAC, or set rbac.namespaces=[<ns>, ...] for namespaced RBAC." -}}
{{- end -}}
{{- if and .Values.rbac .Values.rbac.namespaces -}}
{{- $installNs := include "kagent.namespace" . -}}
{{- if not (has $installNs .Values.rbac.namespaces) -}}
{{- fail (printf "rbac.namespaces is set but does not include the install namespace %q" $installNs) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Returns "1" when a PodDisruptionBudget threshold is explicitly set, empty otherwise.

Uses `kindIs "invalid"` rather than `default ""` so that an explicit `0` counts as
set: Helm's `default` treats 0 as empty, which would silently drop a
`maxUnavailable: 0` budget and render a manifest the user never asked for.
An empty string is also treated as unset, so `minAvailable: ""` disables the field.
*/}}
{{- define "kagent.pdb.isSet" -}}
{{- if not (kindIs "invalid" .) -}}
{{- if ne (toString .) "" -}}1{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Guards on a component `pdb` block.

Kubernetes rejects a PodDisruptionBudget that sets both `minAvailable` and
`maxUnavailable`, and a budget that sets neither is meaningless, so both cases
fail at template time with a message naming the offending values path rather
than surfacing later as an opaque API server error.

Call with a dict: (dict "pdb" .Values.controller.pdb "path" "controller.pdb")
*/}}
{{- define "kagent.pdb.validate" -}}
{{- $pdb := .pdb | default dict -}}
{{- if $pdb.enabled -}}
{{- $hasMin := include "kagent.pdb.isSet" $pdb.minAvailable -}}
{{- $hasMax := include "kagent.pdb.isSet" $pdb.maxUnavailable -}}
{{- if and $hasMin $hasMax -}}
{{- fail (printf "%s: minAvailable and maxUnavailable are mutually exclusive. Set exactly one (to use minAvailable, set %s.maxUnavailable=null)." .path .path) -}}
{{- end -}}
{{- if not (or $hasMin $hasMax) -}}
{{- fail (printf "%s is enabled but neither minAvailable nor maxUnavailable is set. Set exactly one." .path) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
UI selector labels
*/}}
{{- define "kagent.ui.selectorLabels" -}}
{{ include "kagent.selectorLabels" . }}
app.kubernetes.io/component: ui
{{- end }}

{{/*
Controller selector labels
*/}}
{{- define "kagent.controller.selectorLabels" -}}
{{ include "kagent.selectorLabels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Engine selector labels
*/}}
{{- define "kagent.engine.selectorLabels" -}}
{{ include "kagent.selectorLabels" . }}
app.kubernetes.io/component: engine
{{- end }}

{{/*
Controller labels
*/}}
{{- define "kagent.controller.labels" -}}
{{ include "kagent.labels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
UI labels
*/}}
{{- define "kagent.ui.labels" -}}
{{ include "kagent.labels" . }}
app.kubernetes.io/component: ui
{{- end }}

{{/*
Engine labels
*/}}
{{- define "kagent.engine.labels" -}}
{{ include "kagent.labels" . }}
app.kubernetes.io/component: engine
{{- end }}

{{/*
Check if leader election should be enabled (more than 1 replica)
*/}}
{{- define "kagent.leaderElectionEnabled" -}}
{{- gt (.Values.controller.replicas | int) 1 -}}
{{- end -}}

{{/*
Validate the external runtime gateway configuration. The broker owns sessions
in memory, so both the steady-state replica count and rollout strategy must
prevent two controller pods from accepting the same device token.
*/}}
{{- define "kagent.controller.externalGateway.validate" -}}
{{- $externalGateway := .Values.controller.externalGateway | default dict -}}
{{- range $entry := .Values.controller.env | default list -}}
  {{- $name := get $entry "name" | default "" -}}
  {{- if hasPrefix "EXTERNAL_GATEWAY_" $name -}}
    {{- fail (printf "controller.env[%s] is reserved; configure the external gateway through controller.externalGateway" $name) -}}
  {{- end -}}
{{- end -}}
{{- if (get $externalGateway "enabled") -}}
  {{- if ne (.Values.controller.replicas | int) 1 -}}
    {{- fail "controller.externalGateway requires controller.replicas=1 because gateway sessions are process-local" -}}
  {{- end -}}
  {{- $gatewayPort := include "kagent.controller.externalGateway.port" . -}}
  {{- $grpcBindPort := regexFind "[0-9]+$" (.Values.controller.grpc.bindAddress | toString) -}}
  {{- if eq $grpcBindPort $gatewayPort -}}
    {{- fail (printf "controller.grpc.bindAddress must not use external gateway port %s" $gatewayPort) -}}
  {{- end -}}
  {{- if eq (.Values.controller.service.ports.grpc | toString) $gatewayPort -}}
    {{- fail (printf "controller.service.ports.grpc must not use external gateway port %s" $gatewayPort) -}}
  {{- end -}}
  {{- if eq (.Values.controller.service.ports.targetPort | toString) $gatewayPort -}}
    {{- fail (printf "controller.service.ports.targetPort must not use external gateway port %s" $gatewayPort) -}}
  {{- end -}}
  {{- if and (include "kagent.controller.metricsEnabled" .) (eq (include "kagent.controller.metricsPort" .) $gatewayPort) -}}
    {{- fail (printf "controller.metrics.bindAddress must not use external gateway port %s" $gatewayPort) -}}
  {{- end -}}
  {{- $dnsLabelPattern := "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" -}}
  {{- $deviceID := get $externalGateway "deviceId" | default "" -}}
  {{- if or (not $deviceID) (gt (len $deviceID) 63) (not (regexMatch $dnsLabelPattern $deviceID)) -}}
    {{- fail "controller.externalGateway.deviceId must be a lowercase DNS label with at most 63 characters" -}}
  {{- end -}}
  {{- $codexSlotID := get $externalGateway "codexSlotId" | default "" -}}
  {{- $claudeSlotID := get $externalGateway "claudeSlotId" | default "" -}}
  {{- if and (not $codexSlotID) (not $claudeSlotID) -}}
    {{- fail "controller.externalGateway requires at least one of codexSlotId or claudeSlotId" -}}
  {{- end -}}
  {{- if and $codexSlotID (or (gt (len $codexSlotID) 63) (not (regexMatch $dnsLabelPattern $codexSlotID))) -}}
    {{- fail "controller.externalGateway.codexSlotId must be a lowercase DNS label with at most 63 characters" -}}
  {{- end -}}
  {{- if and $claudeSlotID (or (gt (len $claudeSlotID) 63) (not (regexMatch $dnsLabelPattern $claudeSlotID))) -}}
    {{- fail "controller.externalGateway.claudeSlotId must be a lowercase DNS label with at most 63 characters" -}}
  {{- end -}}
  {{- $existingSecret := get $externalGateway "existingSecret" | default dict -}}
  {{- $secretName := get $existingSecret "name" | default "" -}}
  {{- if not $secretName -}}
    {{- fail "controller.externalGateway.existingSecret.name is required" -}}
  {{- end -}}
  {{- $dnsSubdomainPattern := "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$" -}}
  {{- if or (gt (len $secretName) 253) (not (regexMatch $dnsSubdomainPattern $secretName)) -}}
    {{- fail "controller.externalGateway.existingSecret.name must be a valid DNS subdomain with labels of at most 63 characters" -}}
  {{- end -}}
  {{- if not (get $existingSecret "key") -}}
    {{- fail "controller.externalGateway.existingSecret.key is required" -}}
  {{- end -}}
  {{- range $volume := .Values.controller.volumes | default list -}}
    {{- if eq (get $volume "name" | default "") "external-gateway-token" -}}
      {{- fail "controller.volumes[external-gateway-token] is reserved while controller.externalGateway is enabled" -}}
    {{- end -}}
  {{- end -}}
  {{- $reservedMountPath := include "kagent.controller.externalGateway.tokenMountPath" $ -}}
  {{- range $volumeMount := .Values.controller.volumeMounts | default list -}}
    {{- $mountPath := trimSuffix "/" (get $volumeMount "mountPath" | default "") -}}
    {{- $overlapsReservedPath := and $mountPath (or
          (eq $mountPath $reservedMountPath)
          (hasPrefix (printf "%s/" $mountPath) $reservedMountPath)
          (hasPrefix (printf "%s/" $reservedMountPath) $mountPath)) -}}
    {{- if or (eq (get $volumeMount "name" | default "") "external-gateway-token") $overlapsReservedPath -}}
      {{- fail "the external gateway token volume and mount path are reserved while controller.externalGateway is enabled" -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{- define "kagent.controller.externalGateway.port" -}}8085{{- end -}}
{{- define "kagent.controller.externalGateway.tokenMountPath" -}}/var/run/secrets/kagent/external-gateway{{- end -}}
{{- define "kagent.controller.externalGateway.tokenFile" -}}{{ include "kagent.controller.externalGateway.tokenMountPath" . }}/token{{- end -}}

{{/*
Extract the TCP port from controller.metrics.bindAddress.

Anchors the digit run to the end of the string so every Go-style
address form the controller binary accepts is handled correctly: bare
":port", host-qualified "host:port", and bracketed IPv6 "[::1]:port"
all yield the trailing port. Returns "0" or "" when the binary's
disable sentinel is in use; callers must consult
`kagent.controller.metricsEnabled` before rendering manifests.
*/}}
{{- define "kagent.controller.metricsPort" -}}
{{- regexFind "[0-9]+$" (.Values.controller.metrics.bindAddress | toString) -}}
{{- end -}}

{{/*
Returns "1" when the controller metrics resources (Service, RBAC,
container port, env vars) should render, empty otherwise. Honours both
disable signals: `controller.metrics.enabled=false` and the binary's
own `--metrics-bind-address=0` sentinel reached through `bindAddress`.
The two are equivalent so the field name keeps faith with the binary's
documented contract (see go/core/pkg/app/app.go).
*/}}
{{- define "kagent.controller.metricsEnabled" -}}
{{- $port := include "kagent.controller.metricsPort" . -}}
{{- if and .Values.controller.metrics.enabled $port (ne $port "0") -}}1{{- end -}}
{{- end -}}

{{/*
Controller gRPC observability PrometheusRule name.
*/}}
{{- define "kagent.controller.grpcPrometheusRuleName" -}}
{{- printf "%s-controller-grpc" (include "kagent.fullname" .) -}}
{{- end -}}

{{/*
Controller gRPC observability Grafana dashboard ConfigMap name.
*/}}
{{- define "kagent.controller.grpcDashboardConfigMapName" -}}
{{- printf "%s-controller-grpc-dashboard" (include "kagent.fullname" .) -}}
{{- end -}}

{{/*
PostgreSQL service name for the bundled postgres instance
*/}}
{{- define "kagent.postgresqlServiceName" -}}
{{- printf "%s-postgresql" (include "kagent.fullname" .) -}}
{{- end -}}

{{/*
Bundled PostgreSQL image - constructs the full image reference from registry/repository/name/tag
*/}}
{{- define "kagent.postgresql.image" -}}
{{- $pg := .Values.database.postgres.bundled -}}
{{- $parts := compact (list $pg.image.registry $pg.image.repository $pg.image.name) -}}
{{- printf "%s:%s" (join "/" $parts) $pg.image.tag -}}
{{- end -}}

{{/*
Password secret name - returns the chart-managed Secret name for POSTGRES_PASSWORD.
*/}}
{{- define "kagent.passwordSecretName" -}}
{{- printf "%s-postgresql" (include "kagent.fullname" .) -}}
{{- end -}}

{{/*
A2A Base URL - computes the default URL based on the controller service name if not explicitly set.
The `name.namespace.svc` short form is used so the URL resolves regardless of the cluster's DNS domain.
*/}}
{{- define "kagent.a2aBaseUrl" -}}
{{- if .Values.controller.a2aBaseUrl -}}
{{- .Values.controller.a2aBaseUrl -}}
{{- else -}}
{{- printf "http://%s-controller.%s.svc:%d" (include "kagent.fullname" .) (include "kagent.namespace" .) (.Values.controller.service.ports.port | int) -}}
{{- end -}}
{{- end -}}

{{/* Public gRPC endpoint advertised by AgentInstance Agent Cards. */}}
{{- define "kagent.a2aGatewayUrl" -}}
{{- if .Values.controller.a2aGatewayUrl -}}
{{- .Values.controller.a2aGatewayUrl -}}
{{- else -}}
{{- printf "http://%s-controller.%s.svc:%d" (include "kagent.fullname" .) (include "kagent.namespace" .) (.Values.controller.service.ports.grpc | int) -}}
{{- end -}}
{{- end -}}

{{/*
Controller Service host:port for nginx upstream (no scheme).
*/}}
{{- define "kagent.controllerServiceAuthority" -}}
{{- printf "%s-controller.%s.svc:%d" (include "kagent.fullname" .) (include "kagent.namespace" .) (.Values.controller.service.ports.port | int) -}}
{{- end -}}

{{/*
imagePullSecrets from global values (for subchart usage).
Reads .Values.global.imagePullSecrets set by the parent chart.
*/}}
{{- define "kagent.imagePullSecrets" -}}
{{- $global := ((.Values.global).imagePullSecrets) | default list -}}
{{- if $global -}}
imagePullSecrets:
{{- toYaml $global | nindent 2 }}
{{- end -}}
{{- end -}}
