# Area caps

Two traced escapes from distinct High PRs cap their shared area at Moderate when their fixes merge
within seven days of each other. Only a successful `boss/verify` status described as `verified` at
the original merge head counts as High. Approved merges and older records without a level do not
count. A second fix of the same original PR does not count again.

A cap stays active until an operator clears it. Only PRs touching the capped area gain an always
ask; other PRs keep their level. The catch-all `other` area is scoped to the first two directory
segments of each changed path, for example `other:packages/api`. The affected PR's label, park note
and confidence reasons identify the cap and the escapes. Ordinary approval can approve that PR;
it does not clear the area for future PRs.

Run these verbs from the repository checkout. Each accepts `--repo <owner/name>`:

```bash
node "$BOSS_VERIFY_TOOLBOX/verify/verify-area-caps-sync.mjs" refresh
node "$BOSS_VERIFY_TOOLBOX/verify/verify-area-caps-sync.mjs" status
node "$BOSS_VERIFY_TOOLBOX/verify/verify-area-caps-sync.mjs" clear --area other:packages/api --reason "Root cause repaired"
node "$BOSS_VERIFY_TOOLBOX/verify/verify-area-caps-sync.mjs" history --since 2026-01-01T00:00:00Z
```

`refresh` traces fixes from the last three days, then reads all recorded escapes and clears.
`status` reads those records and refreshes the local state without tracing. `clear` requires a reason;
add `--dry-run` to inspect its comment without writing. The clear is stored as a marker comment on
the newest counted escape's original PR, so every host sees it. Escapes fixed on or before the
clear remain covered even if traced later. Two new escapes after the clear can cap the area again.
`history` reports each episode, its escapes and who cleared it, when and why, for the weekly retro.

The verify cron gate detaches a refresh at most once every six hours, after its idle and quota
checks and before discovering PRs. When state is missing or older than 24 hours, active sweeps
wait for that refresh to complete before judging; idle ticks do not contact GitHub. A failed
refresh releases the sweep to the fail-open policy below. A refresh that never completes releases
it after five minutes with `area-caps-refresh-timeout` health. Local state lives at
`<git common dir>/boss-verify/area-caps.json`. A trace failure is recorded while existing records
still derive the cap; a record-list failure preserves the previous state. Missing or invalid state
reports `area-caps-unknown`; state older than 24 hours reports `area-caps-stale`. Both fail open
without lowering the head's level by themselves.
