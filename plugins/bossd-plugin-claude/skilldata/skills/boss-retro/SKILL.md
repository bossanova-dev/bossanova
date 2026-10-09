---
name: boss-retro
disable-model-invocation: true
description: Turn improvement notes, human-correction signals and guidance findings into remedy-ladder tickets. Writes by default; --dry-run previews.
---

# boss-retro

Collect → theme → currency and remedy ladder → select → file → retire. Writes by default;
`--dry-run` performs the same judgment and previews tickets without changing notes, tracker issues
or retro state. `--no-dry-run` is a compatibility no-op. No implementation work belongs in this run.

## Contract

One lock, one snapshot, exactly two awaited dispatches. The first themes; the second judges
currency, ladder, detector hits, the rule of the run and completed-ticket follow-through together.
Unreadable notes or tracker state, malformed output, a failed dispatch or an uncertain recheck
stops filing and retirement. No note outside the snapshot may be deleted or retagged. Selection
adds filing labels and one project; it never narrows marker dedupe. Teardown on every locked exit.

## 0. Preflight

Resolve the installed helpers in every shell block; shell state is not shared:

```bash
BOSS_RETRO_TOOLBOX="${BOSS_RETRO_TOOLBOX:-${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-retro/toolbox}"
if [ ! -d "$BOSS_RETRO_TOOLBOX" ]; then BOSS_RETRO_TOOLBOX="$HOME/.codex/skills/boss-retro/toolbox"; fi
```

Create a scratch run directory outside the checkout and set `BOSS_RETRO_RUN_DIR` to it. Run
`node "$BOSS_RETRO_TOOLBOX/retro-run.mjs" settings -- <arguments>` and save `settings.json`.
Exit 64 means usage error; stop. Carry `mode`, `runId`, `selectArgs`, `pathAliases`,
`guidanceAuditConfig` and `selectionArgs` from that JSON. Report `ignored` once.

Resolve the notes CLI with `node "$BOSS_RETRO_TOOLBOX/boss-binary.mjs"`; unavailable means stop
with `BLOCKED: needs the notes store`. In write mode, require tracker creation and attachment
capabilities using `tracker/cli.mjs operations --require createIssue,preparePlanAttachment,finalizePlanAttachment,readPlanAttachment`.
When `needsResolution` is true, run `tracker/cli.mjs resolve-selection --stage retro` with only
`settings.selectionArgs`; pass its result through `retro-run.mjs filing`. Do not silently discard
unresolved labels or an ambiguous project. Zero supported slots require no resolution request.

Run `retro-write.mjs lock acquire --dir <git-common-dir>`. `held` exits without acting;
`acquired` supplies the token. Install teardown that calls `lock release --dir <git-common-dir>
--token <token>` and removes scratch files on every exit, including interrupted runs. Touch the
lock at phase boundaries and while awaiting workers.

## 1. Collect

Run `retro-signals.mjs collect`, adding `--dry-run` in preview mode. Preserve the JSON and report
its collector statuses; collector failures are non-fatal. Write-mode collection stores signals
before the snapshot so those new notes participate in the same guarded retirement.

Read `boss notes ls --tag improvement --json --limit 0` into `notes.json`, then call
`retro-write.mjs snapshot <notes.json> <snapshot-ids>`. A failed read or snapshot stops the run.
Run `guidance-audit.mjs --notes --run-id <runId>` with `--config <guidanceAuditConfig>` when set.
Audit failure is reported and supplies an empty audit array. Do not use `--record` yet.

Call `retro-run.mjs augment <notes.json> --audit <audit.json> --signals <signals.json> --run-id <runId>`.
In write mode omit `--signals`: those signals are already in the real notes snapshot. Synthetic
audit and preview-signal ids stay out of snapshot-ids and every retirement mutation.

Record the fetch time. Fetch the entire marked set with
`tracker/cli.mjs fetch-marked --prefix "Notes: " --out-file <marked.json>` in either mode.
Any non-zero result stops safely. Call `retro-notes.mjs follow-through-due <marked.json>`;
its default cap bounds completed promises. Report `problems` and leave them untouched.
If the augmented notes and the due promises are both empty, report nothing to retro and teardown.

## 2. Theme — one awaited dispatch

Call `retro-notes.mjs cluster <augmented.json>` and `digest <clusters.json>`. One fresh worker
reads the complete digest, checks source records when needed, and writes a complete partition
with titles in `groups.json` compatible with `mergeClusters`. It does not write notes or tickets.
Await its result, then call `merge <clusters.json> <groups.json>`, `rank <merged.json>` and
`stale <ranked.json> --path-aliases <settings.pathAliases>`. An invalid partition stops the run.
Empty clusters still yield an empty partition when follow-through work is due.

For both dispatches, create a `bs-run-sentinel.mjs make-ctx boss-retro` context, seed no successful
verdict, require the worker to heartbeat its context and write a terminal sentinel only after its
output artifact exists. Record the dispatch start timestamp in epoch milliseconds and pass `--dispatched-at <ms>`
to `bs-dispatch-await.mjs wait` and `disposition` with that heartbeat;
only `publishable: true` plus valid output admits the next phase. Clean each context afterward.
A missing, provisional, foreign or abandoned verdict fails safe. Await every worker; never proceed
from a pending dispatch. Use the harness's separate worker context, not two personas in one chat.

## 3. Currency, ladder and follow-through — one awaited dispatch

Obtain the rung vocabulary from `retro-notes.mjs ladder`. One fresh worker receives ranked themes,
staleness signals, augmented source records, and `follow-through-due.due`. It reads the current
code and writes two arrays: `verdicts.json` for all theme keys and `follow-through-verdicts.json`
for every due promise key. No additional detector or follow-through dispatch is permitted.

