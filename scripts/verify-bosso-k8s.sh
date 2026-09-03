#!/usr/bin/env bash
set -euo pipefail

ENVIRONMENT="${ENVIRONMENT:-production}"
case "${ENVIRONMENT}" in
  staging)
    NAMESPACE="${NAMESPACE:-bs-staging}"
    BASE_URL="${BASE_URL:-https://orchestrator-staging.bossanova.dev}"
    ;;
  production)
    NAMESPACE="${NAMESPACE:-bs-production}"
    BASE_URL="${BASE_URL:-https://orchestrator.bossanova.dev}"
    ;;
  *)
    echo "ENVIRONMENT must be staging or production" >&2
    exit 2
    ;;
esac

CURL_CONNECT_TIMEOUT="${CURL_CONNECT_TIMEOUT:-5}"
CURL_MAX_TIME="${CURL_MAX_TIME:-15}"
EXPECTED_IMAGE_REF="${EXPECTED_IMAGE_REF:-${IMAGE_REF:-}}"
MANUAL_CHECKS_CONFIRMED="${MANUAL_CHECKS_CONFIRMED:-false}"

REQUIRED_BOSSO_SECRET_KEYS=(
  BOSSO_DB_DRIVER
  BOSSO_DATABASE_URL
  BOSSO_MULTI_INSTANCE
  BOSSO_REDIS_URL
  BOSSO_INTERNAL_ROUTING_TOKEN
  BOSSO_ATTACH_SIGNING_KEY
  BOSSO_WEBHOOK_ROUTING_URL
  BOSSO_ROUTING_PROVIDER
  BOSSO_WORKOS_API_KEY
  BOSSO_STRIPE_SECRET_KEY
  BOSSO_STRIPE_WEBHOOK_SECRET
  BOSSO_STRIPE_CLOUD_PRICE_ID
  BOSSO_GITHUB_APP_ID
  BOSSO_GITHUB_APP_SLUG
  BOSSO_GITHUB_APP_PRIVATE_KEY
  BOSSO_GITHUB_APP_WEBHOOK_SECRET
  BOSSO_GITHUB_APP_CALLBACK_URL
  BOSSO_GITHUB_APP_CLIENT_ID
  BOSSO_GITHUB_APP_CLIENT_SECRET
  BOSSO_SENTRY_DSN
  BOSSO_POSTHOG_PROJECT_TOKEN
  BOSSO_POSTHOG_HOST
)

PLACEHOLDER_SENTINELS=(
  REPLACE_ME
  PRIVATE_IP
)

kubectl_exec() {
  kubectl exec "$@"
}

decode_base64() {
  local encoded="$1"
  if printf '%s' "${encoded}" | base64 --decode 2>/dev/null; then
    return 0
  fi
  printf '%s' "${encoded}" | base64 -D 2>/dev/null
}

if [ -z "${EXPECTED_IMAGE_REF}" ]; then
  echo "EXPECTED_IMAGE_REF or IMAGE_REF is required for ${ENVIRONMENT} verification" >&2
  exit 2
fi

echo "Checking Kubernetes objects in namespace ${NAMESPACE}"
kubectl get namespace "${NAMESPACE}"
kubectl get statefulset/bs-bosso -n "${NAMESPACE}"
kubectl get deployment/bs-deploy-redis -n "${NAMESPACE}"
kubectl get service/bs-bosso-headless -n "${NAMESPACE}"
kubectl get service/bs-bosso-service -n "${NAMESPACE}"
kubectl get service/bs-redis-service -n "${NAMESPACE}"

echo "Checking deployed bs-bosso image"
DEPLOYED_IMAGE_REF="$(
  kubectl get statefulset/bs-bosso -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="bosso")].image}'
)"
if [ "${DEPLOYED_IMAGE_REF}" != "${EXPECTED_IMAGE_REF}" ]; then
  echo "deployed image mismatch: expected ${EXPECTED_IMAGE_REF}, got ${DEPLOYED_IMAGE_REF}" >&2
  exit 1
fi

