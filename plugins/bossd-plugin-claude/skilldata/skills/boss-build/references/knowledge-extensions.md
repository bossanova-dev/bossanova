# Pre-PR knowledge extensions (repo opt-in)

The `knowledge` phase runs **between Step 6 and Step 7**: after the review stack returned `clean` and
its fixes are committed, and **before** Step 7 pushes and captures the reviewed tip. A `knowledge`
extension records what the run learned — terms, concepts, durable solutions — as a committed artifact
so it ships **inside** the PR.

## Ordering: why this phase is pre-PR, and why the tip capture follows it

- **Step 7 captures the reviewed tip AFTER this phase returns**, never before it.
- **A `knowledge` extension may write only artifact paths** — its repo-local skill declares the exact
  boundary (in this repo: `docs/solutions/**` and `CONCEPTS.md`). No source, tests, config, or build
  files.
- If an extension is found to have written outside its declared boundary, treat the phase as failed —
  append its skip line, **revert its commits locally**, and do **not** widen the boundary to
  accommodate it.

### Enforce the boundary locally — Step 7 cannot do it for you

**Step 7's reviewed-tip confirmation cannot substitute for checking this boundary**: this phase's own
commits are local and are exactly what the capture is taken _after_, so they pass unexamined. Verify
the paths yourself, around each dispatch:

```bash
KNOWLEDGE_PRE_SHA="$(git rev-parse HEAD)"   # re-take per dispatch, so an earlier legitimate commit survives
# … dispatch one extension, awaited …
KNOWLEDGE_POST_SHA="$(git rev-parse HEAD)"
KNOWLEDGE_BOUNDARY_RE='^(<dir>/.+\.md|<file>\.md)$'   # anchored ERE from the declared paths, NOT a glob
CHANGED="$(git diff --name-only "$KNOWLEDGE_PRE_SHA" "$KNOWLEDGE_POST_SHA")"
OUT_OF_BOUNDS="$(printf '%s\n' "$CHANGED" | grep -Ev "$KNOWLEDGE_BOUNDARY_RE")"
case $? in 0 | 1) ;; *) OUT_OF_BOUNDS="$CHANGED" ;; esac   # grep 2 = bad ERE: fail closed
```

Translate the extension's declared boundary into an **anchored** ERE (`^…$`) yourself; a glob copied
verbatim is not one. Never mask `grep`'s status with `|| true`: the `case` above must see exit `2` to
fail closed.

A non-empty `OUT_OF_BOUNDS`, **or a dirty worktree after the dispatch**, is a failed phase: append
`extension <name>: skipped (wrote outside its declared boundary: <first paths>)`, restore the tree
with `git reset --hard "$KNOWLEDGE_PRE_SHA"` (the pre-dispatch tip is by definition the reviewed,
committed state), and continue to the next descriptor. Reverting is safe precisely because the
boundary forbids source, tests, config, and build files, so nothing another step depends on is lost.

### Never commit secrets — this artifact is public

The Step 12 `notes` phase already forbids copying transcripts, command output, user-provided content,
credentials, tokens, or other secrets into what it persists, and caps its handoff at 8 KiB. Every one
of those rules applies here **more** strongly, not less. An extension must scrub what it writes to the
same standard, and an artifact that cannot be scrubbed must not be written at all — an empty `items`
array is a legitimate success.

## Sampling gate (one roll per run, shared with the Step 12 notes phase)

Reporting is sampled, not unconditional. `notesDefaults.sampleRate` (a number in `[0,1]`, default
`1.0`; `0.33` is the recommended production setting) is the probability that a run reports at all,
and **one roll governs both reporting phases of the run** — this pre-PR `knowledge` phase and the
post-terminal `notes` phase. Whichever phase runs first takes the roll; this one runs first whenever
it runs at all, so take it here and hand the pair forward to Step 12 rather than re-rolling there.
Hand it forward **through a file** inside the git directory, not through the environment. Like the
budget gate, the roll is checked **before** discovery, so a sampled-out run creates no scratch and
prints no discovery output:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
if [ -z "${NOTES_SAMPLED:-}" ]; then
  NOTES_SAMPLE_RATE=$(export BOSS_BUILD_TOOLBOX; node --input-type=module -e 'import { pathToFileURL } from "node:url"; const { loadSkillConfig, notesSampleRate } = await import(pathToFileURL(process.env.BOSS_BUILD_TOOLBOX + "/skill-config.mjs").href); process.stdout.write(String(notesSampleRate(loadSkillConfig())))')
  NOTES_SAMPLED=$(awk -v r="${NOTES_SAMPLE_RATE:-1}" -v s="$$" 'BEGIN{srand(s);print (rand()<r)?"yes":"no"}')
  export NOTES_SAMPLE_RATE NOTES_SAMPLED