For themes, return `{key, verdict, evidence, fixedNotes}` and, for live themes,
`{rung, target, rungEvidence, existingCheck, instances, supersedes}`. Cite or abstain: choose the
strongest feasible rung; rule and lens explain why stronger rungs fail. An existing check means
repair its gap. Confirm prose-pin and guidance-weight hits with source evidence; a false positive
is unverifiable. Judge the quoted rule of the run for promotion or existing enforcement.

For completed promises, use `enforced`, `prose-only` or `regressed`, with evidence of the landed
artifact, `landedRung` and cleanup evidence for the replaced prose instances. A weaker artifact
needs the explicit sourced downgrade supported by the helper; otherwise retain the promised rung.
Abstention is empty evidence, which yields `unconfirmed`, never retirement. The worker writes no
tracker mutation and marks its sentinel only when both complete arrays exist.

Call `retro-notes.mjs verdicts <ranked.json> <verdicts.json>` and
`follow-through <due.json> <follow-through-verdicts.json>`. Keep the returned currency buckets and write `live-clusters.json` from
`currency.live.map(entry => entry.cluster)`; `live` entries wrap the clusters that `select` needs.
Keep follow-through `retire`, `refile`, `unconfirmed`. Missing or malformed keys stop.

## 4. Select and preview

Call `retro-notes.mjs select <live-clusters.json> <marked.json>` with the positional
`settings.selectArgs`, preserving empty positional placeholders. Then call
`retro-run.mjs finalize <selection.json> <snapshot-ids> <augmented.json>`.
This validates all buckets and member ids and moves unpromoted pure rule themes to `stays-rule`.
Use the cap for the total number of children: follow-through refiles consume slots first in due
order; over-cap refiles remain unmarked for a later run. Keep all unfiled notes.

In dry-run mode, print `retro-run.mjs preview <final-selection.json> --path-aliases <aliases>`.
For each admitted refile, print `retro-notes.mjs refile-describe <follow-through.json> <key>`.
Report enforced/unconfirmed follow-through without updating markers. Teardown; no attachment,
recording, note mutation or tracker write. Preview includes rendered `## Codify as` and `## Cleanup`.

## 5. File and follow through — write mode

Use `tracker/cli.mjs operations` to resolve MCP descriptors; execute the tool each descriptor
names. For enforced promises only, read the issue's current full description through `getIssue`,
apply `retro-notes.mjs mark-enforced <description-file> <key> <ISO-time>`, and update that exact
issue through the adapter's `writeDescription` operation via
`tracker/cli.mjs write-description --id <issue-id> --body-file <updated-description>`. Re-read to verify the marker.
Never replace a peer's changed description: re-read immediately before updating and recompute.
An unconfirmed promise gets no mutation. A refile gets no enforced marker.

Before creating tickets fetch marked deltas since the recorded time and call
`retro-notes.mjs recheck <selection.json> <marked.json> <delta.json>`. A failed delta or recheck
stops all creates and note changes. Skip tracked keys. For refiles, call `follow-through-due` on
the union of full and delta marked issues: admit only keys still due for the same issue and round.
A newer promise, enforced marker or waiting successor means skip, never create a duplicate.

When at least one child remains, create one parent using `tracker/cli.mjs create-issue --title
<title> --body-file <parent.md> --state-role unplanned --label-role epic`. Apply each filing label
with `--label` and the optional `--project` to parent and children. An ambiguous create stops;
never retry a create without settling it by reading tracker state.

For ordinary themes obtain the body with `retro-notes.mjs describe <selection.json> <key>
--path-aliases <aliases>`; for refiles use `refile-describe`. Create children with `create-issue
--state-role unplanned --label-role agentPlan --parent <parent-id> --title <title> --body-file <body>`.
Refile bodies carry the same marker, incremented promise round and promised-or-stronger rung.

Preserve each child's source note bodies in a UTF-8 file outside the tree. `sourceNotesTitle` in
`retro-notes.mjs` supplies its exact title. Read `getIssue` attachments before uploading;
a matching title is idempotent. Use `preparePlanAttachment` to obtain the upload URL and headers,
`plan-attachment.mjs put <file> <url> <headers-json-file>` to upload the exact bytes, then
`finalizePlanAttachment`. Re-read the issue to settle a failed or indeterminate finalize.
Retry only a proven absence, once; unreadable state stops. Read the attachment by id through
`readPlanAttachment`; a signed URL is verified using `plan-attachment.mjs verify <file> <url>`.
An unverified attachment retains every associated note.

## 6. Retire and report

Read attachment lists back and call `retro-notes.mjs attachments <attachment-lists.json>`.
Build the deletion ids only from attachment-proven filed themes and already-tracked themes
whose existing source attachments have likewise been verified. A missing attachment keeps notes.
Then call `retire-plan <currency-buckets.json> <selection.json> <delete-ids.json> <staleDays>` →
`retro-run.mjs prune-plan <retire-plan.json>` → `retro-write.mjs retire <pruned-plan.json>
<snapshot-ids> --progress <progress.json>`. Preserve notes on any uncertainty; never invent ids.

After successful retirement, call `guidance-audit.mjs --record --run-id <runId>` with the same
optional config, then `retro-run.mjs record-run --run-id <runId>`. Report recording failures.
Report mode, parent/child ids, helper counts, deferred and unconfirmed work, collector statuses,
ignored selection slots, and whether the rule of the run filed or `stays-rule`. Teardown.

For weekly scheduling and gate behavior read [references/cron-gate.md](references/cron-gate.md).
