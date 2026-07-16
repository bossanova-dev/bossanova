#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE_DIR="infra/kustomize"
K8S_IMAGE="europe-west1-docker.pkg.dev/madverts-operations/services/bosso"
MCP_IMAGE="europe-west1-docker.pkg.dev/madverts-operations/services/mcp-gateway"
K8S_REGISTRY="europe-west1-docker.pkg.dev"
LEGACY_PROVIDER="f""ly"
LEGACY_PROVIDER_UPPER="$(printf '%s' "${LEGACY_PROVIDER}" | tr '[:lower:]' '[:upper:]')"
LEGACY_TOKEN="${LEGACY_PROVIDER_UPPER}_API_TOKEN"
LEGACY_BACKUP="lite""stream"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_file() {
  local file="$1"
  [ -f "${ROOT_DIR}/${file}" ] || fail "missing ${file}"
}

require_absent_file() {
  local file="$1"
  [ ! -e "${ROOT_DIR}/${file}" ] || fail "unexpected ${file}"
}

require_grep() {
  local file="$1"
  local pattern="$2"
  local message="$3"
  grep -Fq -- "$pattern" "${ROOT_DIR}/${file}" || fail "${message}: ${file} missing ${pattern}"
}

require_absent() {
  local file="$1"
  local pattern="$2"
  local message="$3"
  if grep -Fq -- "$pattern" "${ROOT_DIR}/${file}"; then
    fail "${message}: ${file} contains ${pattern}"
  fi
}

require_grep_after() {
  local file="$1"
  local anchor="$2"
  local pattern="$3"
  local message="$4"
  grep -F -A 8 -- "$anchor" "${ROOT_DIR}/${file}" | grep -Fq -- "$pattern" || fail "${message}: ${file} missing ${pattern} after ${anchor}"
}

require_no_kubectl_apply_in_workflow() {
  local file="$1"
  if grep -Eq 'kubectl[[:space:]].*(apply|delete|rollout|scale)|make[[:space:]].*(apply-|deploy-)' "${ROOT_DIR}/${file}"; then
    fail "release workflow must not mutate Kubernetes: ${file}"
  fi
}

check_k8s_has_separate_image() {
  require_file "services/bosso/Dockerfile.k8s"
  require_absent_file "services/bosso/Dockerfile"
  require_absent_file "services/bosso/${LEGACY_PROVIDER}.toml"
  require_absent_file "services/bosso/${LEGACY_BACKUP}.yml"
  require_grep "services/bosso/Dockerfile.k8s" 'ENTRYPOINT ["bosso"]' "K8s image must run bosso directly"
  require_absent "services/bosso/Dockerfile.k8s" "${LEGACY_BACKUP}" "K8s image must not include retired backup agent"
  require_grep "${KUSTOMIZE_DIR}/base/statefulset-bosso.yml" "image: ${K8S_IMAGE}:production" "K8s default image must use Artifact Registry"
  require_absent "${KUSTOMIZE_DIR}/base/statefulset-bosso.yml" "ghcr.io/recurser/bosso" "K8s base image must not use GHCR"
  require_absent "${KUSTOMIZE_DIR}/Makefile" "ghcr.io/recurser/bosso" "K8s Makefile must not rewrite GHCR image"
  # GitOps: CI builds and pushes the image, then pins the tag into the overlay
  # on the env branch. The Makefile must NOT build images at deploy time, and
  # each overlay must carry a committed Artifact Registry pin.
  require_absent "${KUSTOMIZE_DIR}/Makefile" "docker build" "K8s Makefile must not build images at deploy time (CI builds them)"
  require_absent "${KUSTOMIZE_DIR}/Makefile" "Dockerfile.k8s" "K8s Makefile must not reference Dockerfile.k8s (CI builds the image)"
  for env in staging production; do
    require_grep "${KUSTOMIZE_DIR}/overlays/${env}/kustomization.yml" "name: ${K8S_IMAGE}" "K8s ${env} overlay must pin the Artifact Registry image"
    require_grep "${KUSTOMIZE_DIR}/overlays/${env}/kustomization.yml" "# bosso" "K8s ${env} overlay must tag the bosso pin for CI to rewrite"
  done
}