fi
NOTES_ROLL_FILE="$(git rev-parse --git-dir 2>/dev/null || echo .)/boss-build-notes-roll"
printf '%s %s\n' "${NOTES_SAMPLE_RATE:-1}" "$NOTES_SAMPLED" > "$NOTES_ROLL_FILE"
```

Write the file on **both** branches of the roll, which is why the `printf` sits outside the `if`; it
overwrites unconditionally, so a file left by an earlier run is replaced rather than read.

- `NOTES_SAMPLED` is not `yes` → **skip the entire phase**. Append
  `reporting: sampled out (rate <r>)` and go straight to Step 7. Do not discover, do not create
  scratch, do not dispatch. Step 12 reuses the same `no` and prints its own
  `notes: sampled out (rate <r>)` line there, so a sampled-out run pays for neither reporting phase.

  That line is deliberately **run-level rather than a `knowledge phase: skipped` claim**. This gate
  fires before discovery, so it cannot yet know whether this repo configures a knowledge extension at
  all — and most repos configure none. A per-phase claim would tell a repo that never opted in that
  something of its own had been dropped, contradicting the resident body's
  `No extensions → do nothing, print nothing`.

- Otherwise continue to the budget gate below. A sampled-in run is bounded exactly as it was before.

An unreadable or malformed _rate_ resolves to `1.0` at both ends — the accessor's own fallback and the
`${NOTES_SAMPLE_RATE:-1}` default — so a broken config costs a few extra dispatches and never silently
switches reporting off for a repository that never asked for that. A corrupt _verdict_ resolves the
other way: a carried second field that is neither empty nor `yes` is not `yes`, so the run samples
out. Like the budget ceiling, the sampling gate is a skip line and never a terminal state.

## Budget gate (check this before discovering)

```bash
KNOWLEDGE_BUDGET_MINUTES=15    # aggregate ceiling for this phase across ALL extensions
KNOWLEDGE_PHASE_STARTED_AT="$(date +%s)"
```

- Track the phase's own elapsed time from `KNOWLEDGE_PHASE_STARTED_AT` and stop dispatching once it
  reaches `KNOWLEDGE_BUDGET_MINUTES`, appending `extension <name>: skipped (phase budget exhausted)`
  for each descriptor left undispatched. Clamp each individual dispatch to the smaller of
  `BOSS_SKILL_EXTENSION_TIMEOUT_MS` and the phase budget still unspent, so the last extension cannot
  overrun the ceiling on its own.
- The ceiling measures **this phase's own spend and nothing else**. It reads no run clock.

The ceiling is a skip line, never a terminal state. The sampling gate above is the other such skip
line, and it is checked before it.

## Discovery

Resolve the extension helper, then discover the `knowledge` role:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
KNOWLEDGE_JSON=$(node "$BOSS_BUILD_TOOLBOX/skill-extensions.mjs" discover --core boss-build --role knowledge --json)
```

Record every `KNOWLEDGE_JSON.skipped` entry whose `deliberate` is `false` as
`extension <name>: skipped (<reason>)` in the ledger, before dispatching. Key that on the entry's
own `deliberate` field, never on the text of `reason`. A `deliberate: true` entry is a same-prefix
skill that is not an extension of this core, and is never reported. Recording is all that is due.
That is distinct from an empty `extensions` list, which is reported as nothing at all:

If `KNOWLEDGE_JSON.extensions` is empty, **do nothing and print nothing** — a repo without a local
knowledge extension has not opted in, and an empty discovery is not a skip worth reporting. **Create
no scratch directory in that case**; proceed directly to Step 7.

## Dispatch

Otherwise create the run scratch:

```bash
KNOWLEDGE_RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-build-knowledge.XXXXXX")
```

Dispatch descriptors in ascending `(order, name)` order as fresh, **awaited** subagents. Bound each by
`BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms). Load each extension by **reading the
descriptor's `skillPath` from disk** (`dir` is its directory), passing both `skillPath` and `dir` in
the worker brief, and requiring relative extension resources to resolve from `dir`. Pass that
`SKILL.md` content into the dispatch as the extension's instructions — never by its bare descriptor
`name` via the Skill tool, which refuses a skill declaring `disable-model-invocation: true`.
Each receives:

```json
{
  "role": "knowledge",
  "core": "boss-build",
  "context": {
    "mode": "<interactive if this run involved operator interaction; otherwise headless>",
    "core": "boss-build",
    "planPath": "<plan doc path when this run had one; otherwise null>",
    "mergeBase": "<REVIEW_BASE>",
    "head": "<current HEAD after Step 6 fixes are committed>",
    "carriedObservations": [
      {
        "round": 2,
        "category": "false-universal",
        "paragraph": "Within-run observation from round 2: …"
      }
    ],
    "repoId": "<BOSS_REPO_ID when present; otherwise null>"
  },
  "runTmp": "<KNOWLEDGE_RUN_TMP>",
  "outPath": "<KNOWLEDGE_RUN_TMP>/knowledge-<extension-name>.json"
}
```

There is deliberately **no `outcome` key**: this phase runs pre-PR, so no terminal outcome exists yet.
`carriedObservations` is the distilled within-run trail from the review loop. It is input to the
knowledge extension's overlap/synthesis work only; this phase still owns any durable `docs/` or
`CONCEPTS.md` write, and the mid-run observation step never writes those paths.

## Validation and the ledger

Validate each result with:

```bash
node "$BOSS_BUILD_TOOLBOX/skill-extensions.mjs" validate --role knowledge --file "<outPath>"
```

It exits `0` when valid and `1` when not, and never throws. Each accepted item is
`{ path, title, kind }`, where `path` is repo-relative and is the proof the artifact reached the tree.
An `{"ok": false, …}` envelope is itself **invalid**: the extension's own `error` text becomes the skip
reason rather than its empty `items` being folded in as accepted results.

An accepted envelope with an **empty `items` array is a success**, not a skip: a run that produced
nothing worth recording legitimately records nothing. Report it as a zero-count success.

On success append one ledger line with the total captured-artifact count. On **any** failure —
discovery skip, timeout, missing output, malformed envelope, validation failure, or subagent failure —
append:

```text
extension <name>: skipped (<reason>)
```

and continue to the next descriptor. Remove `KNOWLEDGE_RUN_TMP` on every post-opt-in path out of this
phase.

## Non-fatal in every case

This phase **can never produce `BLOCKED`** and never changes the run's terminal state, exit code,
tracker writes, or PR body. Every failure mode above is a skip line and nothing more. A run whose
knowledge phase failed entirely still proceeds to Step 7 exactly as a run that never opted in.
