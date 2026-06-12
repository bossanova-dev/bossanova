#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE_DIR="infra/kustomize"
SUITE="${1:-all}"
LEGACY_PROVIDER="f""ly"
LEGACY_INGRESS="cloud""flared"
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
  if [ -f "${ROOT_DIR}/${file}" ] && grep -Fq -- "$pattern" "${ROOT_DIR}/${file}"; then
    fail "${message}: ${file} still contains ${pattern}"
  fi
}

check_terraform_env() {
  require_file "infra/environments/variables.tf"
  require_file "infra/environments/main.tf"
  require_file "${KUSTOMIZE_DIR}/overlays/production/.env-bosso.example"
  require_file "docs/plans/2026-05-28-gke-orchestrator-migration.md"

  local variables=(
    "variable \"bosso_workos_api_key\""
    "variable \"bosso_stripe_secret_key\""
    "variable \"bosso_stripe_cloud_price_id\""
    "variable \"bosso_sentry_dsn\""
  )
  local terraform_lines=(
    "bosso_github_app_private_key_env = replace(var.bosso_github_app_private_key, \"\\n\", \"\\\\n\")"
    "BOSSO_WORKOS_API_KEY=\${var.bosso_workos_api_key}"
    "BOSSO_STRIPE_SECRET_KEY=\${var.bosso_stripe_secret_key}"
    "BOSSO_STRIPE_CLOUD_PRICE_ID=\${var.bosso_stripe_cloud_price_id}"
    "BOSSO_GITHUB_APP_ID=\${var.bosso_github_app_id}"
    "BOSSO_GITHUB_APP_SLUG=\${var.bosso_github_app_slug}"
    "BOSSO_GITHUB_APP_PRIVATE_KEY=\${local.bosso_github_app_private_key_env}"
    "BOSSO_GITHUB_APP_WEBHOOK_SECRET=\${var.bosso_github_app_webhook_secret}"
    "BOSSO_GITHUB_APP_CALLBACK_URL=\${var.bosso_github_app_callback_url}"
    "BOSSO_GITHUB_APP_CLIENT_ID=\${var.bosso_github_app_client_id}"
    "BOSSO_GITHUB_APP_CLIENT_SECRET=\${var.bosso_github_app_client_secret}"
    "BOSSO_SENTRY_DSN=\${var.bosso_sentry_dsn}"
  )

  for pattern in "${variables[@]}"; do
    require_grep "infra/environments/variables.tf" "$pattern" "missing Terraform variable"
  done
  for pattern in "${terraform_lines[@]}"; do
    require_grep "infra/environments/main.tf" "$pattern" "missing Terraform output key"
  done
  require_absent "infra/environments/main.tf" "BOSSO_GITHUB_APP_PRIVATE_KEY=\${var.bosso_github_app_private_key}" "Terraform output must escape GitHub App private key newlines"

  require_grep "docs/plans/2026-05-28-gke-orchestrator-migration.md" "terraform output -raw kubernetes_secret_bosso" "docs must show complete bosso secret output command"
  require_grep "docs/plans/2026-05-28-gke-orchestrator-migration.md" "BOSSO_WORKOS_API_KEY" "docs must list required Bosso production keys"
}

check_dockerignore() {
  require_file ".dockerignore"
  local patterns=(
    "**/.env"
    "**/.env.*"
    "!**/.env.example"
    "!**/.env.*.example"
    "**/overlays/*/.env-*"
    "!**/overlays/*/.env-*.example"
    "**/*.tfvars"
    "!**/*.tfvars.example"
    "**/*.tfstate"
    "**/*.tfstate.*"
    "**/terraform.tfvars"
    "**/*private-key*"
    "**/*.pem"
    "**/*.key"
    "**/*.p12"
    "**/*.crt"
  )
  for pattern in "${patterns[@]}"; do
    require_grep ".dockerignore" "$pattern" "Docker context can include secret/state files"
  done
}

