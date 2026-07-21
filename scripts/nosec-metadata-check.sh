#!/usr/bin/env bash
# nosec-metadata-check.sh — enforce "no silent suppressions" for the blocking
# gosec gate (BOS-28).
#
# With `nosec: enabled` dropped from .gosec.json, gosec HONORS inline `#nosec`
# annotations, so a suppression is now load-bearing: it removes a finding from a
# BLOCKING release gate. SECURITY.md ("Suppressions") therefore requires every
# `#nosec` to carry inline, auditable justification rather than silently muting
# the scanner. gosec itself accepts a bare `// #nosec G204` with no explanation,
# which would let a future suppression bypass the gate with no visible reason —
# exactly the "silent suppression" the policy forbids.
#
# This check closes that gap. Every suppression in scanned Go code MUST:
#   1. use the `#nosec` form (the single auditable channel `grep -rn '#nosec'`
#      covers) — the alternative `//gosec:disable` form is rejected outright, so
#      it cannot suppress a blocking finding while escaping the register audit;
#   2. name an explicit rule id (`#nosec G304`, not a naked `#nosec` that mutes
#      every rule on the line); and
#   3. carry a `-- <reason>` justification on the annotation; and
#   4. record `owner=@handle`, `review-by=YYYY-MM-DD`, and `issue=BOS-NN` on the
#      annotation or its immediately following comment.
#
# Scope mirrors the gosec job: git-tracked *.go files, excluding the generated
# (gen/, genproto/), testdata, and node_modules trees the scan excludes. Runs
# from the repository root. Exits non-zero (listing every offender) on any bare
# `#nosec`; exits 0 when all suppressions carry a reason.
set -euo pipefail

# Path segments excluded from the gosec scan (see security.yml -exclude-dir).
# Matched against any full path SEGMENT so nested packages are covered too.
is_excluded() {
  case "/$1/" in
    */gen/* | */genproto/* | */testdata/* | */node_modules/*) return 0 ;;
    *) return 1 ;;
  esac
}

# A compliant suppression: `#nosec` + a REQUIRED rule list (one or more exact
# G### gosec IDs) + `--` + a non-metadata reason. The rule list is mandatory
# (`+`, not `*`): a rule-id-less `// #nosec -- reason` suppresses EVERY gosec
# rule on the line (over-broad) and SECURITY.md's format requires an explicit
# rule id, so a naked `#nosec` fails.
compliant_re='#nosec([[:space:]]+G[0-9]{3})+[[:space:]]*--[[:space:]]*[^[:space:]]'
metadata_first_re='#nosec([[:space:]]+G[0-9]{3})+[[:space:]]*--[[:space:]]*(owner=|review-by=|issue=)'
owner_re='owner=@[[:alnum:]][[:alnum:]_-]*'
review_by_re='review-by=[0-9]{4}-[0-9]{2}-[0-9]{2}'
issue_re='issue=BOS-[0-9]+'

# gosec also honors `//gosec:disable[...]` as a suppression form (gosec v2.22.5
# README), which would suppress a blocking finding while bypassing both this
# gate and the `grep -rn '#nosec'` register audit. We do not permit it at all:
# every suppression MUST funnel through the single auditable `#nosec` channel.
disable_re='gosec:disable'

violations=0
report() {
  # report <header-once-msg> <file:line: text>
  if [ "$violations" -eq 0 ]; then
    echo "::error::$1"
  fi
  echo "  $2"
  violations=$((violations + 1))
}

while IFS= read -r file; do
  is_excluded "$file" && continue
  # Only inspect files that actually contain a suppression directive.
  grep -Eq '#nosec|gosec:disable' "$file" 2>/dev/null || continue

  # 1. #nosec must carry a rule id, inline reason, and approval metadata.
  while IFS=: read -r lineno content; do
    if ! printf '%s' "$content" | grep -Eq "$compliant_re"; then
      trimmed=$(printf '%s' "$content" | sed 's/^[[:space:]]*//')
      report "non-compliant #nosec suppression(s): every #nosec MUST carry a rule id AND a '-- <reason>' (SECURITY.md § Suppressions)." \
        "${file}:${lineno}: ${trimmed}"
      continue
    fi
    if printf '%s' "$content" | grep -Eq "$metadata_first_re"; then
      trimmed=$(printf '%s' "$content" | sed 's/^[[:space:]]*//')
      report "non-compliant #nosec suppression(s): every #nosec MUST carry a non-metadata reason before approval metadata (SECURITY.md § Suppressions)." \
        "${file}:${lineno}: ${trimmed}"
      continue
    fi

    metadata_context=$content
    next_line=$(sed -n "$((lineno + 1))p" "$file")
    if printf '%s' "$next_line" | grep -Eq '^[[:space:]]*//[^#]*$'; then
      metadata_context="${metadata_context} ${next_line}"
    fi
    if ! printf '%s' "$metadata_context" | grep -Eq "$owner_re" || \
      ! printf '%s' "$metadata_context" | grep -Eq "$review_by_re" || \
      ! printf '%s' "$metadata_context" | grep -Eq "$issue_re"; then
      trimmed=$(printf '%s' "$content" | sed 's/^[[:space:]]*//')
      report "non-compliant #nosec suppression(s): every #nosec MUST carry owner=@handle, review-by=YYYY-MM-DD, and issue=BOS-NN (SECURITY.md § Suppressions)." \
        "${file}:${lineno}: ${trimmed}"
    fi
  done < <(grep -nF '#nosec' "$file")

  # 2. //gosec:disable is not an accepted suppression form here.
  while IFS=: read -r lineno content; do
    trimmed=$(printf '%s' "$content" | sed 's/^[[:space:]]*//')
    report "disallowed gosec:disable suppression(s): use an auditable '// #nosec <rule> -- <reason>' instead (SECURITY.md § Suppressions)." \
      "${file}:${lineno}: ${trimmed}"
  done < <(grep -nE "$disable_re" "$file")
done < <(git ls-files '*.go')

if [ "$violations" -gt 0 ]; then
  echo "::error::${violations} disallowed/non-compliant suppression(s); use '// #nosec <rule> -- <reason>; owner=@handle review-by=YYYY-MM-DD issue=BOS-NN' (SECURITY.md § Suppressions)."
  exit 1
fi

echo "nosec-metadata-check: all suppressions use an auditable, rule-scoped, approved #nosec"
