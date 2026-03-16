#!/usr/bin/env bash
# Generates a Homebrew formula from the template with real checksums.
# Usage: ./generate-formula.sh v1.0.0 /path/to/release/artifacts
#
# Expects release binaries in the artifacts directory:
#   boss-darwin-arm64, bossd-darwin-arm64
#   boss-darwin-amd64, bossd-darwin-amd64
#   boss-linux-amd64,  bossd-linux-amd64

set -euo pipefail

VERSION="${1:?Usage: $0 <version> <artifacts-dir>}"
ARTIFACTS="${2:?Usage: $0 <version> <artifacts-dir>}"

# Strip leading 'v' for formula version field
FORMULA_VERSION="${VERSION#v}"

sha() { shasum -a 256 "$1" | awk '{print $1}'; }

TEMPLATE="$(dirname "$0")/bossanova.rb"

sed \
  -e "s|\${VERSION}|${FORMULA_VERSION}|g" \
  -e "s|\${SHA256_DARWIN_ARM64_BOSS}|$(sha "${ARTIFACTS}/boss-darwin-arm64")|g" \
  -e "s|\${SHA256_DARWIN_ARM64_BOSSD}|$(sha "${ARTIFACTS}/bossd-darwin-arm64")|g" \
  -e "s|\${SHA256_DARWIN_AMD64_BOSS}|$(sha "${ARTIFACTS}/boss-darwin-amd64")|g" \
  -e "s|\${SHA256_DARWIN_AMD64_BOSSD}|$(sha "${ARTIFACTS}/bossd-darwin-amd64")|g" \
  -e "s|\${SHA256_LINUX_AMD64_BOSS}|$(sha "${ARTIFACTS}/boss-linux-amd64")|g" \
  -e "s|\${SHA256_LINUX_AMD64_BOSSD}|$(sha "${ARTIFACTS}/bossd-linux-amd64")|g" \
  "$TEMPLATE"
