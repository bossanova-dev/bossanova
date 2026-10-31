---
name: boss-retro
disable-model-invocation: true
description: Turn improvement notes, human-correction signals and guidance findings into remedy-ladder tickets. Writes by default; --dry-run previews.
---

# boss-retro

Collect → theme → currency and remedy ladder → select → promote → file → retire. Writes by default;
`--dry-run` performs the same judgment and previews tickets without changing notes, tracker issues
or retro state. `--no-dry-run` is a compatibility no-op. Implementation is limited to discovered promotion extensions.

## Contract

One lock, one snapshot, the theming dispatch plus one awaited dispatch per currency shard and,
when due, one follow-through shard; plus one awaited dispatch per discovered `promote` and `maintenance`
extension. Currency shards judge currency, ladder, detector hits and the rule of the run. Maintenance never touches notes,
the snapshot, retro state or the run outcome.
Unreadable notes or tracker state, malformed output, a failed dispatch or an uncertain recheck
stops filing and retirement. No note outside the snapshot may be deleted or retagged. Selection
adds filing labels and one project; it never narrows marker dedupe. Teardown on every locked exit.

## 0. Preflight

Resolve the installed helpers in every shell block; shell state is not shared:

```bash
BOSS_RETRO_TOOLBOX="${BOSS_RETRO_TOOLBOX:-${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-retro/toolbox}"
if [ ! -d "$BOSS_RETRO_TOOLBOX" ]; then BOSS_RETRO_TOOLBOX="$HOME/.codex/skills/boss-retro/toolbox"; fi
```

Create a scratch run directory outside the checkout and set `BOSS_RETRO_RUN_DIR` to it.
The scratch directory holds files only; run every command from the checkout root, and rerun a
helper exiting 3 (`not-a-checkout`) from that root rather than substituting an empty result.
Run
`node "$BOSS_RETRO_TOOLBOX/retro/retro-run.mjs" settings -- <arguments>` and save `settings.json`.
Exit 64 means usage error; stop. Carry `mode`, `runId`, `selectArgs`, `pathAliases`, `upstream`,
`guidanceAuditConfig` and `selectionArgs` from that JSON. Report `ignored` once.

Resolve the notes CLI with `node "$BOSS_RETRO_TOOLBOX/boss-binary.mjs"`; unavailable means stop
with `BLOCKED: needs the notes store`. In write mode, save `tracker/cli.mjs operations` to
`operations.json` and run `retro-run.mjs storage-route <operations.json>`. A non-zero exit stops
with its message. Carry its `route` to phase 5.
When `needsResolution` is true, run `tracker/cli.mjs resolve-selection --stage retro` with only
`settings.selectionArgs`; pass its result through `retro-run.mjs filing`. Do not silently discard
unresolved labels or an ambiguous project. Zero supported slots require no resolution request.

Each capability is an operation in the resolved adapter's map. `tracker/cli.mjs operations` lists
its transport. An `mcp` operation is the named tool, called through `resolvedServer`; a `command`
operation is run with `tracker/cli.mjs invoke-operation <op> --args-file <file>`, whose `result` is
the answer. A descriptor verb (`create-issue`, `update-comment`, `write-description`) that prints
`outcome: operation-executed` has already performed the write: do not make an MCP call for it.

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
Other audit failures are reported and supply an empty audit array. Do not use `--record` yet.

Call `retro-run.mjs augment <notes.json> --audit <audit.json> --signals <signals.json> --run-id <runId>`.
In write mode omit `--signals`: those signals are already in the real notes snapshot. Synthetic
audit and preview-signal ids stay out of snapshot-ids and every retirement mutation.

Record the fetch time. Fetch the entire marked set with
`tracker/cli.mjs fetch-marked --prefix "Notes: " --out-file <marked.json>` in either mode.
Any non-zero result stops safely. Call `retro-notes.mjs follow-through-due <marked.json>`;
its default cap bounds completed promises. Report `problems` and leave them untouched.
If the augmented notes and the due promises are both empty, report nothing to retro, run phase 7,
and teardown.

## 2. Theme — one awaited dispatch

Call `retro-notes.mjs cluster <augmented.json>` and `digest <clusters.json>`. One fresh worker
reads the complete digest, groups on `detail` when present as the note’s specific content rather than
a statement shared across a trigger, checks source records when needed, and writes a complete partition
with titles in `groups.json` compatible with `mergeClusters`. It does not write notes or tickets.
Await its result, then call `merge <clusters.json> <groups.json>`, `rank <merged.json>` and
`stale <ranked.json> --path-aliases <settings.pathAliases>`. An invalid partition stops the run.
Empty clusters still yield an empty partition when follow-through work is due.

For each dispatch, create a `bs-run-sentinel.mjs make-ctx boss-retro` context, seed no successful
verdict, require the worker to heartbeat its context and write a terminal sentinel only after its
output artifact exists. Record the dispatch start timestamp in epoch milliseconds and pass `--dispatched-at <ms>`
to `bs-dispatch-await.mjs wait` and `disposition` with that heartbeat;
only `publishable: true` plus valid output admits the next phase. Clean each context afterward.
A missing, provisional, foreign or abandoned verdict fails safe. Await every worker; never proceed
from a pending dispatch. Use the harness's separate worker context, not separate personas in one chat.

