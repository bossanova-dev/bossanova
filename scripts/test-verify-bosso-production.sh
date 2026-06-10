#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
KUBECTL_LOG="${TMP_DIR}/kubectl.log"
CURL_LOG="${TMP_DIR}/curl.log"
SECRET_KEY_LOG="${TMP_DIR}/secret-keys.log"
export KUBECTL_LOG
export CURL_LOG
export SECRET_KEY_LOG

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cat >"${TMP_DIR}/kubectl" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail

echo "kubectl $*" >>"${KUBECTL_LOG}"

if [[ "$*" == *"get namespace bs-production"* || "$*" == *"get namespace bs-staging"* ]]; then
  echo "namespace"
  exit 0
fi

if [[ "$*" == *"get deployment/bs-deploy-cloudflared"* ]]; then
  echo "deployment.apps/bs-deploy-cloudflared"
  exit 0
fi

if [[ "$*" == *"get deployment/bs-deploy-redis"* ]]; then
  echo "deployment.apps/bs-deploy-redis"
  exit 0
fi

if [[ "$*" == *"get service/bs-bosso-headless"* ]]; then
  echo "service/bs-bosso-headless"
  exit 0
fi

if [[ "$*" == *"get service/bs-bosso-service"* ]]; then
  echo "service/bs-bosso-service"
  exit 0
fi

if [[ "$*" == *"get service/bs-redis-service"* ]]; then
  echo "service/bs-redis-service"
  exit 0
fi

if [[ "$*" == *"rollout status"*"statefulset/bs-bosso"* ]]; then
  echo "statefulset rolling"
  exit 0
fi

if [[ "$*" == *"rollout status"*"deployment/bs-deploy-cloudflared"* ]]; then
  echo "deployment rolling"
  exit 0
fi

if [[ "$*" == *"rollout status"*"deployment/bs-deploy-redis"* ]]; then
  echo "deployment rolling"
  exit 0
fi

if [[ "$*" == *"get statefulset/bs-bosso"* && "$*" == *"containers[?(@.name==\"bosso\")].image"* ]]; then
  echo "${DEPLOYED_IMAGE_REF}"
  exit 0
fi

if [[ "$*" == *"get statefulset/bs-bosso"* ]]; then
  echo "statefulset.apps/bs-bosso"
  exit 0
fi

if [[ "$*" == *"get secret bs-bosso-secret"* ]]; then
  if [[ "$*" =~ jsonpath=\{\.data\.([A-Za-z0-9_]+)\} ]]; then
    key="${BASH_REMATCH[1]}"
    if [ -n "${MISSING_SECRET_KEY:-}" ] && [ "${key}" = "${MISSING_SECRET_KEY}" ]; then
      # Simulate a key that is absent/empty in the secret: jsonpath yields no
      # output. The smoke script must treat this as a missing required key.
      exit 0
    fi
    if [ -n "${PLACEHOLDER_SECRET_KEY:-}" ] && [ "${key}" = "${PLACEHOLDER_SECRET_KEY}" ]; then
      echo "${key}" >>"${SECRET_KEY_LOG}"
      echo "UkVQTEFDRV9NRQ=="
      exit 0
    fi
    case "${key}" in
      BOSSO_DB_DRIVER|BOSSO_DATABASE_URL|BOSSO_MULTI_INSTANCE|BOSSO_REDIS_URL|BOSSO_INTERNAL_WEBHOOK_TOKEN|BOSSO_ATTACH_SIGNING_KEY|BOSSO_WEBHOOK_ROUTING_URL|BOSSO_ROUTING_PROVIDER|BOSSO_WORKOS_API_KEY|BOSSO_STRIPE_SECRET_KEY|BOSSO_STRIPE_CLOUD_PRICE_ID|BOSSO_GITHUB_APP_ID|BOSSO_GITHUB_APP_SLUG|BOSSO_GITHUB_APP_PRIVATE_KEY|BOSSO_GITHUB_APP_WEBHOOK_SECRET|BOSSO_GITHUB_APP_CALLBACK_URL|BOSSO_GITHUB_APP_CLIENT_ID|BOSSO_GITHUB_APP_CLIENT_SECRET|BOSSO_SENTRY_DSN)
        echo "${key}" >>"${SECRET_KEY_LOG}"
        echo "eA=="
        exit 0
        ;;
      *)
        exit 1
        ;;
    esac
  fi
  echo "unexpected secret jsonpath: $*" >&2
  exit 99
