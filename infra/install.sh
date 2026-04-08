#!/bin/sh
set -eu

# Bossanova Installer
# Downloads and installs boss, bossd, and plugins from GitHub Releases
# Usage: curl -fsSL https://raw.githubusercontent.com/bossanova-dev/bossanova/main/infra/install.sh | sh

VERSION="latest"
REPO="bossanova-dev/bossanova"
if [ "$VERSION" = "latest" ]; then
  RELEASE_URL="https://github.com/${REPO}/releases/latest/download"
else
  RELEASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
fi

# Colors for output
if [ -t 1 ]; then
  BOLD='\033[1m'
  GREEN='\033[0;32m'
  YELLOW='\033[0;33m'
  RED='\033[0;31m'
  RESET='\033[0m'
else
  BOLD=''
  GREEN=''
  YELLOW=''
  RED=''
  RESET=''
fi

# Print colored message
info() {
  printf "%b\n" "${GREEN}${1}${RESET}"
}

warn() {
  printf "%b\n" "${YELLOW}WARNING: ${1}${RESET}" >&2
}

error() {
  printf "%b\n" "${RED}ERROR: ${1}${RESET}" >&2
}

# Banner
printf "\n%b\n" "${BOLD}Bossanova Installer${RESET}"
printf "Version: %s\n\n" "${VERSION}"

# Prerequisites: Check for required CLIs
check_prerequisites() {
  if ! command -v claude >/dev/null 2>&1; then
    error "Claude Code CLI is required but not installed."
    printf "\nInstall Claude Code:\n"
    printf "  macOS:     brew install --cask claude-code\n"
    printf "  See https://claude.ai/download for other platforms\n\n"
    exit 1
  fi

  if ! command -v gh >/dev/null 2>&1; then
    error "GitHub CLI is required but not installed."
    printf "\nInstall GitHub CLI:\n"
    printf "  macOS:     brew install gh\n"
    printf "  Linux:     See https://github.com/cli/cli#installation\n\n"
    exit 1
  fi

  # Need sha256sum or shasum for verification
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    error "sha256sum or shasum is required for checksum verification."
    exit 1
  fi
}

# Platform detection
detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)

  case "${OS}" in
    darwin)
      OS="darwin"
      ;;
    linux)
      OS="linux"
      ;;
    *)
      error "Unsupported operating system: ${OS}"
      printf "Bossanova supports macOS (darwin) and Linux only.\n"
      exit 1
      ;;
  esac

  case "${ARCH}" in
    x86_64)
      ARCH="amd64"
      ;;
    arm64|aarch64)
      ARCH="arm64"
      ;;
    *)
      error "Unsupported architecture: ${ARCH}"
      printf "Bossanova supports amd64 and arm64 only.\n"
      exit 1
      ;;
  esac

  PLATFORM="${OS}-${ARCH}"

  # Verify platform combination is supported
  case "${PLATFORM}" in
    darwin-amd64|darwin-arm64|linux-amd64)
      # Supported platforms
      ;;
    *)
      error "Unsupported platform combination: ${PLATFORM}"
      printf "Supported platforms:\n"
      printf "  - darwin-amd64 (macOS Intel)\n"
      printf "  - darwin-arm64 (macOS Apple Silicon)\n"
      printf "  - linux-amd64 (Linux x86_64)\n"
      exit 1
      ;;
  esac
}

# SHA256 verification (portable across macOS and Linux)
verify_sha256() {
  file="$1"
  expected="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${file}" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "${file}" | awk '{print $1}')
  fi
  if [ "${actual}" != "${expected}" ]; then
    error "SHA256 mismatch for $(basename "${file}")"
    printf "  Expected: %s\n" "${expected}"
    printf "  Got:      %s\n" "${actual}"
    return 1
  fi
}

# Check if boss is already installed
check_existing_install() {
  if command -v boss >/dev/null 2>&1; then
    UPGRADE=true
    EXISTING_VERSION=$(boss version 2>/dev/null || echo "unknown")
    info "Found existing installation (${EXISTING_VERSION}). Upgrading..."
  else
    UPGRADE=false
  fi
}

# Determine install location
determine_install_dir() {
  # Prefer ~/.local/bin if it exists and is on PATH
  if [ -d "${HOME}/.local/bin" ] && echo "${PATH}" | grep -q "${HOME}/.local/bin"; then
    INSTALL_DIR="${HOME}/.local/bin"
    NEEDS_SUDO=false
  # Otherwise use /usr/local/bin (may need sudo)
  elif [ -w "/usr/local/bin" ]; then
    INSTALL_DIR="/usr/local/bin"
    NEEDS_SUDO=false
  else
    INSTALL_DIR="/usr/local/bin"
    NEEDS_SUDO=true
  fi
}

# Determine plugin directory
determine_plugin_dir() {
  case "${OS}" in
    darwin)
      PLUGIN_DIR="${HOME}/Library/Application Support/bossanova/plugins"
      ;;
    linux)
      PLUGIN_DIR="${HOME}/.config/bossanova/plugins"
      ;;
  esac
}