check_smoke_script() {
  require_file "scripts/verify-bosso-production.sh"
  require_file "scripts/verify-bosso-staging.sh"
  require_file "scripts/verify-bosso-k8s.sh"
  require_grep "scripts/verify-bosso-production.sh" 'ENVIRONMENT=production "${SCRIPT_DIR}/verify-bosso-k8s.sh"' "production smoke wrapper must call generic verifier"
  require_grep "scripts/verify-bosso-staging.sh" 'ENVIRONMENT=staging "${SCRIPT_DIR}/verify-bosso-k8s.sh"' "staging smoke wrapper must call generic verifier"
  require_grep "scripts/verify-bosso-k8s.sh" 'EXPECTED_IMAGE_REF="${EXPECTED_IMAGE_REF:-${IMAGE_REF:-}}"' "smoke script must accept expected image input"
  require_grep "scripts/verify-bosso-k8s.sh" "Checking deployed bs-bosso image" "smoke script must check deployed image"
  require_grep "scripts/verify-bosso-k8s.sh" 'containers[?(@.name=="bosso")].image' "smoke script must read StatefulSet bosso container image"
  require_grep "scripts/verify-bosso-k8s.sh" "get secret bs-bosso-secret" "smoke script must inspect bs-bosso-secret"
  require_grep "scripts/verify-bosso-k8s.sh" "decode_base64()" "smoke script must decode required secret values"
  require_grep "scripts/verify-bosso-k8s.sh" "PLACEHOLDER_SENTINELS=" "smoke script must define placeholder sentinels"
  require_grep "scripts/verify-bosso-k8s.sh" "REPLACE_ME" "smoke script must reject REPLACE_ME placeholder values"
  require_grep "scripts/verify-bosso-k8s.sh" "PRIVATE_IP" "smoke script must reject PRIVATE_IP placeholder values"
  require_grep "scripts/verify-bosso-k8s.sh" "placeholder value in bs-bosso-secret" "smoke script must fail closed on placeholder secret values"
  require_grep "scripts/verify-bosso-k8s.sh" "https://orchestrator-k8s-staging.bossanova.dev" "smoke script must include staging canary URL"
  require_grep "scripts/verify-bosso-k8s.sh" "https://orchestrator-k8s.bossanova.dev" "smoke script must include production canary URL"

  local required_keys=(
    "BOSSO_DB_DRIVER"
    "BOSSO_DATABASE_URL"
    "BOSSO_MULTI_INSTANCE"
    "BOSSO_REDIS_URL"
    "BOSSO_INTERNAL_WEBHOOK_TOKEN"
    "BOSSO_ATTACH_SIGNING_KEY"
    "BOSSO_WEBHOOK_ROUTING_URL"
    "BOSSO_ROUTING_PROVIDER"
    "BOSSO_WORKOS_API_KEY"
    "BOSSO_STRIPE_SECRET_KEY"
    "BOSSO_STRIPE_CLOUD_PRICE_ID"
    "BOSSO_GITHUB_APP_ID"
    "BOSSO_GITHUB_APP_SLUG"
    "BOSSO_GITHUB_APP_PRIVATE_KEY"
    "BOSSO_GITHUB_APP_WEBHOOK_SECRET"
    "BOSSO_GITHUB_APP_CALLBACK_URL"
    "BOSSO_GITHUB_APP_CLIENT_ID"
    "BOSSO_GITHUB_APP_CLIENT_SECRET"
    "BOSSO_SENTRY_DSN"
  )
  for key in "${required_keys[@]}"; do
    require_grep "scripts/verify-bosso-k8s.sh" "$key" "smoke script does not require secret key"
  done

  require_grep "scripts/verify-bosso-k8s.sh" 'MANUAL_CHECKS_CONFIRMED' "smoke script must gate success on confirmed manual checks"
}