fi

if [[ "$*" == *"get pods"* ]]; then
  echo "bs-bosso-0"
  exit 0
fi

if [[ "$*" == *"wait"*"app=bs-bosso"* ]]; then
  echo "pod/bs-bosso-0 condition met"
  exit 0
fi

if [[ "$*" == *"wait"*"app=bs-cloudflared"* ]]; then
  echo "pod/bs-cloudflared-0 condition met"
  exit 0
fi

if [[ "$*" == *"wait"*"app=bs-redis"* ]]; then
  echo "pod/bs-redis-0 condition met"
  exit 0
fi

if [[ "$*" == *"logs"* ]]; then
  echo "bosso ready"
  exit 0
fi

if [[ "$*" == *"getent hosts bs-bosso-0.bs-bosso-headless.bs-production.svc.cluster.local"* || "$*" == *"getent hosts bs-bosso-0.bs-bosso-headless.bs-staging.svc.cluster.local"* ]]; then
  echo "10.0.0.10 bs-bosso-0"
  exit 0
fi

if [[ "$*" == *"redis-cli PING"* ]]; then
  echo "PONG"
  exit 0
fi

echo "unexpected kubectl invocation: $*" >&2
exit 99
FAKE_KUBECTL
chmod +x "${TMP_DIR}/kubectl"

cat >"${TMP_DIR}/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
echo "curl $*" >>"${CURL_LOG}"
echo "ok"
FAKE_CURL
chmod +x "${TMP_DIR}/curl"

export PATH="${TMP_DIR}:${PATH}"
export DEPLOYED_IMAGE_REF="us-central1-docker.pkg.dev/example/bs/bosso:expected"
export EXPECTED_IMAGE_REF="${DEPLOYED_IMAGE_REF}"
export MANUAL_CHECKS_CONFIRMED=true

OUTPUT="$("${ROOT_DIR}/scripts/verify-bosso-production.sh" 2>&1)" || fail "verify-bosso-production failed unexpectedly: ${OUTPUT}"

grep -Fq "statefulset/bs-bosso" "${KUBECTL_LOG}" || fail "kubectl log missing statefulset/bs-bosso"
grep -Fq "secret bs-bosso-secret" "${KUBECTL_LOG}" || fail "kubectl log missing secret bs-bosso-secret"
grep -Fq "https://orchestrator-k8s.bossanova.dev/healthz" "${CURL_LOG}" \
  || fail "curl log missing production canary health URL"

export DEPLOYED_IMAGE_REF="europe-west1-docker.pkg.dev/madverts-operations/services/bosso:staging-test"
export EXPECTED_IMAGE_REF="${DEPLOYED_IMAGE_REF}"
export MANUAL_CHECKS_CONFIRMED=true
OUTPUT="$(ENVIRONMENT=staging NAMESPACE=bs-staging "${ROOT_DIR}/scripts/verify-bosso-k8s.sh" 2>&1)" \
  || fail "verify-bosso-k8s staging failed unexpectedly: ${OUTPUT}"
grep -Fq "https://orchestrator-k8s-staging.bossanova.dev/healthz" "${CURL_LOG}" \
  || fail "curl log missing staging canary health URL"

