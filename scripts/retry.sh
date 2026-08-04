#!/usr/bin/env sh
# retry.sh — run a command, retrying a bounded number of times with linear backoff.
#
# Usage: scripts/retry.sh <attempts> <base_delay_seconds> <command> [args...]
#
# Intended for NETWORK fetches only (downloading a pinned tool, `terraform init`
# pulling providers). A GitHub/registry 5xx reds an otherwise-green release PR,
# and re-running by hand is the only recovery. Do NOT wrap deterministic checks
# (lint, validate, tests) — retrying those just makes a real failure N times
# slower without ever changing the outcome.
#
# To retry a pipeline, pass it to a shell explicitly:
#   scripts/retry.sh 3 5 sh -c 'curl -sSfL "$url" | sh'
#
# Exits with the final attempt's exit code.

set -eu

if [ "$#" -lt 3 ]; then
	echo "usage: $0 <attempts> <base_delay_seconds> <command> [args...]" >&2
	exit 2
fi

attempts="$1"
base_delay="$2"
shift 2

case "$attempts" in
*[!0-9]* | '') echo "$0: attempts must be a positive integer, got '$attempts'" >&2 && exit 2 ;;
esac
case "$base_delay" in
*[!0-9]* | '') echo "$0: base_delay_seconds must be a non-negative integer, got '$base_delay'" >&2 && exit 2 ;;
esac
[ "$attempts" -ge 1 ] || { echo "$0: attempts must be >= 1" >&2 && exit 2; }

attempt=1
while :; do
	# Disable errexit around the call so a failure lands here, not in the trap.
	set +e
	"$@"
	status=$?
	set -e

	[ "$status" -eq 0 ] && exit 0

	if [ "$attempt" -ge "$attempts" ]; then
		echo "retry: '$1' failed with exit $status after $attempt attempt(s) — giving up" >&2
		exit "$status"
	fi

	delay=$((base_delay * attempt))
	echo "retry: '$1' failed with exit $status (attempt $attempt/$attempts) — retrying in ${delay}s" >&2
	[ "$delay" -gt 0 ] && sleep "$delay"
	attempt=$((attempt + 1))
done
