#!/bin/sh
# Install a verified release, or build locked crates when downloads are unavailable.
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
  [ "$got" = "lychee $version" ] && return 0
  echo "lychee version mismatch at $1: want $version, got $got" >&2
  return 1
}

case "$operation" in
  check) check "$dest"; exit $? ;;
  install) if check "$dest" 2>/dev/null; then exit 0; fi ;;
  download) ;;
  *) echo "usage: $0 check|install <version> <binary>" >&2; exit 2 ;;
esac

case "$(uname -s)" in
  Linux) target=unknown-linux-gnu ;;
  Darwin) target=apple-darwin ;;
  *) echo 'lychee: unsupported operating system' >&2; exit 2 ;;
esac
case "$(uname -m)" in
  x86_64) arch=x86_64 ;;
  arm64|aarch64) arch=aarch64 ;;
  *) echo 'lychee: unsupported architecture' >&2; exit 2 ;;
esac
asset=lychee-$arch-$target.tar.gz
url=https://github.com/lycheeverse/lychee/releases/download/lychee-v$version

if [ "$operation" = download ]; then
  curl -sSfL "$url/$asset" -o "$dest/$asset"
  curl -sSfL "$url/$asset.sha256" -o "$dest/checksums.txt"
  touch "$dest/release-ready"
  exit 0
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
work=
cleanup() { [ -z "$work" ] || rm -rf "$work"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
fail() { echo "lychee install failed for $dest: $1" >&2; exit 1; }
mkdir -p "$(dirname -- "$dest")" || fail 'cannot create cache directory'
work=$(mktemp -d "$(dirname -- "$dest")/.lychee.XXXXXX") || fail 'cannot create install directory'
mkdir -p "$work/release" "$work/extracted"

if "$script_dir/retry.sh" 3 5 sh "$0" download "$version" "$work/release" >&2; then
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
  binary=$(find "$work/extracted" -type f -name lychee) || fail 'cannot locate release binary'
else
  release_status=$?
  if ! command -v cargo >/dev/null 2>&1; then
    echo 'release install failed; cargo fallback unavailable: cargo not on PATH' >&2
    exit "$release_status"
  fi
  echo "release install failed; building lychee $version through cargo" >&2
  if "$script_dir/retry.sh" 3 5 cargo install --locked lychee --version "$version" --root "$work/cargo" >&2; then
    binary=$work/cargo/bin/lychee
  else
    cargo_status=$?
    echo "tool install failed from both release (exit $release_status) and cargo (exit $cargo_status)" >&2
    exit "$cargo_status"
  fi
fi
chmod +x "$binary" || fail 'cannot mark binary executable'
check "$binary" || fail 'installed binary has the wrong version'
mv -f "$binary" "$dest" || fail 'atomic install failed'
