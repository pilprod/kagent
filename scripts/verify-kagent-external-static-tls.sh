#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/kagent-static-tls.XXXXXX")"
trap 'rm -rf -- "$tmp_root"' EXIT

prepare_chart() {
  local source_dir="$1"
  local target_dir="$2"

  cp -R "$source_dir" "$target_dir"
  rm -rf -- "$target_dir/charts"
  awk '
    /^dependencies:/ { exit }
    { gsub(/\$\{VERSION\}/, "0.0.0-static-tls-test"); print }
  ' "$target_dir/Chart-template.yaml" >"$target_dir/Chart.yaml"
}

render_default() {
  local chart_dir="$1"
  local output_file="$2"

  helm template release "$chart_dir" --namespace kagent >"$output_file"
}

sha256_file() {
  local file="$1"

  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  else
    shasum -a 256 "$file" | awk '{print $1}'
  fi
}

current_chart="$tmp_root/current-kagent"
prepare_chart "$repo_root/helm/kagent" "$current_chart"

default_render="$tmp_root/default.yaml"
render_default "$current_chart" "$default_render"
default_hash="$(sha256_file "$default_render")"
printf 'default-render-sha256=%s\n' "$default_hash"

if [[ -n "${KAGENT_HELM_BASE_REF:-}" ]]; then
  base_root="$tmp_root/base"
  mkdir -p "$base_root"
  git -C "$repo_root" archive "$KAGENT_HELM_BASE_REF" helm/kagent | tar -x -C "$base_root"
  base_chart="$tmp_root/base-kagent"
  prepare_chart "$base_root/helm/kagent" "$base_chart"
  base_render="$tmp_root/base-default.yaml"
  render_default "$base_chart" "$base_render"
  if ! cmp -s "$base_render" "$default_render"; then
    printf 'default kagent render differs from %s\n' "$KAGENT_HELM_BASE_REF" >&2
    diff -u "$base_render" "$default_render" >&2 || true
    exit 1
  fi
fi

existing_args=(
  --set controller.substrate.enabled=true
  --set controller.substrate.ateApiEndpoint=dns:///api.ate-system.svc:443
  --set controller.substrate.tls.mode=existingSecret
  --set controller.substrate.tls.serverName=api.ate-system.svc
  --set controller.substrate.tls.existingSecret.name=kagent-ate-client-tls
  --set controller.substrate.tls.existingSecret.serverCAKey=ate-server-ca.pem
  --set controller.substrate.tls.existingSecret.clientCredentialBundleKey=kagent-client-bundle.pem
)

existing_render="$tmp_root/existing-secret.yaml"
helm template release "$current_chart" \
  --namespace kagent \
  --show-only templates/controller-deployment.yaml \
  "${existing_args[@]}" >"$existing_render"

if grep -Eq 'podCertificate|clusterTrustBundle|certificates\.k8s\.io' "$existing_render"; then
  printf 'existingSecret render contains a beta certificate API dependency\n' >&2
  exit 1
fi

for expected in \
  'fsGroup: 65532' \
  'secretName: "kagent-ate-client-tls"' \
  'defaultMode: 0440' \
  'key: "ate-server-ca.pem"' \
  'key: "kagent-client-bundle.pem"' \
  'name: SUBSTRATE_ATE_API_SERVER_NAME' \
  'value: "api.ate-system.svc"' \
  'value: /run/substrate-existing-tls/server-ca.pem' \
  'value: /run/substrate-existing-tls/client-credential-bundle.pem'; do
  if ! grep -Fq "$expected" "$existing_render"; then
    printf 'existingSecret render is missing: %s\n' "$expected" >&2
    exit 1
  fi
done

override_render="$tmp_root/existing-secret-fsgroup-override.yaml"
helm template release "$current_chart" \
  --namespace kagent \
  --show-only templates/controller-deployment.yaml \
  "${existing_args[@]}" \
  --set controller.podSecurityContext.fsGroup=2000 >"$override_render"
if ! grep -Fq 'fsGroup: 2000' "$override_render"; then
  printf 'existingSecret render did not preserve the explicit controller fsGroup\n' >&2
  exit 1
fi

schema_output=""
if schema_output="$(helm template release "$current_chart" \
  --namespace kagent \
  --show-only templates/controller-deployment.yaml \
  "${existing_args[@]}" \
  --set controller.substrate.tls.existingSecret.name=INVALID_NAME 2>&1)"; then
  printf 'expected the values schema to reject an invalid Secret name\n' >&2
  exit 1
fi
if ! grep -Fq "values don't meet the specifications" <<<"$schema_output"; then
  printf 'invalid Secret name bypassed values schema validation:\n%s\n' "$schema_output" >&2
  exit 1
fi

expect_template_failure() {
  local expected="$1"
  shift
  local output

  if output="$(helm template release "$current_chart" \
    --namespace kagent \
    --show-only templates/controller-deployment.yaml \
    --skip-schema-validation \
    "${existing_args[@]}" "$@" 2>&1)"; then
    printf 'expected Helm rendering to fail with: %s\n' "$expected" >&2
    exit 1
  fi
  if ! grep -Fq "$expected" <<<"$output"; then
    printf 'Helm failed without the expected validation message: %s\n%s\n' "$expected" "$output" >&2
    exit 1
  fi
}

expect_template_failure \
  'controller.substrate.tls.existingSecret.name must be a valid Kubernetes Secret name' \
  --set controller.substrate.tls.existingSecret.name=INVALID_NAME
expect_template_failure \
  'controller.substrate.tls.existingSecret.serverCAKey must be a valid Kubernetes Secret data key' \
  --set controller.substrate.tls.existingSecret.serverCAKey=invalid/key
expect_template_failure \
  'controller.substrate.tls.serverName must be an internal Kubernetes Service DNS name' \
  --set controller.substrate.tls.serverName=api.example.com
expect_template_failure \
  'serverCAKey and clientCredentialBundleKey must differ' \
  --set controller.substrate.tls.existingSecret.serverCAKey=shared.pem \
  --set controller.substrate.tls.existingSecret.clientCredentialBundleKey=shared.pem
expect_template_failure \
  'controller.substrate.ateApiEndpoint is required' \
  --set controller.substrate.ateApiEndpoint=
expect_template_failure \
  'substrate.enabled must be false when controller.substrate.tls.mode=existingSecret' \
  --set substrate.enabled=true

printf 'kagent existing-Secret TLS verification passed\n'
