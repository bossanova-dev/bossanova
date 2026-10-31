#!/bin/sh
set -eu

usage() {
  echo "usage: $0 check|install <version>" >&2
  exit 2
}
[ "$#" -eq 2 ] || usage
operation=$1
version=${2#v}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
gobin=$(go env GOBIN)
[ -n "$gobin" ] || gobin=$(go env GOPATH)/bin

# Prints the minor number of a goX.Y[.Z] version, or nothing for an unparseable one.
go_minor() {
  printf '%s\n' "$1" | sed -n 's/^go1\.\([0-9][0-9]*\).*/\1/p'
}

check() {
  bin=$(command -v golangci-lint 2>/dev/null) || {
    echo "golangci-lint not installed. Run 'make deps'." >&2
    return 3
  }
  got=$("$bin" --version 2>/dev/null) || got=unreadable
  if ! printf '%s\n' "$got" | grep -Eq "version v?$version( |\$)"; then
    echo "golangci-lint version mismatch at $bin: want v$version" >&2
    echo "  got: $got" >&2
    echo "  run 'make deps' to install the pinned version" >&2
    return 1
  fi
  built=$(printf '%s\n' "$got" | sed -n 's/.* built with \(go[^ ]*\).*/\1/p')
  lint_go=$(node "$script_dir/../test-infra/golangci-toolchain.mjs" goversion) || return 5
  built_minor=$(go_minor "$built")
  lint_minor=$(go_minor "$lint_go")
  if [ -n "$built_minor" ] && [ -n "$lint_minor" ] && [ "$built_minor" -lt "$lint_minor" ]; then
    echo "golangci-lint at $bin was built with $built, older than the Go lint runs under ($lint_go);" >&2
    echo "  it cannot typecheck packages compiled by $lint_go." >&2
    echo "  run 'make deps-golangci' to install the pinned release." >&2
    echo "  If that release is too old, bump GOLANGCI_LINT_VERSION; rebuilding from source does not help." >&2
    return 4
  fi
}

case "$operation" in
  check) check; exit $? ;;
  install) ;;
  *) usage ;;
esac

if check 2>/dev/null; then
  echo "    golangci-lint: v$version already installed"
  exit 0
fi
mkdir -p "$gobin"
echo "    golangci-lint: installing v$version..."
want="v$version" gobin="$gobin" "$script_dir/retry.sh" 3 5 sh -c 'set -e; tmp=$(mktemp); trap "rm -f $tmp" EXIT; curl -sSfL -o "$tmp" "https://raw.githubusercontent.com/golangci/golangci-lint/$want/install.sh"; sh "$tmp" -b "$gobin" "$want"'
check