# Download a binary and its checksum, then verify
download_and_verify() {
  name="$1"
  dest="$2"

  if ! curl -fsSL "${RELEASE_URL}/${name}" -o "${dest}"; then
    error "Failed to download ${name}"
    printf "\nCould not download from: %s\n" "${RELEASE_URL}/${name}"
    return 1
  fi

  # Download SHA256 checksum
  if curl -fsSL "${RELEASE_URL}/${name}.sha256" -o "${dest}.sha256" 2>/dev/null; then
    expected=$(awk '{print $1}' "${dest}.sha256")
    if ! verify_sha256 "${dest}" "${expected}"; then
      error "Integrity check failed for ${name}. Aborting."
      exit 1
    fi
    rm -f "${dest}.sha256"
  else
    warn "SHA256 checksum not available for ${name}, skipping verification"
  fi
}

# Download binaries
download_binaries() {
  TEMP_DIR=$(mktemp -d)
  trap 'rm -rf "${TEMP_DIR}"' EXIT

  printf "  Downloading boss (%s)...          " "${PLATFORM}"
  if ! download_and_verify "boss-${PLATFORM}" "${TEMP_DIR}/boss"; then
    exit 1
  fi
  printf "%bdone%b\n" "${GREEN}" "${RESET}"

  printf "  Downloading bossd (%s)...         " "${PLATFORM}"
  if ! download_and_verify "bossd-${PLATFORM}" "${TEMP_DIR}/bossd"; then
    exit 1
  fi
  printf "%bdone%b\n" "${GREEN}" "${RESET}"

  # Download plugins (2 binaries)
  printf "  Downloading plugins (2)...                  "
  for plugin in bossd-plugin-dependabot bossd-plugin-repair; do
    if ! download_and_verify "${plugin}-${PLATFORM}" "${TEMP_DIR}/${plugin}"; then
      exit 1
    fi
  done
  printf "%bdone%b\n" "${GREEN}" "${RESET}"
}

# Stop running daemon before upgrade
stop_daemon() {
  if [ "${UPGRADE}" = true ]; then
    printf "  Stopping daemon for upgrade...              "
    "${INSTALL_DIR}/boss" daemon stop >/dev/null 2>&1 || true
    # Give it a moment to shut down
    sleep 1
    printf "%bdone%b\n" "${GREEN}" "${RESET}"
  fi
}

# Install binaries
install_binaries() {
  printf "  Installing to %s...               " "${INSTALL_DIR}"

  # Make binaries executable
  chmod +x "${TEMP_DIR}/boss" "${TEMP_DIR}/bossd"
  chmod +x "${TEMP_DIR}"/bossd-plugin-*

  # Install boss and bossd
  if [ "${NEEDS_SUDO}" = true ]; then
    sudo mv "${TEMP_DIR}/boss" "${INSTALL_DIR}/boss"
    sudo mv "${TEMP_DIR}/bossd" "${INSTALL_DIR}/bossd"
  else
    mv "${TEMP_DIR}/boss" "${INSTALL_DIR}/boss"
    mv "${TEMP_DIR}/bossd" "${INSTALL_DIR}/bossd"
  fi

  # Create plugin directory and install plugins
  mkdir -p "${PLUGIN_DIR}"
  mv "${TEMP_DIR}"/bossd-plugin-* "${PLUGIN_DIR}/"

  printf "%bdone%b\n" "${GREEN}" "${RESET}"
}

# Configure plugins
configure_plugins() {
  printf "  Configuring plugins...                      "
  if ! "${INSTALL_DIR}/boss" config init --plugin-dir "${PLUGIN_DIR}" >/dev/null 2>&1; then
    error "Failed to configure plugins"
    exit 1
  fi
  printf "%bdone%b\n" "${GREEN}" "${RESET}"
}

# Register daemon
register_daemon() {
  DAEMON_TYPE="launchd"
  if [ "${OS}" = "linux" ]; then
    DAEMON_TYPE="systemd"
  fi

  printf "  Registering daemon (%s)...             " "${DAEMON_TYPE}"
  if ! "${INSTALL_DIR}/boss" daemon install >/dev/null 2>&1; then
    error "Failed to register daemon"
    exit 1
  fi
  printf "%bdone%b\n" "${GREEN}" "${RESET}"
}

# Success message
print_success() {
  # Get installed version
  INSTALLED_VERSION=$("${INSTALL_DIR}/boss" version 2>/dev/null || echo "unknown")

  printf "\n"
  if [ "${UPGRADE}" = true ]; then
    printf "%b✓ Bossanova upgraded successfully!%b\n\n" "${GREEN}${BOLD}" "${RESET}"
    printf "Previous version: %s\n" "${EXISTING_VERSION}"
    printf "New version:      %s\n" "${INSTALLED_VERSION}"
    printf "Installed to:     %s\n" "${INSTALL_DIR}"
    printf "Plugins:          %s\n" "${PLUGIN_DIR}"
  else
    printf "%b✓ Bossanova installed successfully!%b\n\n" "${GREEN}${BOLD}" "${RESET}"
    printf "Version:      %s\n" "${INSTALLED_VERSION}"
    printf "Installed to: %s\n" "${INSTALL_DIR}"
    printf "Plugins:      %s\n" "${PLUGIN_DIR}"
    printf "\nGet started:\n"
    printf "  boss repo add /path/to/your/repo\n"
    printf "  boss\n"
    printf "\nDocs: https://github.com/bossanova-dev/bossanova\n"
  fi
  printf "\n"
}

# Main execution
main() {
  check_prerequisites
  detect_platform
  check_existing_install
  determine_install_dir
  determine_plugin_dir
  download_binaries
  stop_daemon
  install_binaries
  configure_plugins
  register_daemon
  print_success
}

main
