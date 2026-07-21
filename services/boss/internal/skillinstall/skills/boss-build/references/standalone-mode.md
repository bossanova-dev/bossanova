# Standalone mode (no bossd) — `BOSSD_MANAGED=0`

Read this when the Preflight probe set `BOSSD_MANAGED=0` — i.e. no bossd daemon provisioned this
session. The skill then degrades to a **plain scheduled runner**: it owns branch, PR, and env
fallbacks itself instead of adopting bossd's bootstrap handshake. The body carries the mechanics; this
reference is the full narrative for the four daemon-coupled points.

## The signal

`scripts/bossd-present.mjs` is the single source of truth: bossd injects `BOSS_SESSION_ID` into every
managed session's env and forbids `.env` from shadowing it, so its presence is the reliable
"a daemon owns this session" signal (CLI exit `0` = managed, `3` = standalone). Absence ⇒ standalone.
If a canonical config/env detector later lands, `bossd-present.mjs` delegates to it rather than
reading the var directly.

## Preflight — soften the empty-branch assertion

bossd always pre-creates a dedicated, non-empty session branch, so under `BOSSD_MANAGED=1` the
`test -n "$SESSION_BRANCH"` assertion stays hard. A standalone run may legitimately start on the base
branch (or detached), so that assertion is guarded to fire only under a daemon; Step 2 bootstraps the
branch afterwards.

## Step 2 — bootstrap our own branch

Once the ticket id is known, a standalone run creates its own branch off the base when it is on the
base branch (or has no branch):

```bash
if [ "$BOSSD_MANAGED" = "0" ] && { [ -z "$SESSION_BRANCH" ] || [ "$SESSION_BRANCH" = "$BASE_BRANCH" ]; }; then
  SESSION_BRANCH="boss-build/$(echo "<TICKET-ID>" | tr 'A-Z' 'a-z')"
  git switch -c "$SESSION_BRANCH" "$BASE_BRANCH"
fi
test -n "$SESSION_BRANCH"
```

The branch is named `boss-build/<ticket-id>` (lower-cased). Provisioning an _isolated checkout_
remains the scheduled runner's responsibility; the skill only guarantees it is on a dedicated non-base
branch before it commits.

## Step 2.5 / Step 7 — bootstrap-PR framing → create-if-absent

bossd's bootstrap draft PR and its empty `chore: [skip ci] create pull request` commit exist **only**
under a daemon. So the Step 2.5 routing rows about ignoring a bootstrap commit and reusing a bootstrap
PR apply to `BOSSD_MANAGED=1` only. Standalone has neither, and collapses to exactly two routes:

- **fresh** — no open PR → Step 7's `gh pr create` branch makes it. This is the common standalone path:
  "create the PR yourself if absent" is the default, not an exception.
- **resume** — a prior standalone run already left real commits / an open PR on the branch → reuse it
  (Step 7's `gh pr edit` branch, reachable only on this resume).

The `foreign` guard (a branch carrying real work matching no ownership signal ⇒ `NO_CHANGE`) is
unchanged and applies in both modes. No PR command semantics change: an empty `PR_NUMBER` already
routes to `gh pr create`, so the create-if-absent default needs no new code.

## Step 3 — guard the session link on env presence

`update_session` links the boss session to the ticket for the TUI `[l]inear` shortcut. It is called
only when `BOSS_SESSION_ID` is set (`BOSSD_MANAGED=1`). Under `BOSSD_MANAGED=0` there is no bossd
session to link, so the call is skipped — expected standalone degradation, not an error. It was always
best-effort and non-fatal.

## Step 11 — proof env fallback

The proof upload env (`PROOF_ANTHROPIC_API_KEY`, etc.) is daemon-injected. Under `BOSSD_MANAGED=0` it
is not injected, so `node scripts/proof.mjs run` legitimately posts its honest `env-unavailable` note
(doctor output embedded). That is the intended non-fatal standalone degradation — capture-only and
never a terminal-state change — not a failure to fix. No proof code changes; `proof.mjs` already gates
on the key.

## Step 12 — Stop-hook removal is an inherent no-op

bossd installs the Stop-hooks the skill removes so bossd does not double-finalize. Under
`BOSSD_MANAGED=0` bossd installed none, so `node scripts/remove-bossd-stop-hooks.mjs` finds nothing to
remove and writes nothing — an inherent no-op that needs no separate branch. Always call the
single-source-of-truth script; **never** inline its Stop-hook filter (guarded by
`scripts/check-no-inline-stop-hooks.mjs`).