## 3. Currency, ladder and follow-through — awaited shards

Call `retro-notes.mjs shard <ranked.json> <stale.json> <due.json> <shard-dir> [size]` and save
its manifest as `manifest.json`. Size defaults to 50; the positional size overrides
`BOSS_RETRO_SHARD_SIZE`. Call `bs-dispatch-await.mjs batches <manifest.shards-json>` for bounded waves.
Dispatch one fresh worker per shard with only its `input` file, writing its array to `outPath`.
Each worker judges exactly its input's keys against current code and dispatches nothing.

Use the phase 2 sentinel and heartbeat discipline with the shard `id` as the sentinel name and
`--artifact <outPath>`. Await every worker in a wave before starting the next. A shard whose
`disposition` is not publishable may be redispatched once with the same input; a second failure
stops filing and retirement. After all shards are publishable, call
`retro-notes.mjs merge-verdicts <manifest.json> <ranked.json> <due.json> <merged-dir>`.
A non-zero result stops filing and retirement. Preserve its coverage report and use its merged
`verdicts.json` and `follow-through-verdicts.json` below.

For themes, return `{key, verdict, evidence, fixedNotes}` and, for live themes,
`{rung, target, rungEvidence, existingCheck, instances, supersedes}`. Cite or abstain: choose the
strongest feasible rung; rule and lens explain why stronger rungs fail. An existing check means
repair its gap. Confirm prose-pin and guidance-weight hits with source evidence; a false positive
is unverifiable. Judge the quoted rule of the run for promotion or existing enforcement.

For completed promises, use `enforced`, `prose-only` or `regressed`, with evidence of the landed
artifact, `landedRung` and cleanup evidence for the replaced prose instances. A weaker artifact
needs the explicit sourced downgrade supported by the helper; otherwise retain the promised rung.
Abstention is empty evidence, which yields `unconfirmed`, never retirement. Each worker writes no
tracker mutation and marks its sentinel only when its complete shard array exists.

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

Continue through phase 4b in both modes before preview or filing.

## 4b. Promote

Write `classes.json` with `retro-classes.mjs classify <final-selection.json> <currency-buckets.json>`
using `--min-runs <settings.minRuns>` when set. Discover `promote` extensions with
`skill-extensions.mjs discover --core boss-retro --role promote --mode <headless|interactive> --json`.
In `(order, name)` order, dispatch each descriptor's `skillPath` as one fresh awaited worker under
phase 2's sentinel and heartbeat discipline and `BOSS_SKILL_EXTENSION_TIMEOUT_MS` bound, with
`{role: "promote", core: "boss-retro", context: {mode, runId, classesPath}, runTmp, outPath}`.
Validate each output with `skill-extensions.mjs validate --role promote --file <outPath>`;
failed discovery, dispatch or validation is a reported non-fatal skip and leaves filing intact.
Keep valid outputs as an array in `promote-results.json`. Preview extensions write no repository,
tracker or state changes, and preview claims never suppress filing.

In write mode call `retro-run.mjs verify-promotions <classes.json> <promote-results.json>`;
save its result inside an array as `verified-promotions.json`. It independently reads each claimed
PR with `gh pr view`: only OPEN or MERGED with an exact class `Notes: <key>` line qualifies.
Unverifiable or unknown claims keep their filing path. Validated `tracked` classifications name
registered coverage without a PR claim; an existing-check gap still files for repair.
Re-run `retro-run.mjs finalize <selection.json> <snapshot-ids> <augmented.json>
--classes <classes.json> --promotions <promote-results.json>` into `final-selection.json`.
Call `retro-run.mjs route <final-selection.json> --upstream '<settings.upstream JSON>'
--path-aliases '<settings.pathAliases JSON>'` into `remedy-route.json`; use its `selection` as `selection.json` for filing.
Promoted and covered classes keep their source notes until normal attachment-proven retirement.
Write `class-report.json` with `retro-classes.mjs report <classes.json>
--promotions <verified-promotions.json>` (an empty result array in dry-run); print this report
with its class counts in the preview and phase 6 report.

In dry-run mode, print `retro-run.mjs preview <remedy-route.json> --path-aliases <aliases>` including `forward` and `flagged`.
For each admitted refile, print `retro-notes.mjs refile-describe <follow-through.json> <key>`.
Report enforced/unconfirmed follow-through without updating markers. Teardown; no attachment,
recording, note mutation or tracker write. Preview includes rendered `## Codify as` and `## Cleanup`.
Run phase 7 in `dry-run` mode before teardown.

## 5. File and follow through — write mode

