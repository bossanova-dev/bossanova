# Planning claims

## Claim before drafting

Read `tracker/cli.mjs operations` and require `readComments,writeComment`. Before routing a selected
issue, save its bare payload to this run's `precheck` scratch artifact. Generate a token with
`node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" claim-token`, retaining it as `PLAN_CLAIM_TOKEN`.
Before posting any comment, acquire the issue lease with that same token:

```sh
node "$BOSS_PLAN_TOOLBOX/issue-lease.mjs" acquire --scope plan --issue "<ISSUE-ID>" --token "$PLAN_CLAIM_TOKEN"
```

- Exit 0 (`acquired`): retain the lease for the whole planning run; continue to the comment claim.
- Exit 3 (`held`): post nothing. Treat this as `LOST` with no comment to delete: headless sweeps
  end `noop`; named-ticket or interactive runs report the peer.
- Exit 4 (`unavailable`): record its reason in the run report and continue to the comment claim.
- Exit 2: correct the usage error or abort closed; never treat it as graceful degradation.

The lease defaults to 7200 seconds. Per-child epic claims use this same protocol and retain their
own issue id and token until release; the parent keeps its lease until the parent run ends.
Then format the comment with
`node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" claim-comment --scope plan --token "$PLAN_CLAIM_TOKEN" --session-id "${BOSS_SESSION_ID:-}"`.
Post that body through `writeComment`, retaining both token and returned comment id. Re-read
`readComments` into `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.claim-comments.json` and run
`node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" claim-verdict --scope plan --me "$PLAN_CLAIM_TOKEN" --comments "$CLAIM_COMMENTS"`.
`CLAIM_COMMENTS` names that fresh scratch file. Planning claims never use build scope.

- Exit 0 (`WON`): continue. Plan claims older than two hours forfeit automatically without a
  session-liveness lookup; younger claims arbitrate in the adapter's claim order: earliest `createdAt` then token, or lowest comment id on trackers whose `created_at` is too coarse, such as GitHub.
- Exit 3 (`LOST`): release your own comment. Headless sweeps end `noop`. Selection already
  skipped claimed heads, so losing means a peer claimed the same head concurrently; the next
  scheduled run selects the next available head. Interactive or named-ticket runs report the peer; only
  an explicit interactive confirmation permits continuing unclaimed.
- Exit 4 (`NO_WINNER`): release and retry claiming once; a repeat ends `noop`.
- Exit 2 naming an unsupported `claimScopes` plan capability: warn, continue unclaimed and omit
  `--claim-token` at rechecks. Other errors stop closed; never format a build claim as a fallback.

Release always includes the lease, even when no comment was posted or deletion is unavailable:

```sh
node "$BOSS_PLAN_TOOLBOX/issue-lease.mjs" release --scope plan --issue "<ISSUE-ID>" --token "$PLAN_CLAIM_TOKEN"
```

Also delete only the retained own comment id through optional `deleteComment` when available.
Both releases are best-effort, never fatal, and run before retrying or selecting another head,
every terminal Phase 6 notes flush, and every Phase 2 abort before scratch removal or `exit 1`.
If deletion is unavailable or fails, report that the comment claim expires; never delete a peer's
claim. Release each retained issue/token pair on terminal paths, including parent and child claims.

## Recheck at write boundaries

After the drafter returns, before metadata adoption or Phase 4 gates, and again immediately before
each attachment prepare after all reviewers and gates, fetch `getIssue` anew and write the bare
payload to `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.recheck.json`. Refresh comments into
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.claim-comments.json` as well. Keep the original
selection payload unchanged. Child planners use these names under their own run scratch and their
own selected issue id; the parent claim remains held until the parent run ends.

Invoke `node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" recheck "$PRECHECK" "$RECHECK" --selected-id "<ISSUE-ID>"`.
`PRECHECK` is the original selection artifact and `RECHECK` the new bare payload. If the run holds
a planning claim, append `--claim-token "$PLAN_CLAIM_TOKEN" --comments "$CLAIM_COMMENTS"`.
Do not pass a token on the unsupported-adapter or confirmed interactive override route.
For a multi-upload epic, append `--own-attachment-id <id>` once per exact attachment id returned
by this run's successful `finalizePlanAttachment` calls for this target issue. Retain those ids
across prepare retries. Never infer ownership from title, a fresh attachment list, or a prior run;
never replace or add attachments to the original selection snapshot. These exact own ids are
excluded only from `peer-attachment` detection; identity, state, planned-state and claim checks
still run, and any other new plan or spec attachment still stops the run.

Exit 0 (`proceed`) permits the next step. Exit 3 (`stop`) reports all reasons: `planned-by-peer`,
`state-moved`, `peer-attachment`, `claim-lost`, `claim-missing`, or `fetched-issue-id-mismatch`.
Release the claim, invoke `guard-discard` for the draft through the current run-file protocol,
remove only this run's scratch, make no planning write, and finish `noop` through Phase 6/7.
Exit 2 is unreadable or usage input: correct it or stop closed, release and report the error;
never continue with an unevaluated recheck. An attachment present at selection is not a peer write.

The cron gate suppresses fully claimed queues only when its candidate batch is below the 250-row
limit. A full batch leaves the earlier existence answer intact because unclaimed work may lie
beyond it. This conservative case may start a planner that finds no candidate in its bounded
window; it does not prove the whole backlog is claimed. The worker retains its normal bounded
selection and claim arbitration.
