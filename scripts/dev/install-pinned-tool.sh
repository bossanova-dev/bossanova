#!/bin/sh
# Supply a pinned tool from a release, then the Go module proxy. Callers verify it.
set -eu

if [ "$#" -lt 5 ] || [ "$4" != -- ]; then
  echo "usage: $0 <gobin> <go-module-cmd-path> <version> -- <release-command...>" >&2
  exit 2
fi
gobin=$1
module=$2
version=${3#v}
shift 4
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if "$script_dir/retry.sh" 3 5 "$@" >&2; then
  exit 0
else
  release_status=$?
fi
if ! command -v go >/dev/null 2>&1; then
  echo 'release install failed; module-proxy fallback unavailable: go not on PATH' >&2
  exit "$release_status"
fi
# Release checksums cannot match a local build; source integrity uses Go's checksum database.
echo "release install failed; building $module@v$version from the Go module proxy" >&2
if GOBIN=$gobin "$script_dir/retry.sh" 3 5 go install "$module@v$version" >&2; then
  exit 0
else
  proxy_status=$?
  echo "tool install failed from both release (exit $release_status) and Go module proxy (exit $proxy_status)" >&2
  exit "$proxy_status"
fi
