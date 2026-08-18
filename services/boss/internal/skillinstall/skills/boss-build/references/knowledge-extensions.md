# Pre-PR knowledge extensions (repo opt-in)

The `knowledge` phase runs **between Step 6 and Step 7**: after the review stack returned `clean` and
its fixes are committed, and **before** Step 7 pushes and captures the reviewed tip. A `knowledge`
extension records what the run learned — terms, concepts, durable solutions — as a committed artifact
so it ships **inside** the PR.

The core knows the **role**, never a particular knowledge methodology. Which skill implements capture
is the repo-local extension's business; a repo with no such extension has not opted in.

## Ordering: why this phase is pre-PR, and why the tip capture follows it

This phase **commits to the session branch**, so it necessarily moves `HEAD` after review. That is the
exact hazard Step 7's reviewed-tip confirmation exists to catch, and the ordering is what makes it
safe — state it explicitly, because a later edit that moves this phase will silently reintroduce the
hazard:

- **Step 7 captures the reviewed tip AFTER this phase returns**, never before it. Capturing first
  would make this phase's own commit read as an unreviewed third-party advance and fail the run
  `BLOCKED`.
- **A `knowledge` extension may write only artifact paths** — its repo-local skill declares the exact
  boundary (in this repo: `docs/solutions/**` and `CONCEPTS.md`). No source, tests, config, or build
  files. That restriction is what lets the tip capture follow the phase without weakening it: no code
  path reaches the PR unreviewed.
- If an extension is found to have written outside its declared boundary, treat the phase as failed —
  append its skip line, **revert its commits locally**, and do **not** widen the boundary to
  accommodate it.

The alternative placement — post-terminal, alongside the Step 12 `notes` phase — is structurally safer
but useless here: Step 12 runs after the push and forbids changing the worktree, index or `HEAD`, so a
document written there would land in a tree nobody merges.

### Enforce the boundary locally — Step 7 cannot do it for you

The boundary above is a rule the extension is asked to follow; on its own it is only self-discipline,
and **Step 7's reviewed-tip confirmation cannot substitute for checking it**. That confirmation
compares the captured reviewed tip against the **remote** tip to catch a third party advancing the
branch; this phase's own commits are local and are exactly what the capture is taken _after_, so they
match by construction and pass unexamined. Verify the paths yourself, around each dispatch:

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
verbatim is not one, and an unanchored fragment lets `vendor/docs/solutions-shim.go` read as in-bounds
— the boundary check would then return clean in exactly the sloppy-author case it exists to catch.
Never mask `grep`'s status with `|| true` either: exit `2` (a malformed ERE) is indistinguishable from
exit `1` (nothing out of bounds), so a typo in the pattern would silently pass everything. The `case`
above treats it as "everything is out of bounds" instead.

A non-empty `OUT_OF_BOUNDS`, **or a dirty worktree after the dispatch**, is a failed phase: append
`extension <name>: skipped (wrote outside its declared boundary: <first paths>)`, restore the tree
with `git reset --hard "$KNOWLEDGE_PRE_SHA"` (the pre-dispatch tip is by definition the reviewed,
committed state), and continue to the next descriptor. Reverting is safe precisely because the
boundary forbids source, tests, config, and build files, so nothing another step depends on is lost.

### Never commit secrets — this artifact is public

The Step 12 `notes` phase already forbids copying transcripts, command output, user-provided content,
credentials, tokens, or other secrets into what it persists, and caps its handoff at 8 KiB. Every one
of those rules applies here **more** strongly, not less: `notes` writes to a private external store,
while a `knowledge` artifact is committed to the branch, pushed, and published in the pull request,
where it cannot be unpublished. An extension must scrub what it writes to the same standard, and an
artifact that cannot be scrubbed must not be written at all — an empty `items` array is a legitimate
success.

## Budget gate (check this before discovering)

Unlike the Step 12 `notes` phase, this one runs **before** the terminal outcome, so time it spends is
time Step 7 onward no longer has. `BOSS_SKILL_EXTENSION_TIMEOUT_MS` bounds a _single_ dispatch and
nothing bounds their sum, so N extensions — or one that sits at its timeout — could consume the
remaining wall clock and trip the breaker, producing the very `BLOCKED` this phase promises never to
cause. Two bounds close that, and the deadline is checked **before** discovery so a tight run creates
no scratch and prints no discovery output at all:

```bash
KNOWLEDGE_RESERVE_MINUTES=25   # Step 7 onward (push, PR gate, CI wait, finalize) must keep this
KNOWLEDGE_BUDGET_MINUTES=15    # aggregate ceiling for this phase across ALL extensions
# Empty deadline means NO cap, not zero — bare $PREFLIGHT_DEADLINE would read unset as 0 and make
# KNOWLEDGE_REMAINING hugely negative, silently self-disabling the phase forever.
if [ -n "${PREFLIGHT_DEADLINE:-}" ]; then
  KNOWLEDGE_REMAINING=$(( (PREFLIGHT_DEADLINE - $(date +%s)) / 60 ))
else
  KNOWLEDGE_REMAINING=$(( KNOWLEDGE_RESERVE_MINUTES + KNOWLEDGE_BUDGET_MINUTES ))
fi
```

- `KNOWLEDGE_REMAINING < KNOWLEDGE_RESERVE_MINUTES + KNOWLEDGE_BUDGET_MINUTES` → **skip the entire
  phase**. Append `knowledge phase: skipped (insufficient remaining wall clock)` and go straight to
  Step 7. Do not discover, do not create scratch.
- Otherwise track the phase's own elapsed time and stop dispatching once it reaches
  `KNOWLEDGE_BUDGET_MINUTES`, appending `extension <name>: skipped (phase budget exhausted)` for each
  descriptor left undispatched. Clamp each individual dispatch to the smaller of
  `BOSS_SKILL_EXTENSION_TIMEOUT_MS` and the phase budget still unspent, so the last extension cannot
  overrun the ceiling on its own.

Both bounds are skip lines, never a terminal state — that is what makes the non-fatal guarantee below
true of the wall clock and not merely of the failure modes.

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
skill that is not an extension of this core — a markerless helper, or one extending another core —
and is never reported. Recording is all that is due: a discovery skip is never fatal and never
changes control flow; the phase still degrades exactly as documented below. That is distinct from
an empty `extensions` list, which is reported as nothing at all:

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
    "repoId": "<BOSS_REPO_ID when present; otherwise null>"
  },
  "runTmp": "<KNOWLEDGE_RUN_TMP>",
  "outPath": "<KNOWLEDGE_RUN_TMP>/knowledge-<extension-name>.json"
}
```

There is deliberately **no `outcome` key**: this phase runs pre-PR, so no terminal outcome exists yet.
`mergeBase` and `head` bound what the run actually changed, which is the material a capture pass reads.

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

That guarantee rests on three bounds, all of them above — take none of them as decorative:

1. **Time** — the budget gate skips the phase outright when the reserve is not there, and caps the
   aggregate spend when it is, so the phase cannot starve Step 7 into a wall-clock `BLOCKED`.
2. **Paths** — the local boundary check reverts an out-of-bounds dispatch to the reviewed tip, so the
   phase cannot smuggle unreviewed code into the PR and cannot fail Step 7's tip confirmation.
3. **Failure handling** — every discovery, dispatch, validation, and timeout failure is an appended
   skip line, never a raised error.

Weaken any one of them and the heading stops being true. If a future change cannot preserve all
three, change this heading rather than leave a guarantee the mechanism no longer provides.
