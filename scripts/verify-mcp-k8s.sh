#!/usr/bin/env bash
set -euo pipefail

# Verify the hosted MCP gateway (BOS-49) in a namespace: the Deployment (gateway
# + cloudflared sidecar), the Service, the pinned image, and — the core guard —
# that bs-mcp-secret carries every REQUIRED_MCP_SECRET_KEYS entry with a real
# (non-placeholder) value, so the pod never crash-loops on a missing env. The
# live tunnel curls (OAuth metadata + no-token 401) sit behind the manual-checks
# gate because they require the Cloudflare Tunnel to be up (staging-first).
#
# Mirrors scripts/verify-bosso-k8s.sh; exercised offline by
# scripts/test-verify-mcp-k8s.sh with fake kubectl/curl on PATH.

ENVIRONMENT="${ENVIRONMENT:-production}"
case "${ENVIRONMENT}" in
  staging)
    NAMESPACE="${NAMESPACE:-bs-staging}"
    BASE_URL="${BASE_URL:-https://mcp-staging.bossanova.dev}"
    ;;
  production)
    NAMESPACE="${NAMESPACE:-bs-production}"
    BASE_URL="${BASE_URL:-https://mcp.bossanova.dev}"
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

# Every key the gateway pod (and its cloudflared sidecar) reads. Keep in sync
# with local.kubernetes_secret_mcp in infra/environments/main.tf and the
# .env-mcp.example overlays. A missing key here means a crash-looping pod.
REQUIRED_MCP_SECRET_KEYS=(
  MCP_GATEWAY_ADDR
  MCP_GATEWAY_PUBLIC_URL
  MCP_WORKOS_API_KEY
  MCP_WORKOS_CLIENT_ID
  MCP_ORCHESTRATOR_URL
  CLOUDFLARE_TUNNEL_TOKEN
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
kubectl get deployment/bs-mcp-gateway -n "${NAMESPACE}"
kubectl get service/bs-mcp-gateway-service -n "${NAMESPACE}"

echo "Checking deployed bs-mcp-gateway image"
DEPLOYED_IMAGE_REF="$(
  kubectl get deployment/bs-mcp-gateway -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="mcp-gateway")].image}'
)"
if [ "${DEPLOYED_IMAGE_REF}" != "${EXPECTED_IMAGE_REF}" ]; then
  echo "deployed image mismatch: expected ${EXPECTED_IMAGE_REF}, got ${DEPLOYED_IMAGE_REF}" >&2
  exit 1
fi

echo "Checking cloudflared sidecar is present"
SIDECAR_IMAGE="$(
  kubectl get deployment/bs-mcp-gateway -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="cloudflared")].image}'
)"
if [ -z "${SIDECAR_IMAGE}" ]; then
  echo "cloudflared sidecar missing from bs-mcp-gateway pod" >&2
  exit 1
fi

echo "Checking bs-mcp-secret required keys"
for key in "${REQUIRED_MCP_SECRET_KEYS[@]}"; do
  encoded_value="$(kubectl get secret bs-mcp-secret -n "${NAMESPACE}" -o "jsonpath={.data.${key}}")"
  if [ -z "${encoded_value}" ]; then
    echo "missing required key in bs-mcp-secret: ${key}" >&2
    exit 1
  fi
  if ! decoded_value="$(decode_base64 "${encoded_value}")"; then
    echo "invalid base64 value in bs-mcp-secret: ${key}" >&2
    exit 1
  fi
  for sentinel in "${PLACEHOLDER_SENTINELS[@]}"; do
    if [[ "${decoded_value}" == *"${sentinel}"* ]]; then
      echo "placeholder value in bs-mcp-secret: ${key} contains ${sentinel}" >&2
      exit 1
    fi
  done
done

echo "Checking rollout status"
kubectl rollout status -n "${NAMESPACE}" deployment/bs-mcp-gateway --timeout=5m

echo "Waiting for pod readiness"
kubectl wait -n "${NAMESPACE}" --for=condition=Ready pod -l app=bs-mcp-gateway --timeout=120s

cat <<CHECKS

Manual required checks for ${ENVIRONMENT} (need the live Cloudflare Tunnel):
- curl -i ${BASE_URL}/.well-known/oauth-protected-resource returns the metadata JSON.
- A no-token request to ${BASE_URL}/mcp returns 401 with a WWW-Authenticate header.
- Run a tools/call over ${BASE_URL}/mcp and confirm the SSE stream stays open past
  one keepalive interval (gateway ~25s must beat Cloudflare's ~100s idle timeout).
CHECKS

if [ "${MANUAL_CHECKS_CONFIRMED}" != "true" ]; then
  cat <<INSTRUCTIONS

Automated checks passed, but manual ${ENVIRONMENT} tunnel checks are not recorded.
Complete every manual required check above, then rerun with:

  ENVIRONMENT=${ENVIRONMENT} MANUAL_CHECKS_CONFIRMED=true scripts/verify-mcp-k8s.sh

Refusing success until manual checks are confirmed.
INSTRUCTIONS
  exit 42
fi

echo "Verifying public OAuth protected-resource metadata"
curl -fsS --connect-timeout "${CURL_CONNECT_TIMEOUT}" --max-time "${CURL_MAX_TIME}" \
  "${BASE_URL}/.well-known/oauth-protected-resource" >/dev/null
echo

echo "Verifying no-token /mcp is rejected (expect 401)"
status="$(curl -s -o /dev/null -w '%{http_code}' \
  --connect-timeout "${CURL_CONNECT_TIMEOUT}" --max-time "${CURL_MAX_TIME}" "${BASE_URL}/mcp")"
if [ "${status}" != "401" ]; then
  echo "no-token /mcp returned ${status}, expected 401" >&2
  exit 1
fi

echo "Manual ${ENVIRONMENT} checks confirmed."
