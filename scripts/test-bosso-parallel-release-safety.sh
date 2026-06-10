#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
K8S_IMAGE="europe-west1-docker.pkg.dev/madverts-operations/services/bosso"
K8S_REGISTRY="europe-west1-docker.pkg.dev"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_file() {
  local file="$1"
  [ -f "${ROOT_DIR}/${file}" ] || fail "missing ${file}"
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

check_fly_stays_sqlite() {
  require_file "services/bosso/fly.toml"
  require_file "services/bosso/Dockerfile"
  require_grep "services/bosso/fly.toml" 'BOSSO_DB_PATH = "/data/bosso.db"' "Fly must keep SQLite database path"
  require_absent "services/bosso/fly.toml" 'BOSSO_DB_DRIVER = "postgres"' "Fly must not force Postgres"
  require_absent "services/bosso/fly.toml" 'BOSSO_MULTI_INSTANCE = "true"' "Fly must not enable multi-instance routing"
  require_grep "services/bosso/Dockerfile" "FROM litestream/litestream:0.3 AS litestream" "Fly image must include Litestream"
  require_grep "services/bosso/Dockerfile" 'COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream' "Fly image must copy Litestream binary"
  require_grep "services/bosso/Dockerfile" 'COPY services/bosso/litestream.yml /etc/litestream.yml' "Fly image must include Litestream config"
  require_grep "services/bosso/Dockerfile" 'ENTRYPOINT ["litestream", "replicate", "-exec", "bosso"]' "Fly image must run bosso under Litestream"
}

check_k8s_has_separate_image() {
  require_file "services/bosso/Dockerfile.k8s"
  require_grep "services/bosso/Dockerfile.k8s" 'ENTRYPOINT ["bosso"]' "K8s image must run bosso directly"
  require_absent "services/bosso/Dockerfile.k8s" "litestream" "K8s image must not include Litestream"
  require_grep "services/bosso/kustomize/Makefile" "Dockerfile.k8s" "K8s Makefile must build from Dockerfile.k8s"
  require_grep "services/bosso/kustomize/base/statefulset-bosso.yml" "image: ${K8S_IMAGE}:production" "K8s default image must use Artifact Registry"
  require_grep "services/bosso/kustomize/Makefile" "IMAGE ?= ${K8S_IMAGE}" "K8s Makefile default image must use Artifact Registry"
  require_grep "services/bosso/kustomize/Makefile" "\$(KUSTOMIZE) edit set image ${K8S_IMAGE}=\$(IMAGE_REF)" "K8s Makefile must rewrite Artifact Registry image"
  require_absent "services/bosso/kustomize/base/statefulset-bosso.yml" "ghcr.io/recurser/bosso" "K8s base image must not use GHCR"
  require_absent "services/bosso/kustomize/Makefile" "ghcr.io/recurser/bosso" "K8s Makefile must not rewrite GHCR image"
}

check_release_workflows() {
  for file in .github/workflows/perform-staging-release.yml .github/workflows/perform-production-release.yml; do
    require_file "$file"
    require_grep "$file" "registry.fly.io" "release workflow must still push Fly image"
    require_grep "$file" "--config services/bosso/fly.toml" "release workflow must still deploy Fly"
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
  require_grep ".github/workflows/perform-staging-release.yml" "${K8S_IMAGE}:staging-\${{ github.sha }}" "staging K8s image must keep SHA tag"
  require_grep ".github/workflows/perform-staging-release.yml" "${K8S_IMAGE}:staging-latest" "staging K8s image must keep latest tag"
  require_grep ".github/workflows/perform-production-release.yml" "${K8S_IMAGE}:\${{ github.sha }}" "production K8s image must keep SHA tag"
  require_grep ".github/workflows/perform-production-release.yml" "${K8S_IMAGE}:production-latest" "production K8s image must keep latest tag"
}

check_kustomize_staging_and_production() {
  require_file "services/bosso/kustomize/overlays/staging/kustomization.yml"
  require_file "services/bosso/kustomize/overlays/staging/namespace.yml"
  require_file "services/bosso/kustomize/overlays/production/namespace.yml"
  require_grep "services/bosso/kustomize/overlays/staging/namespace.yml" "name: bs-staging" "staging namespace must be bs-staging"
  require_grep "services/bosso/kustomize/overlays/production/namespace.yml" "name: bs-production" "production namespace must be bs-production"
  require_absent "services/bosso/kustomize/base/kustomization.yml" "namespace.yml" "base must not include environment namespace"
  require_absent "services/bosso/kustomize/base/kustomization.yml" "namespace: bs-production" "base must not force production namespace"
}

check_dns_cutover_safety() {
  require_file "infra/modules/cf-dns/main.tf"
  require_file "infra/environments/variables.tf"
  require_grep_after "infra/environments/variables.tf" 'variable "bosso_api_tunnel_dns_enabled"' 'default     = false' "canonical tunnel DNS cutover flag must default false"
  require_grep "infra/modules/cf-dns/main.tf" 'proxied = var.api_cname_target != "" ? true : false' "canonical Fly DNS must stay DNS-only until tunnel cutover"
  require_grep "infra/modules/cf-dns/main.tf" 'resource "cloudflare_record" "api_k8s_canary"' "K8s canary DNS record must exist"
  require_grep "infra/environments/main.tf" '"orchestrator-k8s-staging"' "staging K8s canary hostname must be configured"
  require_grep "infra/environments/main.tf" '"orchestrator-k8s"' "production K8s canary hostname must be configured"
}

check_fly_stays_sqlite
check_k8s_has_separate_image
check_release_workflows
check_kustomize_staging_and_production
check_dns_cutover_safety

echo "PASS: bosso parallel release safety"
