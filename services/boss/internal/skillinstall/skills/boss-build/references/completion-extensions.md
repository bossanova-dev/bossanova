# Completion extensions (Step 12)

The phase runs after Stop-hook removal and before lock release, callback cleanup and the terminal
line. `OUTCOME` is already decided and never changes. Completion is unsampled and independent of
`BOSS_NOTES_SUPPRESSED`. The helper owns eligibility, merge execution and final-state verification;
extensions own only repo policy about requesting an authorized merge.

## Once per run

Discover `--core boss-build --role completion --json`. Record each non-deliberate discovery skip as
`extension <name>: skipped (<reason>)`. No extension or no PR means a silent no-op: stamp
`completion-phase-done` without live reads or dispatch.

Otherwise use git-dir paths keyed by `BLI_RUNID` for the envelope, check-verdict JSON, validated
results array and attempt file. The final record is
`$(git rev-parse --git-dir)/boss-build-completion.json`. A record with this run id means the phase
already settled: do not re-dispatch. Pass the Step 12 CI reading, rather than a previous snapshot:

```bash
node "$BOSS_BUILD_TOOLBOX/completion-gate.mjs" envelope \
  --run-id "$BLI_RUNID" --outcome "$OUTCOME" --pr "$PR_NUMBER" \
  --ci-wait-state "$CI_WAIT_STATE" --check-verdict-file "$CHECK_VERDICT_FILE" \
  --extension-count "$COMPLETION_EXTENSION_COUNT" \
  --callbacks-available "$COMPLETION_CALLBACKS_AVAILABLE" \
  --attempt-file "$COMPLETION_ATTEMPT_FILE" --out "$COMPLETION_ENVELOPE_FILE"
```

The helper reads live PR/repo/session state, current head and upstream, reviewed-tree drift and
watchers before cleanup. Its context includes the review tokens, criteria counts, follow-up counts,
launch origin and archive-after-merge flag, plus `mergeEligible`, `ineligibleReasons`,
`mergeAuthorized`, `authorizationReasons` and the exact argv `mergeCommand`. Unknown evidence is
ineligible. Repos authorize merges only with literal `completionDefaults.allowMerge: true`.

## One awaited worker

Dispatch one worker that reads each descriptor's `skillPath`, resolves its resources from `dir`,
and runs extensions in `(order, name)` order. Give each the envelope
`{role: "completion", core: "boss-build", context: <helper envelope>, runTmp, outPath}` and bound
it by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`. Validate every result with
`skill-extensions.mjs validate --role completion --file <outPath>`; retain valid envelopes in the
results JSON array and record failures as skip lines. Await the worker before settling.

An extension returns the standard `ok`, `extension`, `role` header and top-level
`{action, reason, mergeSha}`. `action` is `merged` or `skipped`, `reason` a non-empty string;
`mergeSha` is a 40-hex SHA for `merged`, and `""` for `skipped`.

**The only permitted merge path is `context.mergeCommand`.** Run that exact argv and wrap its
JSON result in the extension result header. Never use `gh pr merge`, a direct `boss merge`, or
`merge_session`. The helper rechecks authorization and live state, records the attempt before
calling `boss merge <session-id> --yes --json`, and refuses a second attempt in the same run.
Epic-driven launches remain ineligible; their skip reason is `epic-child`.

## Settle and publish

```bash
node "$BOSS_BUILD_TOOLBOX/completion-gate.mjs" settle \
  --envelope "$COMPLETION_ENVELOPE_FILE" --attempt-file "$COMPLETION_ATTEMPT_FILE" \
  --results-file "$COMPLETION_RESULTS_FILE" \
  --out "$(git rev-parse --git-dir)/boss-build-completion.json"
```

Trust only the settled, run-id-matched record, which verifies the live PR state and merge SHA.
An extension's merge claim alone proves nothing. An observed merge outside the gate is recorded as
`merged-outside-gate` with a warning; a claim without an observed merge becomes
`claimed-merge-not-observed`. All phase failures warn or record extension skip lines, never change
`OUTCOME`, and still stamp `completion-phase-done` before releasing the lock.

For a verified `merged` record, stamp `pr-merged-by-completion`, move the ticket to the configured
`.done` state and remove `please-review` (best-effort, warnings on failure). Callback cleanup still
runs; skip CI-watch classification. Print
`REVIEW_READY <ticket> <pr-url> merged <sha> — <summary>`.
For any other result, retain existing tracker, label, callback-watch and terminal behavior.
