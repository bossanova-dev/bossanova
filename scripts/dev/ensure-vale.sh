#!/bin/sh
# Install a verified release, or build the pinned module when downloads are unavailable.
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 check|install <version> <binary>" >&2
  exit 2
fi
operation=$1
version=${2#v}
case "$3" in
  /*) dest=$3 ;;
  *) dest=$PWD/$3 ;;
esac

check() {
  [ -x "$1" ] && [ ! -d "$1" ] || return 3
  got=$("$1" --version 2>/dev/null) || got=unreadable
  [ "$got" = "vale version $version" ] && return 0
  echo "vale version mismatch at $1: want $version, got $got" >&2
  return 1
}

case "$operation" in
  check) check "$dest"; exit $? ;;
  install) if check "$dest" 2>/dev/null; then exit 0; fi ;;
  download) ;;
  *) echo "usage: $0 check|install <version> <binary>" >&2; exit 2 ;;
esac

case "$(uname -s)" in
  Linux) platform=Linux ;;
  Darwin) platform=macOS ;;
  *) echo 'vale: unsupported operating system' >&2; exit 2 ;;
esac
case "$(uname -m)" in
  x86_64) arch=64-bit ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo 'vale: unsupported architecture' >&2; exit 2 ;;
esac
asset=vale_${version}_${platform}_${arch}.tar.gz
url=https://github.com/errata-ai/vale/releases/download/v$version

if [ "$operation" = download ]; then
  curl -sSfL "$url/$asset" -o "$dest/$asset"
  curl -sSfL "$url/vale_${version}_checksums.txt" -o "$dest/checksums.txt"
  # A partial archive download must not win over the completed source fallback.
  touch "$dest/release-ready"
  exit 0
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
work=
cleanup() { [ -z "$work" ] || rm -rf "$work"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
fail() { echo "vale install failed for $dest: $1" >&2; exit 1; }
mkdir -p "$(dirname -- "$dest")" || fail 'cannot create cache directory'
work=$(mktemp -d "$(dirname -- "$dest")/.vale.XXXXXX") || fail 'cannot create install directory'
mkdir -p "$work/release" "$work/gobin" "$work/extracted"
GOFLAGS="-ldflags=-X=main.version=$version" GOTOOLCHAIN=auto \
  "$script_dir/install-pinned-tool.sh" "$work/gobin" github.com/errata-ai/vale/v3/cmd/vale "$version" -- \
  sh "$0" download "$version" "$work/release" || fail 'release and module-proxy installation failed'

if [ -f "$work/release/release-ready" ]; then
  # Integrity failures are terminal: never hide a corrupt release with a source build.
  expected=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$work/release/checksums.txt")
  [ -n "$expected" ] || fail 'release checksum missing'
  if command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$work/release/$asset") || fail 'checksum computation failed'
  elif command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$work/release/$asset") || fail 'checksum computation failed'
  else
    fail 'install shasum or sha256sum to verify the download'
  fi
  [ "${actual%% *}" = "$expected" ] || fail 'checksum mismatch'
  tar -xzf "$work/release/$asset" -C "$work/extracted" || fail 'release extraction failed'
  binary=$work/extracted/vale
else
  binary=$work/gobin/vale
  buildinfo=$(go version -m "$binary") || fail 'cannot read source buildinfo'
  printf '%s\n' "$buildinfo" | awk -v version="v$version" \
    '$1 == "mod" && $2 == "github.com/errata-ai/vale/v3" && $3 == version { found=1 } END { exit !found }' ||
    fail 'source buildinfo has the wrong main module or version'
fi
chmod +x "$binary" || fail 'cannot mark binary executable'
check "$binary" || fail 'installed binary has the wrong version'
mv -f "$binary" "$dest" || fail 'atomic install failed'