REQUIRED_BOSSO_SECRET_KEYS=(
  BOSSO_DB_DRIVER
  BOSSO_DATABASE_URL
  BOSSO_MULTI_INSTANCE
  BOSSO_REDIS_URL
  BOSSO_INTERNAL_WEBHOOK_TOKEN
  BOSSO_ATTACH_SIGNING_KEY
  BOSSO_WEBHOOK_ROUTING_URL
  BOSSO_ROUTING_PROVIDER
  BOSSO_WORKOS_API_KEY
  BOSSO_STRIPE_SECRET_KEY
  BOSSO_STRIPE_CLOUD_PRICE_ID
  BOSSO_GITHUB_APP_ID
  BOSSO_GITHUB_APP_SLUG
  BOSSO_GITHUB_APP_PRIVATE_KEY
  BOSSO_GITHUB_APP_WEBHOOK_SECRET
  BOSSO_GITHUB_APP_CALLBACK_URL
  BOSSO_GITHUB_APP_CLIENT_ID
  BOSSO_GITHUB_APP_CLIENT_SECRET
  BOSSO_SENTRY_DSN
)
for key in "${REQUIRED_BOSSO_SECRET_KEYS[@]}"; do
  grep -Fxq "${key}" "${SECRET_KEY_LOG}" || fail "secret key was not requested: ${key}"
done

export DEPLOYED_IMAGE_REF="us-central1-docker.pkg.dev/example/bs/bosso:expected"
export EXPECTED_IMAGE_REF="${DEPLOYED_IMAGE_REF}"
unset MANUAL_CHECKS_CONFIRMED
set +e
OUTPUT="$("${ROOT_DIR}/scripts/verify-bosso-production.sh" 2>&1)"
status=$?
set -e
if [ "${status}" -ne 42 ]; then
  fail "verify-bosso-production manual gate returned ${status}, expected 42: ${OUTPUT}"
fi
grep -Fq "manual production checks are not recorded" <<<"${OUTPUT}" \
  || fail "manual gate failure did not explain missing confirmation: ${OUTPUT}"
export MANUAL_CHECKS_CONFIRMED=true

export DEPLOYED_IMAGE_REF="us-central1-docker.pkg.dev/example/bs/bosso:stale"
export EXPECTED_IMAGE_REF="us-central1-docker.pkg.dev/example/bs/bosso:expected"
if OUTPUT="$("${ROOT_DIR}/scripts/verify-bosso-production.sh" 2>&1)"; then
  fail "verify-bosso-production passed with stale image"
fi
grep -Fq "deployed image mismatch" <<<"${OUTPUT}" || fail "stale image failure missing deployed image mismatch: ${OUTPUT}"

# A required secret key that is absent/empty must fail closed and name the key.
export DEPLOYED_IMAGE_REF="us-central1-docker.pkg.dev/example/bs/bosso:expected"
export EXPECTED_IMAGE_REF="${DEPLOYED_IMAGE_REF}"
export MISSING_SECRET_KEY="BOSSO_WORKOS_API_KEY"
if OUTPUT="$("${ROOT_DIR}/scripts/verify-bosso-production.sh" 2>&1)"; then
  fail "verify-bosso-production passed with a missing required secret key"
fi
grep -Fq "missing required key in bs-bosso-secret: BOSSO_WORKOS_API_KEY" <<<"${OUTPUT}" \
  || fail "missing-secret-key failure did not name the key: ${OUTPUT}"
unset MISSING_SECRET_KEY

# A required secret key that still contains a placeholder/sentinel value must
# fail closed and name the key plus sentinel.
export PLACEHOLDER_SECRET_KEY="BOSSO_DATABASE_URL"
if OUTPUT="$("${ROOT_DIR}/scripts/verify-bosso-production.sh" 2>&1)"; then
  fail "verify-bosso-production passed with a placeholder required secret key"
fi
grep -Fq "placeholder value in bs-bosso-secret: BOSSO_DATABASE_URL contains REPLACE_ME" <<<"${OUTPUT}" \
  || fail "placeholder-secret-key failure did not name the key and sentinel: ${OUTPUT}"
unset PLACEHOLDER_SECRET_KEY

echo "PASS: verify-bosso-production fake integration"