Use `tracker/cli.mjs operations` to resolve each operation's transport. For enforced promises only,
read the issue's current full description through `getIssue`,
apply `retro-notes.mjs mark-enforced <description-file> <key> <ISO-time>`, and update that exact
issue through the adapter's `writeDescription` operation via
`tracker/cli.mjs write-description --id <issue-id> --body-file <updated-description>`.
For `outcome: descriptor-emitted`, execute the emitted `{tool, args}` through `resolvedServer`;
for `outcome: operation-executed`, use `result` without making another write.
Re-read to verify the marker for either transport.
Never replace a peer's changed description: re-read immediately before updating and recompute.
An unconfirmed promise gets no mutation. A refile gets no enforced marker.

Before creating tickets fetch marked deltas since the recorded time and call
`retro-notes.mjs recheck <selection.json> <marked.json> <delta.json>`. A failed delta or recheck
stops all creates and note changes. Skip tracked keys. For refiles, call `follow-through-due` on
the union of full and delta marked issues: admit only keys still due for the same issue and round.
A newer promise, enforced marker or waiting successor means skip, never create a duplicate.

Call `retro-run.mjs forward <remedy-route.json> --repo <settings.upstream.notesRepo>` only when
`forward` is nonempty; save `forward-result.json`. Failed or unproven themes retain all notes and file nothing locally.

When at least one child remains, create one parent using `tracker/cli.mjs create-issue --title
<title> --body-file <parent.md> --state-role unplanned --label-role epic`. Apply each filing label
with `--label` and the optional `--project` to parent and children. An ambiguous create stops;
never retry a create without settling it by reading tracker state.

For ordinary themes obtain the body with `retro-notes.mjs describe <selection.json> <key>
--path-aliases <aliases>`; for refiles use `refile-describe`. Create children with `create-issue
--state-role unplanned --label-role agentPlan --parent <parent-id> --title <title> --body-file <body>`.
For each parent or child create, `outcome: descriptor-emitted` requires executing the emitted
`{tool, args}` through `resolvedServer`; `outcome: operation-executed` supplies the created issue in
`result` and needs no further create.
Refile bodies carry the same marker, incremented promise round and promised-or-stronger rung.

Preserve each child's source note bodies in a UTF-8 file outside the tree. `sourceNotesTitle` in
`retro-notes.mjs` supplies its exact title. For `single-record`, read `getIssue` attachments first;
a matching `Source notes (<id>)` title is idempotent only after byte verification. Otherwise run
`tracker/cli.mjs invoke-operation writeSourceNotes --args-file <f>` with
`{issue, file, title: <sourceNotesTitle(id)>}`. Verify with `readPlanAttachment`
`{id: result.id, outFile}` and compare bytes against the local notes file. Settle a non-zero or
indeterminate write by re-reading `getIssue`, never by blind retry. Different bytes, duplicate
records, oversize or unreadable state retain every associated note.

For `attachment`, read `getIssue` attachments before uploading;
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
Union `retro-run.mjs forward-delete-ids <remedy-route.json> <forward-result.json>` into `delete-ids.json`
when forwarding ran: only fully proven themes qualify; `prune-plan` still filters synthetic ids.
Then call `retire-plan <currency-buckets.json> <selection.json> <delete-ids.json> <staleDays>` →
`retro-run.mjs prune-plan <retire-plan.json>` → `retro-write.mjs retire <pruned-plan.json>
<snapshot-ids> --progress <progress.json>`. Preserve notes on any uncertainty; never invent ids.

After successful retirement, call `guidance-audit.mjs --record --run-id <runId>` with the same
optional config, then `retro-run.mjs record-run --run-id <runId> --classes <class-report.json>`. Report recording failures.
Report forwarded and failed themes from `forward-result.json` when present, flagged counts from `remedy-route.json`,
and the class report (also persisted as state `lastReport`), mode, parent/child ids, helper counts, the `merge-verdicts` coverage object, deferred and
unconfirmed work, collector statuses,
ignored selection slots, and whether the rule of the run filed or `stays-rule`. Run phase 7 in
`write` mode, then teardown.

## 7. Maintenance extensions

Runs after the phase 1 empty exit, the phase 4 preview and the phase 6 report — never after a run
that stopped safely on an error, where it is reported as skipped. Discover with
`node "$BOSS_RETRO_TOOLBOX/skill-extensions.mjs" discover --core boss-retro --role maintenance --mode <headless|interactive> --json`
(`headless` when `BOSS_UNATTENDED` or `BOSS_CRON` is set). For each descriptor in `(order, name)`
order, dispatch one fresh awaited worker loaded from its `skillPath` (never by skill name), under
the phase 2 sentinel and heartbeat discipline and bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`
(default 300000 ms), with
`{role: "maintenance", core: "boss-retro", context: {mode: "write"|"dry-run", runId}, runTmp, outPath}`.
Keep the lock held and touched while awaiting. Validate with
`skill-extensions.mjs validate --role maintenance --file <outPath>` and report each extension's
`outcome`, `summary` and `issues`. Discovery failure, timeout, missing output or failed validation
is a reported, non-fatal skip. In `dry-run` mode an extension writes nothing to the tracker.

For weekly scheduling and gate behavior read [references/cron-gate.md](references/cron-gate.md).
