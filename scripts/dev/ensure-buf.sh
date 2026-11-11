#!/bin/sh
# Resolve a release-pinned buf without consulting the host's buf installation.
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 check|install <version> <binary>" >&2
  exit 2
fi
operation=$1
version=${2#v}
# Always execute a path, including when an override names a relative file.
case "$3" in
  /*) dest=$3 ;;
  *) dest=$PWD/$3 ;;
esac

# Internal release leg: retry downloads only. Integrity failures must never trigger a source build.
if [ "$operation" = download ]; then
  asset=buf-$(uname -s)-$(uname -m)
  url=https://github.com/bufbuild/buf/releases/download/v$version
  curl -sSfL "$url/$asset" -o "$dest/buf"
  curl -sSfL "$url/sha256.txt" -o "$dest/sha256.txt"
  touch "$dest/release-ready"
  exit 0
fi

check() {
  [ -x "$1" ] && [ ! -d "$1" ] || return 3
  got=$("$1" --version 2>/dev/null) || got=unreadable
  if [ "${got#v}" != "$version" ]; then
    echo "buf version mismatch at $1: want $version, got $got" >&2
    return 1
  fi
}

case "$operation" in
  check) check "$dest"; exit $? ;;
  install) if check "$dest" 2>/dev/null; then exit 0; fi ;;
  *) echo "usage: $0 check|install <version> <binary>" >&2; exit 2 ;;
esac

asset=buf-$(uname -s)-$(uname -m)
url=https://github.com/bufbuild/buf/releases/download/v$version
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
install_tmp=
cleanup() {
  [ -z "$install_tmp" ] || rm -rf "$install_tmp"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
fail() {
  echo "buf install failed for $dest from $url/$asset: $1; set BUF=/path/to/buf-$version for an offline binary" >&2
  exit 1
}
mkdir -p "$(dirname -- "$dest")" || fail 'cannot create cache directory'
install_tmp=$(mktemp -d "$(dirname -- "$dest")/.buf.XXXXXX") || fail 'cannot create install directory'
binary_tmp=$install_tmp/buf
"$script_dir/install-pinned-tool.sh" "$install_tmp" github.com/bufbuild/buf/cmd/buf "$version" -- \
  sh "$0" download "$version" "$install_tmp" || fail 'release and module-proxy installation failed'
if [ -f "$install_tmp/release-ready" ]; then
  expected=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$install_tmp/sha256.txt")
  [ -n "$expected" ] || fail 'release checksum missing'
  if command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$binary_tmp") || fail 'checksum computation failed'
  elif command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$binary_tmp") || fail 'checksum computation failed'
  else
    fail 'install shasum or sha256sum to verify the download'
  fi
  [ "${actual%% *}" = "$expected" ] || fail 'checksum mismatch'
fi
chmod +x "$binary_tmp" || fail 'cannot mark binary executable'
check "$binary_tmp" || fail 'installed binary has the wrong version'
mv -f "$binary_tmp" "$dest" || fail 'atomic install failed'