check_release_workflows() {
  for file in .github/workflows/perform-staging-release.yml .github/workflows/perform-production-release.yml; do
    require_file "$file"
    require_absent "$file" "registry.${LEGACY_PROVIDER}.io" "release workflow must not push legacy image"
    require_absent "$file" "${LEGACY_PROVIDER}ctl" "release workflow must not deploy with legacy CLI"
    require_absent "$file" "${LEGACY_TOKEN}" "release workflow must not require legacy token"
    require_absent "$file" "super${LEGACY_PROVIDER}/${LEGACY_PROVIDER}ctl-actions" "release workflow must not install legacy CLI"
    require_grep "$file" "google-github-actions/auth@v3" "release workflow must authenticate to Google Cloud"
    require_grep "$file" "credentials_json: \${{ secrets.GOOGLE_CREDENTIALS }}" "release workflow must use GOOGLE_CREDENTIALS"
    require_grep "$file" "registry: ${K8S_REGISTRY}" "release workflow must log in to Artifact Registry"
    require_grep "$file" "username: oauth2accesstoken" "release workflow must use Google OAuth Docker username"
    require_grep "$file" "${K8S_IMAGE}" "release workflow must push K8s image to Artifact Registry"
    require_absent "$file" "ghcr.io/recurser/bosso" "release workflow K8s image must not use GHCR"
    require_absent "$file" "registry: ghcr.io" "release workflow must not log in to GHCR for K8s image"
    require_grep "$file" "services/bosso/Dockerfile.k8s" "release workflow must build K8s image from Dockerfile.k8s"
    require_no_kubectl_apply_in_workflow "$file"
  done
  require_grep ".github/workflows/perform-staging-release.yml" "type=sha,prefix=staging-" "staging K8s image must keep immutable SHA tag"
  require_absent ".github/workflows/perform-staging-release.yml" "staging-latest" "staging K8s image must not publish mutable latest tag"
  require_grep ".github/workflows/perform-production-release.yml" "type=sha,prefix=production-" "production K8s image must keep immutable SHA tag"
  require_absent ".github/workflows/perform-production-release.yml" "production-latest" "production K8s image must not publish mutable latest tag"
  # GitOps pin: each release must commit the rewritten overlay back to its env
  # branch, marked [skip ci] so the pin commit does not re-trigger the release.
  require_grep ".github/workflows/perform-staging-release.yml" "git push origin staging" "staging release must commit the image pin to the staging branch"
  require_grep ".github/workflows/perform-staging-release.yml" "[skip ci]" "staging pin commit must be marked [skip ci]"
  require_grep ".github/workflows/perform-production-release.yml" "git push origin production" "production release must commit the image pin to the production branch"
  require_grep ".github/workflows/perform-production-release.yml" "[skip ci]" "production pin commit must be marked [skip ci]"
}

check_kustomize_staging_and_production() {
  require_file "${KUSTOMIZE_DIR}/overlays/staging/kustomization.yml"
  require_file "${KUSTOMIZE_DIR}/overlays/staging/namespace.yml"
  require_file "${KUSTOMIZE_DIR}/overlays/production/namespace.yml"
  require_grep "${KUSTOMIZE_DIR}/overlays/staging/namespace.yml" "name: bs-staging" "staging namespace must be bs-staging"
  require_grep "${KUSTOMIZE_DIR}/overlays/production/namespace.yml" "name: bs-production" "production namespace must be bs-production"
  require_absent "${KUSTOMIZE_DIR}/base/kustomization.yml" "namespace.yml" "base must not include environment namespace"
  require_absent "${KUSTOMIZE_DIR}/base/kustomization.yml" "namespace: bs-production" "base must not force production namespace"
}

check_dns_cutover_safety() {
  require_file "infra/modules/cf-dns/main.tf"
  require_file "infra/environments/variables.tf"
  require_absent "infra/environments/variables.tf" 'bosso_api_tunnel_dns_enabled' "tunnel DNS cutover flag must be removed"
  require_absent "infra/modules/cf-dns/variables.tf" "${LEGACY_PROVIDER}_hostname" "legacy DNS fallback variable must be removed"
  require_grep_after "infra/environments/variables.tf" 'variable "bosso_gcp_lb_enabled"' 'default     = false' "GCP LB creation flag must default false"
  require_grep_after "infra/environments/variables.tf" 'variable "bosso_api_gcp_lb_dns_enabled"' 'default     = false' "canonical GCP LB DNS cutover flag must default false"
  require_absent "infra/modules/cf-dns/main.tf" 'resource "cloudflare_record" "api_k8s_canary"' "retired ingress canary CNAME must be removed"
  require_grep "infra/modules/cf-dns/main.tf" 'resource "cloudflare_record" "api_gcp_lb"' "canonical GCP LB A record resource must exist"
  require_grep "infra/modules/cf-dns/main.tf" 'resource "cloudflare_record" "api_k8s_canary_gcp_lb"' "K8s canary GCP LB A record resource must exist"
  require_grep "infra/environments/main.tf" '"orchestrator-k8s-staging"' "staging K8s canary hostname must be configured"
  require_grep "infra/environments/main.tf" '"orchestrator-k8s"' "production K8s canary hostname must be configured"
}

