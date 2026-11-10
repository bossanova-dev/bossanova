# Planning claims

## Claim before drafting

Read `tracker/cli.mjs operations` and require `readComments,writeComment`. Before routing a selected
issue, run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs read-issue --id <ISSUE-ID> --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json`.
Exit 0 continues; exit 64 means fix the call.

On exit 2, execute the tracker adapter's `getIssue` operation and save the bare object. Keep identity
(`id` or `identifier`), a non-empty state name (`status`, `stateName`, string `state` or `state.name`),
an own `description` string or null, and the complete `attachments` array of objects. This is the
`ISSUE_SNAPSHOT_FIELDS` contract from `tracker/issue-snapshot.mjs`; guards refuse incomplete snapshots
with exit 2. Preserve the adapter's UUID for later description reads.

The claim's token and posted comment id live only in
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.claim-receipt.json`. Set `CLAIM_RECEIPT` to that
path in each shell call; never carry ownership state in shell variables or invent another file.
Start the receipt, then acquire its issue lease before posting any comment:

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-claim.mjs start --receipt "$CLAIM_RECEIPT" --issue <ISSUE-ID>
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-claim.mjs acquire --receipt "$CLAIM_RECEIPT"
```

- Exit 0 (`acquired`): retain the lease for the whole planning run; continue to the comment claim.
- Exit 3 (`held`): post nothing. Refresh comments and gather owner evidence with the helper below,
  including `--release-forfeited-leases`. If any peer's `lease.verdict` is `released`, retry acquire
  once. Otherwise treat this as `LOST` with no comment to delete: headless sweeps end `noop`;
  named-ticket or interactive runs report the peer from `peers`. A second held result is `LOST`.
- Exit 4 (`unavailable`): record its reason in the run report and continue to the comment claim.
- Exit 2: correct the usage error or abort closed; never treat it as graceful degradation.

The lease defaults to 7200 seconds. Per-child epic claims use their own receipt under their
own run scratch; the parent keeps its lease until the parent run ends.
Then format the comment with
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-claim.mjs comment --receipt "$CLAIM_RECEIPT" --session-id "${BOSS_SESSION_ID:-}"`.
Post that body through `writeComment`, then immediately persist its returned id with
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-claim.mjs record-comment --receipt "$CLAIM_RECEIPT" --id <COMMENT-ID>`.
Every later step reads ownership from the receipt. Re-read `readComments` into
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.claim-comments.json` and run
the owner evidence helper, then arbitration:

```sh
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node session/claim-owner-status.mjs --comments "$CLAIM_COMMENTS" --claim-receipt "$CLAIM_RECEIPT" --out "$CLAIM_LIVENESS" --issue "<ISSUE-ID>" --release-forfeited-leases
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-claim.mjs verdict --receipt "$CLAIM_RECEIPT" --comments "$CLAIM_COMMENTS" --liveness "$CLAIM_LIVENESS"
```

`CLAIM_COMMENTS` names the fresh `claim-comments` scratch file; `CLAIM_LIVENESS` is this run's
`claim-liveness` artifact, `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.claim-liveness.json`.
Use the same helper after refreshing comments on the lease-held route. Exit 2 requires fixing
unreadable comments or usage before continuing; a failed owner lookup contributes no evidence.
Planning claims never use build scope.

- Exit 0 (`WON`): continue. Plan claims forfeit after two hours, or immediately when the owner
  session is proven archived or closed. An unknown or unreadable owner adds no forfeiture evidence;
  NOT_FOUND, MERGED and ORPHANED retain the age rule. Surviving claims arbitrate in the adapter's claim order: earliest `createdAt` then token, or lowest comment id on trackers whose `created_at` is too coarse, such as GitHub.
- Exit 3 (`LOST`): release through the receipt below. Headless sweeps end `noop`. Selection already
  skipped claimed heads, so losing means a peer claimed the same head concurrently; the next
  scheduled run selects the next available head. Name every non-terminal peer from the evidence's
  `peers`: token prefix, owner session (or none), `ageMinutes`, and minutes until the two-hour
  forfeit (`max(0, 120 - ageMinutes)`). Interactive or named-ticket runs report the peer; only
  an explicit interactive confirmation permits continuing unclaimed.
- Exit 4 (`NO_WINNER`): release, then run `start` again on the same receipt for a fresh token
  and retry claiming once; a repeat ends `noop`.
- Exit 2 naming an unsupported `claimScopes` plan capability: warn, continue unclaimed and omit
  `--claim-receipt` at rechecks. Release through the receipt first. Other errors stop closed; never format a build claim as a fallback.

Release always includes the lease, even when no comment was posted or deletion is unavailable:

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-claim.mjs release --receipt "$CLAIM_RECEIPT"
```

The helper prints `{lease,commentId}` and records the release in the receipt. Use that printed
`commentId`, when non-null, to delete exactly this run's comment through optional `deleteComment`.
If the deletion happens in a later shell call, invoke `release` again to obtain the recorded id;
never carry the id across calls. Lease release and comment deletion are best-effort, never fatal,
and run before retrying or selecting another head, every terminal Phase 6 notes flush, and every
Phase 2 abort before scratch removal or `exit 1`. Remove scratch last, after deletion has been
attempted. If deletion is unavailable or fails, report that the comment claim expires; never delete
a peer's claim, including forfeited terminal peers. Release through each parent and child receipt
on terminal paths.

## Recheck at write boundaries

After the drafter returns, before metadata adoption or Phase 4 gates, and again immediately before
each attachment prepare after all reviewers and gates, run
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs read-issue --id <ISSUE-ID> --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.recheck.json`.
Exit 0 continues; exit 2 uses the fallback above; exit 64 means fix the call. Refresh comments into
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.claim-comments.json` as well. When holding a planning
claim, refresh `CLAIM_LIVENESS` through the owner evidence helper above. Keep the original
selection payload unchanged. Child planners use these names under their own run scratch and their
own selected issue id; the parent claim remains held until the parent run ends.

Invoke `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-run-guards.mjs recheck "$PRECHECK" "$RECHECK" --selected-id "<ISSUE-ID>"`.
`PRECHECK` is the original selection artifact and `RECHECK` the new bare payload. If the run holds
a planning claim, append `--claim-receipt "$CLAIM_RECEIPT" --comments "$CLAIM_COMMENTS" --liveness "$CLAIM_LIVENESS"`.
Omit the claim receipt on the unsupported-adapter or confirmed interactive override route.
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
Exit 2 is unreadable or usage input, including an incomplete snapshot: correct it or stop closed, release and report the error;
never continue with an unevaluated recheck. An attachment present at selection is not a peer write.

The cron gate suppresses fully claimed queues only when its candidate batch is below the 250-row
limit. A full batch leaves the earlier existence answer intact because unclaimed work may lie
beyond it. This conservative case may start a planner that finds no candidate in its bounded
window; it does not prove the whole backlog is claimed. The worker retains its normal bounded
selection and claim arbitration.
