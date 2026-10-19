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
binary_tmp=
sums_tmp=
cleanup() {
  [ -z "$binary_tmp" ] || rm -f "$binary_tmp"
  [ -z "$sums_tmp" ] || rm -f "$sums_tmp"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
fail() {
  echo "buf install failed for $dest from $url/$asset: $1; set BUF=/path/to/buf-$version for an offline binary" >&2
  exit 1
}
mkdir -p "$(dirname -- "$dest")" || fail 'cannot create cache directory'
binary_tmp=$(mktemp "$(dirname -- "$dest")/.buf.XXXXXX") || fail 'cannot create download file'
sums_tmp=$(mktemp "$(dirname -- "$dest")/.buf-sha256.XXXXXX") || fail 'cannot create checksum file'
"$script_dir/retry.sh" 3 5 curl -sSfL "$url/$asset" -o "$binary_tmp" || fail 'binary download failed'
"$script_dir/retry.sh" 3 5 curl -sSfL "$url/sha256.txt" -o "$sums_tmp" || fail 'checksum download failed'
expected=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$sums_tmp")
[ -n "$expected" ] || fail 'release checksum missing'
if command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$binary_tmp") || fail 'checksum computation failed'
elif command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$binary_tmp") || fail 'checksum computation failed'
else
  fail 'install shasum or sha256sum to verify the download'
fi
[ "${actual%% *}" = "$expected" ] || fail 'checksum mismatch'
chmod +x "$binary_tmp" || fail 'cannot mark binary executable'
check "$binary_tmp" || fail 'downloaded binary has the wrong version'
mv -f "$binary_tmp" "$dest" || fail 'atomic install failed'