echo "Checking bs-bosso-secret required keys"
for key in "${REQUIRED_BOSSO_SECRET_KEYS[@]}"; do
  encoded_value="$(kubectl get secret bs-bosso-secret -n "${NAMESPACE}" -o "jsonpath={.data.${key}}")"
  if [ -z "${encoded_value}" ]; then
    echo "missing required key in bs-bosso-secret: ${key}" >&2
    exit 1
  fi
  if ! decoded_value="$(decode_base64 "${encoded_value}")"; then
    echo "invalid base64 value in bs-bosso-secret: ${key}" >&2
    exit 1
  fi
  for sentinel in "${PLACEHOLDER_SENTINELS[@]}"; do
    if [[ "${decoded_value}" == *"${sentinel}"* ]]; then
      echo "placeholder value in bs-bosso-secret: ${key} contains ${sentinel}" >&2
      exit 1
    fi
  done
done

echo "Checking rollout status"
kubectl rollout status -n "${NAMESPACE}" statefulset/bs-bosso --timeout=5m
kubectl rollout status -n "${NAMESPACE}" deployment/bs-deploy-redis --timeout=5m

echo "Waiting for pod readiness"
kubectl wait -n "${NAMESPACE}" --for=condition=Ready pod -l app=bs-bosso --timeout=120s
kubectl wait -n "${NAMESPACE}" --for=condition=Ready pod -l app=bs-redis --timeout=120s

echo "Checking public canary health endpoint"
curl -fsS --connect-timeout "${CURL_CONNECT_TIMEOUT}" --max-time "${CURL_MAX_TIME}" "${BASE_URL}/healthz" >/dev/null
echo

echo "Checking streaming transport duplex (streamprobe)"
(cd services/bossd && go run ./cmd/streamprobe -url "${BASE_URL}")

# Deploy-ordering gate (BOS-241): the freshly-deployed server must support the
# API version this build's first-party clients request. apiversioncheck sends a
# side-effect-free, unauthenticated malformed-version probe and fails if the live
# server's supported-max trails the built client Current — so production can
# never be released trailing the versions in circulation. See docs/api-versioning.md.
echo "Checking API version support (server must not trail built clients)"
(cd services/bossd && go run ./cmd/apiversioncheck -url "${BASE_URL}")

echo "Checking NEG endpoint attachment"
if gcloud compute network-endpoint-groups describe "bs-bosso-neg-${ENVIRONMENT}" \
  --zone europe-west1-b --project madverts-production \
  --format="value(size)" 2>/dev/null | grep -qv '^0$'; then
  echo "NEG has endpoints"
else
  echo "WARN: NEG bs-bosso-neg-${ENVIRONMENT} missing or empty (expected until GCP LB is enabled)"
fi

echo "Checking internal headless DNS"
kubectl_exec -n "${NAMESPACE}" statefulset/bs-bosso -- \
  getent hosts "bs-bosso-0.bs-bosso-headless.${NAMESPACE}.svc.cluster.local"

echo "Checking Redis PING"
kubectl_exec -n "${NAMESPACE}" deployment/bs-deploy-redis -- redis-cli PING | grep -q PONG

cat <<CHECKS

Manual required checks for ${ENVIRONMENT}:
- Run bossd stream against ${BASE_URL} for 60s and confirm events continue without reconnect loops.
- Attach a web terminal through ${BASE_URL}/ws/attach and confirm interactive output, input, and terminal resize handling.
- Only with operator approval: temporarily scale bs-bosso to 3 replicas in ${NAMESPACE}, verify all replicas become Ready, repeat a stream-bound command, then restore and verify HPA-managed state.
CHECKS

if [ "${MANUAL_CHECKS_CONFIRMED}" != "true" ]; then
  cat <<INSTRUCTIONS

Automated checks passed, but manual ${ENVIRONMENT} checks are not recorded.
Complete every manual required check above, then rerun with:

  ENVIRONMENT=${ENVIRONMENT} MANUAL_CHECKS_CONFIRMED=true scripts/verify-bosso-k8s.sh

Refusing success until manual checks are confirmed.
INSTRUCTIONS
  exit 42
fi

echo "Manual ${ENVIRONMENT} checks confirmed."