# BOS-49: hosted MCP gateway deploy safety. The gateway ships in the shared
# kustomize base (gateway + cloudflared sidecar), each overlay pins its own
# Artifact Registry image with a CI-rewritable marker, the tunnel token is
# sourced from the secret (never a literal), and the cf-tunnel Terraform module
# is gated off by default so staging enables before production.
check_mcp_gateway() {
  require_file "${KUSTOMIZE_DIR}/base/deployment-mcp-gateway.yml"
  require_file "${KUSTOMIZE_DIR}/base/service-mcp-gateway.yml"
  require_file "${KUSTOMIZE_DIR}/base/configmap-mcp-gateway.yml"
  require_grep "${KUSTOMIZE_DIR}/base/kustomization.yml" "deployment-mcp-gateway.yml" "base must include the MCP gateway Deployment"
  require_grep "${KUSTOMIZE_DIR}/base/deployment-mcp-gateway.yml" "image: ${MCP_IMAGE}:production" "MCP gateway default image must use Artifact Registry"
  require_grep "${KUSTOMIZE_DIR}/base/deployment-mcp-gateway.yml" "name: cloudflared" "MCP gateway pod must run the cloudflared sidecar"
  require_grep "${KUSTOMIZE_DIR}/base/deployment-mcp-gateway.yml" "name: mcp-gateway" "MCP gateway pod must run the gateway container"
  # The tunnel token must come from the secret env, never a literal in kustomize.
  require_grep "${KUSTOMIZE_DIR}/base/deployment-mcp-gateway.yml" "key: CLOUDFLARE_TUNNEL_TOKEN" "cloudflared token must come from the secret, not a literal"
  require_absent "${KUSTOMIZE_DIR}/base/deployment-mcp-gateway.yml" "TUNNEL_TOKEN: ey" "cloudflared token must not be inlined"
  for env in staging production; do
    require_grep "${KUSTOMIZE_DIR}/overlays/${env}/kustomization.yml" "name: ${MCP_IMAGE}" "K8s ${env} overlay must pin the MCP gateway image"
    require_grep "${KUSTOMIZE_DIR}/overlays/${env}/kustomization.yml" "# mcp-gateway" "K8s ${env} overlay must tag the mcp-gateway pin for CI to rewrite"
    require_grep "${KUSTOMIZE_DIR}/overlays/${env}/kustomization.yml" "name: bs-mcp-secret" "K8s ${env} overlay must generate the bs-mcp-secret"
    require_file "${KUSTOMIZE_DIR}/overlays/${env}/.env-mcp.example"
    require_absent "${KUSTOMIZE_DIR}/overlays/${env}/.env-mcp.example" "cfargotunnel.com" "overlay .env-mcp.example must not carry a real tunnel token"
  done
  # cf-tunnel Terraform module: present, gated off by default, token sourced
  # from state and never a literal.
  require_file "infra/modules/cf-tunnel/main.tf"
  require_grep_after "infra/modules/cf-tunnel/variables.tf" 'variable "enabled"' 'default     = false' "cf-tunnel module must default enabled=false"
  require_grep "infra/environments/main.tf" 'module "cf_tunnel"' "environments must wire the cf-tunnel module"
  require_grep_after "infra/environments/variables.tf" 'variable "mcp_gateway_enabled"' 'default     = false' "MCP gateway enable flag must default false"
  require_grep "infra/environments/main.tf" "CLOUDFLARE_TUNNEL_TOKEN=\${module.cf_tunnel.tunnel_token}" "MCP secret must source the tunnel token from the module output"
  # GitOps release parity with bosso: both release workflows build+push the
  # gateway image from its own Dockerfile.k8s and pin the # mcp-gateway marker
  # back into the overlay on the env branch.
  require_file "services/mcp-gateway/Dockerfile.k8s"
  require_absent_file "services/mcp-gateway/Dockerfile"
  require_grep "services/mcp-gateway/Dockerfile.k8s" 'ENTRYPOINT ["mcp-gateway"]' "MCP gateway K8s image must run mcp-gateway directly"
  for file in .github/workflows/perform-staging-release.yml .github/workflows/perform-production-release.yml; do
    require_grep "$file" "services/mcp-gateway/Dockerfile.k8s" "release workflow must build the gateway image from Dockerfile.k8s"
    require_grep "$file" "${MCP_IMAGE}" "release workflow must push the gateway image to Artifact Registry"
    require_grep "$file" "newTag: '\${{ needs.version.outputs.version }}'  # mcp-gateway" "release workflow must pin the mcp-gateway overlay tag"
  done
}

check_k8s_has_separate_image
check_release_workflows
check_kustomize_staging_and_production
check_dns_cutover_safety
check_mcp_gateway

echo "PASS: bosso parallel release safety"
