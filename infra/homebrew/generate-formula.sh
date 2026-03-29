#!/usr/bin/env bash
# Generates a Homebrew formula from the template with real checksums.
# Usage: ./generate-formula.sh v1.0.0 /path/to/release/artifacts
#
# Expects release binaries in the artifacts directory (15 total):
#   boss-darwin-arm64, bossd-darwin-arm64
#   boss-darwin-amd64, bossd-darwin-amd64
#   boss-linux-amd64,  bossd-linux-amd64
#   bossd-plugin-autopilot-darwin-arm64, bossd-plugin-autopilot-darwin-amd64, bossd-plugin-autopilot-linux-amd64
#   bossd-plugin-dependabot-darwin-arm64, bossd-plugin-dependabot-darwin-amd64, bossd-plugin-dependabot-linux-amd64
#   bossd-plugin-repair-darwin-arm64, bossd-plugin-repair-darwin-amd64, bossd-plugin-repair-linux-amd64

set -euo pipefail

VERSION="${1:?Usage: $0 <version> <artifacts-dir>}"
ARTIFACTS="${2:?Usage: $0 <version> <artifacts-dir>}"

# Strip leading 'v' for formula version field
FORMULA_VERSION="${VERSION#v}"

sha() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

TEMPLATE="$(dirname "$0")/bossanova.rb"

sed \
  -e "s|\${VERSION}|${FORMULA_VERSION}|g" \
  -e "s|\${SHA256_DARWIN_ARM64_BOSS}|$(sha "${ARTIFACTS}/boss-darwin-arm64")|g" \
  -e "s|\${SHA256_DARWIN_ARM64_BOSSD}|$(sha "${ARTIFACTS}/bossd-darwin-arm64")|g" \
  -e "s|\${SHA256_DARWIN_ARM64_AUTOPILOT}|$(sha "${ARTIFACTS}/bossd-plugin-autopilot-darwin-arm64")|g" \
  -e "s|\${SHA256_DARWIN_ARM64_DEPENDABOT}|$(sha "${ARTIFACTS}/bossd-plugin-dependabot-darwin-arm64")|g" \
  -e "s|\${SHA256_DARWIN_ARM64_REPAIR}|$(sha "${ARTIFACTS}/bossd-plugin-repair-darwin-arm64")|g" \
  -e "s|\${SHA256_DARWIN_AMD64_BOSS}|$(sha "${ARTIFACTS}/boss-darwin-amd64")|g" \
  -e "s|\${SHA256_DARWIN_AMD64_BOSSD}|$(sha "${ARTIFACTS}/bossd-darwin-amd64")|g" \
  -e "s|\${SHA256_DARWIN_AMD64_AUTOPILOT}|$(sha "${ARTIFACTS}/bossd-plugin-autopilot-darwin-amd64")|g" \
  -e "s|\${SHA256_DARWIN_AMD64_DEPENDABOT}|$(sha "${ARTIFACTS}/bossd-plugin-dependabot-darwin-amd64")|g" \
  -e "s|\${SHA256_DARWIN_AMD64_REPAIR}|$(sha "${ARTIFACTS}/bossd-plugin-repair-darwin-amd64")|g" \
  -e "s|\${SHA256_LINUX_AMD64_BOSS}|$(sha "${ARTIFACTS}/boss-linux-amd64")|g" \
  -e "s|\${SHA256_LINUX_AMD64_BOSSD}|$(sha "${ARTIFACTS}/bossd-linux-amd64")|g" \
  -e "s|\${SHA256_LINUX_AMD64_AUTOPILOT}|$(sha "${ARTIFACTS}/bossd-plugin-autopilot-linux-amd64")|g" \
  -e "s|\${SHA256_LINUX_AMD64_DEPENDABOT}|$(sha "${ARTIFACTS}/bossd-plugin-dependabot-linux-amd64")|g" \
  -e "s|\${SHA256_LINUX_AMD64_REPAIR}|$(sha "${ARTIFACTS}/bossd-plugin-repair-linux-amd64")|g" \
  "$TEMPLATE"
