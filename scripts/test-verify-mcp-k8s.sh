#!/usr/bin/env bash
set -euo pipefail

# Offline harness for scripts/verify-mcp-k8s.sh (BOS-49). Puts fake kubectl/curl
# on PATH so the verifier's secret-key enumeration can be exercised without a
# cluster: it must PASS on the full REQUIRED_MCP_SECRET_KEYS set and FAIL when a
# single key is removed. Mirrors scripts/test-verify-bosso-production.sh.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
export KUBECTL_LOG="${TMP_DIR}/kubectl.log"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

IMAGE_REF='europe-west1-docker.pkg.dev/madverts-operations/services/mcp-gateway:0.1.0'
SIDECAR_IMAGE_REF='cloudflare/cloudflared:2024.10.0'

# Fake kubectl. Serves the gateway objects, the two container images, and
# bs-mcp-secret keys from REQUIRED_MCP_SECRET_KEYS, honoring REMOVED_KEY so a
# test can simulate a dropped key. Non-placeholder base64 values throughout.
cat >"${TMP_DIR}/kubectl" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail

echo "kubectl $*" >>"${KUBECTL_LOG}"

if [[ "$*" == *"get namespace "* ]]; then echo namespace; exit 0; fi
if [[ "$*" == *'containers[?(@.name=="cloudflared")].image'* ]]; then echo "${SIDECAR_IMAGE_REF}"; exit 0; fi
if [[ "$*" == *'containers[?(@.name=="mcp-gateway")].image'* ]]; then echo "${DEPLOYED_IMAGE_REF}"; exit 0; fi
if [[ "$*" == *"get deployment/bs-mcp-gateway"* ]]; then echo deployment.apps/bs-mcp-gateway; exit 0; fi
if [[ "$*" == *"get service/bs-mcp-gateway-service"* ]]; then echo service/bs-mcp-gateway-service; exit 0; fi
if [[ "$*" == *"rollout status"* ]]; then echo rolling; exit 0; fi
if [[ "$*" == *"wait"* ]]; then echo ready; exit 0; fi

if [[ "$*" == *"get secret bs-mcp-secret"* ]]; then
  if [[ "$*" =~ jsonpath=\{\.data\.([A-Za-z0-9_]+)\} ]]; then
    key="${BASH_REMATCH[1]}"
    if [ "${key}" = "${REMOVED_KEY:-}" ]; then
      printf ''
      exit 0
    fi
    printf 'real-value' | base64
    exit 0
  fi
fi

echo "unhandled kubectl args: $*" >&2
exit 1
FAKE_KUBECTL
chmod +x "${TMP_DIR}/kubectl"

# Fake curl for the confirmed-path public checks: OAuth metadata 200, no-token
# /mcp 401.
cat >"${TMP_DIR}/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"/.well-known/oauth-protected-resource"* ]]; then
  echo '{"resource":"https://mcp.example/","authorization_servers":[]}'
  exit 0
fi
if [[ "$*" == *"/mcp"* && "$*" == *"%{http_code}"* ]]; then
  echo 401
  exit 0
fi
echo "unhandled curl args: $*" >&2
exit 1
FAKE_CURL
chmod +x "${TMP_DIR}/curl"

# env keeps the assignments off the command-prefix (avoids shellcheck SC2097/
# SC2098) while still exporting them to verify-mcp-k8s.sh and the fake kubectl.
run_verify() {
  env \
    PATH="${TMP_DIR}:${PATH}" \
    DEPLOYED_IMAGE_REF="${IMAGE_REF}" \
    SIDECAR_IMAGE_REF="${SIDECAR_IMAGE_REF}" \
    REMOVED_KEY="${1:-}" \
    ENVIRONMENT="${2:-production}" \
    IMAGE_REF="${IMAGE_REF}" \
    MANUAL_CHECKS_CONFIRMED="${3:-false}" \
    bash "${ROOT_DIR}/scripts/verify-mcp-k8s.sh"
}

echo "== positive: full key set, manual confirmed -> exit 0 =="
if ! output="$(run_verify '' production true 2>&1)"; then
  echo "${output}"
  fail "verifier rejected a complete secret + confirmed manual checks"
fi
printf '%s\n' "${output}" | grep -q "Manual production checks confirmed." \
  || fail "verifier did not report success on the full key set"

echo "== negative: CLOUDFLARE_TUNNEL_TOKEN removed -> exit 1 =="
if output="$(run_verify CLOUDFLARE_TUNNEL_TOKEN production true 2>&1)"; then
  echo "${output}"
  fail "verifier PASSED with CLOUDFLARE_TUNNEL_TOKEN missing"
fi
printf '%s\n' "${output}" | grep -q "missing required key in bs-mcp-secret: CLOUDFLARE_TUNNEL_TOKEN" \
  || fail "verifier did not flag the missing tunnel token"

echo "== negative: MCP_WORKOS_CLIENT_ID removed -> exit 1 =="
if output="$(run_verify MCP_WORKOS_CLIENT_ID production true 2>&1)"; then
  echo "${output}"
  fail "verifier PASSED with MCP_WORKOS_CLIENT_ID missing"
fi
printf '%s\n' "${output}" | grep -q "missing required key in bs-mcp-secret: MCP_WORKOS_CLIENT_ID" \
  || fail "verifier did not flag the missing WorkOS client id"

echo "PASS: verify-mcp-k8s enumerates required keys and fails on a missing one"
