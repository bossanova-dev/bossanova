#!/bin/sh
# buf plugin shim: run protoc-gen-connect-openapi at the version pinned in
# tools/protoc-gen-connect-openapi/go.mod, with nothing required on PATH but go.
#
# The pin lives in its own module outside go.work (like tools/sqlc) because a
# tool's requirements join its module's build list under MVS: declaring it in
# lib/bossalib would move shared dependencies of the product workspace (and
# Bazel's go_deps, which reads go.work). `go tool -n` builds the cached binary
# and prints its path; exec'ing it here keeps buf's working directory, so the
# plugin's repo-relative `base=` option still resolves.
set -eu
tool_dir="$(cd "$(dirname "$0")/../tools/protoc-gen-connect-openapi" && pwd)"
bin="$(GOWORK=off go -C "$tool_dir" tool -n protoc-gen-connect-openapi)"
exec "$bin" "$@"