check_k8s_hardening() {
  require_file "${KUSTOMIZE_DIR}/base/statefulset-bosso.yml"
  require_file "${KUSTOMIZE_DIR}/base/deployment-redis.yml"

  local workload_files=(
    "${KUSTOMIZE_DIR}/base/statefulset-bosso.yml"
    "${KUSTOMIZE_DIR}/base/deployment-redis.yml"
  )
  for file in "${workload_files[@]}"; do
    require_grep "$file" "runAsNonRoot: true" "workload must run as non-root"
    require_grep "$file" "seccompProfile:" "workload must use seccomp"
    require_grep "$file" "type: RuntimeDefault" "workload must use RuntimeDefault seccomp"
    require_grep "$file" "allowPrivilegeEscalation: false" "container must block privilege escalation"
    require_grep "$file" "drop:" "container must drop Linux capabilities"
    require_grep "$file" "- ALL" "container must drop all Linux capabilities"
  done

  require_grep "${KUSTOMIZE_DIR}/base/statefulset-bosso.yml" "runAsUser: 65532" "Bosso workload must set numeric non-root user"
  require_grep "${KUSTOMIZE_DIR}/base/statefulset-bosso.yml" "runAsGroup: 65532" "Bosso workload must set numeric non-root group"
  require_grep "${KUSTOMIZE_DIR}/base/deployment-redis.yml" "redis:7.4-alpine@sha256:6ab0b6e7381779332f97b8ca76193e45b0756f38d4c0dcda72dbb3c32061ab99" "Redis image must be digest-pinned"
}

check_retired_ingress_removed() {
  require_file "${KUSTOMIZE_DIR}/base/kustomization.yml"
  require_absent_file "${KUSTOMIZE_DIR}/base/deployment-${LEGACY_INGRESS}.yml"
  require_absent_file "${KUSTOMIZE_DIR}/overlays/production/.env-${LEGACY_INGRESS}.example"
  require_absent_file "${KUSTOMIZE_DIR}/overlays/staging/.env-${LEGACY_INGRESS}.example"
  require_absent "${KUSTOMIZE_DIR}/base/kustomization.yml" "${LEGACY_INGRESS}" "retired ingress resource must not render"
  require_absent "${KUSTOMIZE_DIR}/overlays/production/kustomization.yml" "bs-${LEGACY_INGRESS}-secret" "production overlay must not generate retired ingress secret"
  require_absent "${KUSTOMIZE_DIR}/overlays/staging/kustomization.yml" "bs-${LEGACY_INGRESS}-secret" "staging overlay must not generate retired ingress secret"
}

check_gcp_only_release() {
  require_file "services/bosso/Dockerfile.k8s"
  require_file "scripts/test-bosso-parallel-release-safety.sh"

  require_absent_file "services/bosso/Dockerfile"
  require_absent_file "services/bosso/${LEGACY_PROVIDER}.toml"
  require_absent_file "services/bosso/${LEGACY_BACKUP}.yml"
  require_grep "services/bosso/Dockerfile.k8s" 'ENTRYPOINT ["bosso"]' "K8s Dockerfile must run bosso directly"
  require_absent ".github/workflows/perform-production-release.yml" "${LEGACY_PROVIDER}ctl" "production release must not deploy with legacy CLI"
  require_absent ".github/workflows/perform-staging-release.yml" "${LEGACY_PROVIDER}ctl" "staging release must not deploy with legacy CLI"
}

run_suite() {
  case "$1" in
    terraform-env) check_terraform_env ;;
    dockerignore) check_dockerignore ;;
    smoke-script) check_smoke_script ;;
    k8s-hardening) check_k8s_hardening ;;
    retired-ingress) check_retired_ingress_removed ;;
    parallel-release) check_gcp_only_release ;;
    all)
      check_terraform_env
      check_dockerignore
      check_smoke_script
      check_k8s_hardening
      check_retired_ingress_removed
      check_gcp_only_release
      ;;
    *)
      echo "Usage: $0 [all|terraform-env|dockerignore|smoke-script|k8s-hardening|retired-ingress|parallel-release]" >&2
      exit 2
      ;;
  esac
}

run_suite "$SUITE"
echo "PASS: ${SUITE}"
