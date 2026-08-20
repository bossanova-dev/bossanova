// Executable fixtures for the boss-plan Compound Engineering draft extension (BOS-813).
//
// The sibling scripts/bs-plan-skill.test.mjs pins the *prose* of the planning surfaces. This file
// pins the two behaviours BOS-813's Required proof names — attachment gating and scratch cleanup —
// by running them, in both the interactive and headless shapes, against fixtures on disk.
//
// Nothing here hand-copies the skill's contract. The envelopes come out of the extension's own
// SKILL.md and the staging recipe is extracted from the same file and executed verbatim, so a doc
// that drifts from these fixtures reds this test rather than silently shipping.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPO_ROOT = fileURLToPath(new URL('..', import.meta.url))
const DRAFT_NAME = 'boss-plan-compound-engineering'
const DRAFT_SKILL = join(REPO_ROOT, '.claude', 'skills', DRAFT_NAME, 'SKILL.md')
const FIXTURES = join(REPO_ROOT, 'scripts', 'testdata', 'boss-plan-ce')

const draftBody = () => readFileSync(DRAFT_SKILL, 'utf8')
const fixture = (name) => JSON.parse(readFileSync(join(FIXTURES, `${name}.json`), 'utf8'))

/** Every fenced block of `lang` in `body`, in document order, dedented. Fences nested inside a
 *  numbered list item are indented, so a column-0 anchor would silently miss them — and a block
 *  this file cannot see is a block it cannot gate. */
function fencedBlocks(body, lang) {
  const blocks = []
  const fence = new RegExp('^([ \\t]*)```' + lang + '\\n([\\s\\S]*?)^\\1```', 'gm')
  for (const [, indent, block] of body.matchAll(fence)) {
    blocks.push(indent ? block.replace(new RegExp('^' + indent, 'gm'), '') : block)
  }
  return blocks
}

/** The extension's two executable staging blocks, selected by their `boss-plan-ce:` label rather
 *  than by position — the snapshot is worthless without the cleanup that consumes it, so both must
 *  be present and distinguishable. */
function stagingBlocks(body) {
  const blocks = fencedBlocks(body, 'bash')
  assert.ok(blocks.length >= 2, 'the extension must document a staging snapshot AND its cleanup')
  const snapshot = blocks.filter((b) => /^# boss-plan-ce: staging[ ]snapshot$/m.test(b))
  const cleanup = blocks.filter((b) => /^# boss-plan-ce: staging[ ]cleanup$/m.test(b))
  assert.equal(snapshot.length, 1, 'exactly one labelled staging snapshot block')
  assert.equal(cleanup.length, 1, 'exactly one labelled staging cleanup block')
  return [snapshot[0], cleanup[0]]
}

/** The core's draft-success predicate, as references/interactive-mode.md defines it: a valid
 *  envelope AND a non-empty plan at the `planPath` this dispatch was handed. Both conjuncts, or the
 *  dispatch is a skip — which is exactly what keeps the core off the tracker write. */
function draftSucceeded(envelope, dispatchPlanPath) {
  const envelopeValid =
    !!envelope &&
    typeof envelope === 'object' &&
    envelope.ok === true &&
    envelope.role === 'draft' &&
    typeof envelope.extension === 'string' &&
    envelope.extension !== '' &&
    envelope.planPath === dispatchPlanPath
  if (!envelopeValid) return false
  if (!existsSync(dispatchPlanPath)) return false
  return readFileSync(dispatchPlanPath, 'utf8').trim() !== ''
}

const withTmp = (fn) => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-plan-ce-'))
  try {
    return fn(dir)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

// ---------------------------------------------------------------------------------------------
// Drift gate: the fixtures ARE the extension's documented envelopes.
// ---------------------------------------------------------------------------------------------

test('BOS-813 fixtures: each envelope fixture matches the envelope the SKILL.md documents', () => {
  const documented = fencedBlocks(draftBody(), 'json').map((block) => JSON.parse(block))
  assert.equal(
    documented.length,
    2,
    'the extension must document exactly two envelopes: the failure envelope and the success envelope',
  )
  const byOk = new Map(documented.map((env) => [env.ok, env]))
  assert.ok(
    byOk.has(true) && byOk.has(false),
    'one ok:true and one ok:false envelope must be documented',
  )

  // The success envelope's planPath is the `<context.planPath>` the core passed; the fixture pins a
  // token in its place. Everything else must agree key-for-key.
  const success = fixture('envelope-interactive-success')
  assert.deepEqual(
    { ...byOk.get(true), planPath: success.planPath },
    success,
    'envelope-interactive-success.json has drifted from the Result Envelope block in the SKILL.md',
  )
  assert.equal(
    byOk.get(true).planPath,
    '<context.planPath>',
    'the documented success envelope must return the planPath the core passed, not a path of its own',
  )
  assert.deepEqual(
    byOk.get(false),
    fixture('envelope-headless-ce-unavailable'),
    'envelope-headless-ce-unavailable.json has drifted from the Unavailable CE block in the SKILL.md',
  )
})

// ---------------------------------------------------------------------------------------------
// Attachment gating: the tracker write happens only behind a satisfied draft-success predicate.
// ---------------------------------------------------------------------------------------------

test('BOS-813 fixtures: interactive success gates the tracker attachment open, and only then', () => {
  withTmp((dir) => {
    const planPath = join(dir, 'BOS-000-slug.md')
    const envelope = { ...fixture('envelope-interactive-success'), planPath }

    // (a) valid envelope, no plan written at all — CE returned but produced nothing.
    assert.equal(
      draftSucceeded(envelope, planPath),
      false,
      'a valid envelope with no plan is not a success',
    )

    // (b) valid envelope, plan present but empty — a created-then-unwritten target.
    writeFileSync(planPath, '   \n\n')
    assert.equal(
      draftSucceeded(envelope, planPath),
      false,
      'a valid envelope with an empty plan is not a success',
    )

    // (c) valid envelope AND a non-empty plan at the dispatch's own path — the only success.
    writeFileSync(planPath, '## Summary\n\nA plan body.\n')
    assert.equal(
      draftSucceeded(envelope, planPath),
      true,
      'both conjuncts hold, so the dispatch succeeded',
    )

    // (d) attribution: a plan at a *sibling's* path never credits this dispatch, even though the
    // bytes on disk are identical. This is the false success the per-dispatch target exists to stop.
    const siblingPath = join(dir, 'sibling', 'BOS-000-slug.md')
    mkdirSync(join(dir, 'sibling'))
    writeFileSync(siblingPath, '## Summary\n\nA plan body.\n')
    assert.equal(
      draftSucceeded(envelope, siblingPath),
      false,
      'an envelope naming another dispatch’s planPath must not pass the predicate',
    )
  })
})

test('BOS-813 fixtures: headless CE-unavailable gates the tracker attachment shut', () => {
  withTmp((dir) => {
    const planPath = join(dir, 'BOS-000-slug.md')
    const envelope = fixture('envelope-headless-ce-unavailable')

    assert.equal(
      draftSucceeded(envelope, planPath),
      false,
      'the failure envelope must never satisfy the draft-success predicate',
    )
    // Even if a stray non-empty plan is sitting at the target, ok:false still gates the write shut:
    // the envelope, not the filesystem, decides whether this extension produced anything.
    writeFileSync(planPath, '## Summary\n\nLeft over from something else.\n')
    assert.equal(
      draftSucceeded(envelope, planPath),
      false,
      'a stray file at planPath must not rescue an ok:false dispatch into a tracker write',
    )

    // The extension must send the core down its own fallback tiers rather than improvising a
    // replacement drafting layer — that fall-through is what makes the shut gate safe.
    const body = draftBody()
    assert.match(
      body,
      /records\s+this\s+dispatch\s+as\s+skipped/i,
      'the failure path must be recorded as a skip',
    )
    assert.match(
      body,
      /Tier\s+2[\s\S]{0,80}Tier\s+3/,
      'the failure path must fall through to the core tiers',
    )
  })
})

// ---------------------------------------------------------------------------------------------
// Scratch cleanup: the SKILL.md's own staging recipe, executed.
// ---------------------------------------------------------------------------------------------

const git = (cwd, ...args) =>
  execFileSync(
    'git',
    ['-c', 'user.name=t', '-c', 'user.email=t@t', '-c', 'commit.gpgsign=false', ...args],
    {
      cwd,
      encoding: 'utf8',
    },
  )

test('BOS-813 fixtures: the documented staging recipe enumerates exactly what CE added', () => {
  withTmp((dir) => {
    const repo = join(dir, 'repo')
    const runTmp = join(dir, 'runtmp')
    mkdirSync(repo)
    mkdirSync(runTmp)
    git(repo, 'init', '-q')
    mkdirSync(join(repo, 'docs', 'plans'), { recursive: true })
    writeFileSync(join(repo, 'CONCEPTS.md'), 'original concepts\n')
    writeFileSync(join(repo, 'README.md'), 'committed readme\n')
    writeFileSync(join(repo, 'RENAMEME.md'), 'about to be renamed\n')
    writeFileSync(join(repo, 'STAGEME.md'), 'committed stageme\n')
    writeFileSync(join(repo, 'docs', 'plans', 'pre-existing-plan.md'), 'an older plan\n')
    git(repo, 'add', '-A')
    git(repo, 'commit', '-qm', 'seed')

    // Files the worktree was ALREADY carrying dirty before CE ran. The snapshot exists so these
    // survive cleanup; a recipe that dropped the byte copies would sweep them away with CE's
    // artifacts. The last three are the shapes `git status --porcelain` is not line-safe for: a
    // quoted path, a rename, and a file under a directory porcelain collapses to one `?? dir/`
    // entry unless asked for --untracked-files=all. A path the snapshot misses is a CE write that
    // survives cleanup SILENTLY, which is the exact outcome this recipe exists to prevent.
    writeFileSync(join(repo, 'CONCEPTS.md'), 'original concepts\nwork in progress\n')
    writeFileSync(join(repo, 'spaced name.md'), 'dirty with a space\n')
    git(repo, 'mv', 'RENAMEME.md', 'renamed.md')
    mkdirSync(join(repo, 'userdir'))
    writeFileSync(join(repo, 'userdir', 'u.md'), 'the user own untracked file\n')

    // The pre-run oracle. Captured here rather than read out of the staging dir: the recipe's own
    // snapshot is NUL-delimited (`before.z`) precisely because the line-based form is lossy, so a
    // test that reused it as its equality oracle would be checking the recipe against itself.
    const before = git(repo, 'status', '--porcelain')

    // A file that was CLEAN before the run and that CE modifies. Its pre-run content is the
    // committed content, so cleanup must reach it through a different route than CONCEPTS.md.
    const wasClean = join(repo, 'README.md')

    // Run the extension's own staging block, verbatim apart from binding $RUN_TMP. Both blocks are
    // extracted from the doc and executed; nothing below reimplements what the doc says to do.
    const [snapshot, cleanup] = stagingBlocks(draftBody())
    execFileSync('bash', ['-euo', 'pipefail', '-c', snapshot], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    const stage = join(runTmp, 'ce-stage')
    const plansBefore = readFileSync(join(stage, 'plans-before.txt'), 'utf8')
      .split('\n')
      .filter(Boolean)
    for (const rel of ['CONCEPTS.md', 'spaced name.md', 'renamed.md', 'userdir/u.md']) {
      assert.equal(
        readFileSync(join(stage, 'before', rel), 'utf8'),
        readFileSync(join(repo, rel), 'utf8'),
        `the pre-run snapshot must carry the BYTES of ${rel}, not just its status`,
      )
    }
    assert.deepEqual(
      plansBefore,
      ['pre-existing-plan.md'],
      'the pre-run plans snapshot must list what was there',
    )

    // CE runs: it writes its own plan and gap-fills two unrelated tracked files — one the worktree
    // was already carrying dirty, one that was clean.
    const cePlan = join(repo, 'docs', 'plans', '2026-08-09-001-feature-thing-plan.md')
    writeFileSync(cePlan, "CE's own plan\n")
    writeFileSync(join(repo, 'CONCEPTS.md'), 'original concepts\nwork in progress\nCE gap-fill\n')
    writeFileSync(wasClean, 'committed readme\nCE gap-fill\n')
    writeFileSync(join(runTmp, 'ce-notes.md'), 'scratch CE wrote inside runTmp\n')
    // CE runs `git add`. This is the shape `git checkout --` cannot undo: checkout restores from
    // the INDEX, so it rewrites the file with CE's own staged bytes and leaves the index entry
    // standing — cleanup returns success having undone nothing. Both a staged modification and a
    // staged addition must come back.
    writeFileSync(join(repo, 'STAGEME.md'), 'committed stageme\nCE gap-fill\n')
    writeFileSync(join(repo, 'CE-ADDED.md'), "CE's own new file\n")
    git(repo, 'add', 'STAGEME.md', 'CE-ADDED.md')

    // The cleanup list is the diff of the two snapshots — not a glob, not a guess.
    const plansAfter = git(repo, 'status', '--porcelain', '--', 'docs/plans')
    const addedPlans = execFileSync('bash', ['-c', 'ls docs/plans | sort'], {
      cwd: repo,
      encoding: 'utf8',
    })
      .split('\n')
      .filter(Boolean)
      .filter((name) => !plansBefore.includes(name))
    const after = git(repo, 'status', '--porcelain')
    const changedSincebefore = after
      .split('\n')
      .filter(Boolean)
      .filter((line) => !before.split('\n').filter(Boolean).includes(line))

    assert.deepEqual(
      addedPlans,
      ['2026-08-09-001-feature-thing-plan.md'],
      'the enumeration must name CE’s plan',
    )
    assert.ok(
      plansAfter.includes('2026-08-09-001-feature-thing-plan.md'),
      'git must agree the plan is new',
    )
    // CONCEPTS.md was dirty before AND after, so its porcelain line is unchanged and it does NOT
    // appear as newly-changed. Status can neither see nor undo that drift — only the pre-run byte
    // copy the snapshot took can, which is why the snapshot copies content and not just status.
    assert.deepEqual(
      changedSincebefore
        .map((l) => l.trim())
        .filter((l) => !/(README|STAGEME|CE-ADDED)\.md$/.test(l)),
      ['?? docs/plans/2026-08-09-001-feature-thing-plan.md'],
    )
    assert.equal(
      readFileSync(join(stage, 'before', 'CONCEPTS.md'), 'utf8'),
      'original concepts\nwork in progress\n',
      'the snapshot must carry the pre-run BYTES of an already-dirty file, not just its status',
    )

    // Run the documented cleanup block verbatim. Nothing here reimplements it: if the doc's recipe
    // cannot restore the worktree, this test reds.
    execFileSync('bash', ['-euo', 'pipefail', '-c', cleanup], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    assert.equal(
      git(repo, 'status', '--porcelain'),
      before,
      'after cleanup the worktree must be byte-identical to the pre-run snapshot',
    )
    assert.equal(
      readFileSync(join(repo, 'CONCEPTS.md'), 'utf8'),
      'original concepts\nwork in progress\n',
      'cleanup must restore CE’s edit without discarding the pre-existing dirty work',
    )
    assert.equal(
      readFileSync(wasClean, 'utf8'),
      'committed readme\n',
      'a file that was clean before the run must come back to its committed content',
    )
    assert.ok(
      existsSync(join(repo, 'docs', 'plans', 'pre-existing-plan.md')),
      'a pre-existing plan must survive',
    )
    assert.ok(
      existsSync(join(runTmp, 'ce-notes.md')),
      'what CE wrote inside runTmp is the core’s to remove',
    )
    assert.equal(
      readFileSync(join(repo, 'STAGEME.md'), 'utf8'),
      'committed stageme\n',
      'a file CE modified AND staged must come back to its committed content, index included',
    )
    assert.equal(
      existsSync(join(repo, 'CE-ADDED.md')),
      false,
      'a file CE created AND staged must not survive cleanup',
    )
  })
})

test('BOS-813 fixtures: cleanup is documented as unconditional across the failure paths', () => {
  const body = draftBody()
  assert.match(body, /Cleanup\s+is\s+unconditional/i, 'cleanup must be stated as unconditional')
  assert.match(
    body,
    /runs\s+on\s+the\s+failure\s+paths\s+too/i,
    'a dispatch that fails after CE wrote its plan must still restore the worktree',
  )
  assert.match(
    body,
    /Nothing\s+CE\s+wrote \*\*outside\*\* `runTmp` may\s+survive/,
    'the cleanup boundary must stay at runTmp',
  )

  // The two reaches the recipe does not have. `git status` is blind to gitignored paths and to
  // empty directories, so the boundary claim above is only true for paths git can see. A doc that
  // promised more than the recipe delivers is the failure mode this pair of gates exists to stop —
  // if someone widens the claim, they have to widen the mechanism or red this test.
  assert.match(
    body,
    /\.gitignore/,
    'the doc must name the gitignored-path limit rather than promising to reach it',
  )
  assert.match(
    body,
    /empty\s+directories/i,
    'the doc must name the empty-directory limit rather than promising to reach it',
  )
  // Restore, not checkout: `checkout --` sources the INDEX, so a CE `git add` defeats cleanup.
  const [, cleanup] = stagingBlocks(body)
  assert.match(
    cleanup,
    /git -C "\$ROOT" restore --source=HEAD --staged --worktree/,
    'cleanup must source HEAD and reset both trees, not restore CE’s own staged bytes',
  )
  assert.doesNotMatch(
    cleanup,
    /git -C "\$ROOT" checkout --/,
    'cleanup must not fall back to an index-sourced checkout',
  )
})

test('BOS-813 fixtures: an unsubstituted runTmp aborts both blocks instead of silently passing', () => {
  // The blocks above are executed with `bash -euo pipefail` and `RUN_TMP` bound, which is NOT the
  // shell the agent runs them in: its Bash tool starts a fresh shell per call with default flags and
  // no inherited value. That is the gap this test covers. Unguarded, `STAGE` becomes `/ce-stage`,
  // the cleanup block's `[ -d "$STAGE/before" ] || exit 0` fires, and cleanup exits 0 having left
  // every CE artifact in the worktree — the one failure the whole recipe exists to prevent, and the
  // one that reports success while it happens.
  const [snapshot, cleanup] = stagingBlocks(draftBody())

  // Both blocks must say which shell they are: the fixtures above impose `-euo pipefail`, so a
  // recipe that does not set it itself is gated under flags production never uses.
  for (const [name, block] of [
    ['snapshot', snapshot],
    ['cleanup', cleanup],
  ]) {
    assert.match(
      block,
      /^set -euo[ ]pipefail$/m,
      `the ${name} block must declare its own shell flags, not inherit the fixture's`,
    )
  }

  withTmp((dir) => {
    const repo = join(dir, 'repo')
    mkdirSync(repo)
    git(repo, 'init', '-q')
    writeFileSync(join(repo, 'README.md'), 'committed readme\n')
    git(repo, 'add', '-A')
    git(repo, 'commit', '-qm', 'seed')
    mkdirSync(join(repo, 'docs', 'plans'), { recursive: true })
    writeFileSync(join(repo, 'docs', 'plans', 'ce-plan.md'), "CE's own plan\n")
    writeFileSync(join(repo, 'README.md'), 'committed readme\nCE gap-fill\n')

    const env = { ...process.env }
    delete env.RUN_TMP
    for (const [name, block] of [
      ['snapshot', snapshot],
      ['cleanup', cleanup],
    ]) {
      // Default flags on purpose — `bash -c`, not `bash -euo pipefail -c`. The guard has to hold in
      // the agent's shell, not only in one hardened for the test.
      const run = spawnSync('bash', ['-c', block], { cwd: repo, env, encoding: 'utf8' })
      assert.notEqual(
        run.status,
        0,
        `the ${name} block must abort when runTmp was never substituted, not exit 0`,
      )
    }

    assert.ok(
      existsSync(join(repo, 'docs', 'plans', 'ce-plan.md')),
      'the aborted cleanup must not be mistaken for a cleanup that ran: the artifact is still here',
    )
  })
})

test('BOS-813 fixtures: cleanup restores a dirty file whose directory CE removed', () => {
  // "Cleanup is unconditional" has to survive the ordinary case where CE reorganises a directory.
  // Restoring the byte copies is step 2; without `mkdir -p` the `cp` fails, and under the block's
  // own `set -e` that aborts before step 3 ever runs, leaving CE's other edits in place.
  withTmp((dir) => {
    const repo = join(dir, 'repo')
    const runTmp = join(dir, 'runtmp')
    mkdirSync(repo)
    mkdirSync(runTmp)
    git(repo, 'init', '-q')
    mkdirSync(join(repo, 'docs', 'plans'), { recursive: true })
    mkdirSync(join(repo, 'wip'))
    writeFileSync(join(repo, 'README.md'), 'committed readme\n')
    writeFileSync(join(repo, 'wip', 'keep.md'), 'committed wip\n')
    git(repo, 'add', '-A')
    git(repo, 'commit', '-qm', 'seed')

    writeFileSync(join(repo, 'wip', 'keep.md'), 'committed wip\nthe user own in-progress work\n')

    const [snapshot, cleanup] = stagingBlocks(draftBody())
    execFileSync('bash', ['-euo', 'pipefail', '-c', snapshot], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    // CE reorganises: it removes the directory holding the user's dirty file, and edits another.
    rmSync(join(repo, 'wip'), { recursive: true, force: true })
    writeFileSync(join(repo, 'README.md'), 'committed readme\nCE gap-fill\n')

    execFileSync('bash', ['-euo', 'pipefail', '-c', cleanup], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    assert.equal(
      readFileSync(join(repo, 'wip', 'keep.md'), 'utf8'),
      'committed wip\nthe user own in-progress work\n',
      'a dirty file whose directory CE removed must be restored, parent directory and all',
    )
    assert.equal(
      readFileSync(join(repo, 'README.md'), 'utf8'),
      'committed readme\n',
      'the restore step must not abort before the later steps of an "unconditional" cleanup',
    )
  })
})

test('BOS-813 fixtures: cleanup restores a tracked file CE renamed away', () => {
  // A rename is the one porcelain entry that names two paths. Restoring only the new one deletes
  // it (it is absent from HEAD) and leaves the origin's deletion staged, so a tracked file the run
  // never owned disappears from both trees — the exact opposite of "leaves the worktree as it
  // found it". The origin has to be restored explicitly, and a dirty origin has to come back with
  // the user's own bytes rather than HEAD's.
  withTmp((dir) => {
    const repo = join(dir, 'repo')
    const runTmp = join(dir, 'runtmp')
    mkdirSync(repo)
    mkdirSync(runTmp)
    git(repo, 'init', '-q')
    mkdirSync(join(repo, 'docs', 'plans'), { recursive: true })
    writeFileSync(join(repo, 'clean.md'), 'committed clean\n')
    writeFileSync(join(repo, 'dirty.md'), 'committed dirty\n')
    git(repo, 'add', '-A')
    git(repo, 'commit', '-qm', 'seed')

    writeFileSync(join(repo, 'dirty.md'), 'committed dirty\nthe user own in-progress work\n')

    const [snapshot, cleanup] = stagingBlocks(draftBody())
    execFileSync('bash', ['-euo', 'pipefail', '-c', snapshot], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    // CE reorganises both files by renaming them.
    git(repo, 'mv', 'clean.md', 'renamed-clean.md')
    git(repo, 'mv', 'dirty.md', 'renamed-dirty.md')

    execFileSync('bash', ['-euo', 'pipefail', '-c', cleanup], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    assert.equal(
      readFileSync(join(repo, 'clean.md'), 'utf8'),
      'committed clean\n',
      'a tracked file CE renamed must be restored at its original path',
    )
    assert.equal(
      readFileSync(join(repo, 'dirty.md'), 'utf8'),
      'committed dirty\nthe user own in-progress work\n',
      'a dirty rename origin must come back as the user left it, not as HEAD has it',
    )
    assert.ok(
      !existsSync(join(repo, 'renamed-clean.md')) && !existsSync(join(repo, 'renamed-dirty.md')),
      "CE's renamed copies must not survive cleanup",
    )
    assert.equal(
      execFileSync('git', ['status', '--porcelain'], { cwd: repo, encoding: 'utf8' }),
      ' M dirty.md\n',
      'cleanup must leave no staged rename or deletion behind',
    )
  })
})

test('BOS-813 fixtures: cleanup restores the INDEX of a file that was already dirty pre-run', () => {
  // A status entry records THAT a path was dirty; it does not record what was STAGED for it. Step 3
  // skips every path the snapshot took a byte copy of, so an already-dirty path is undone by the
  // byte-copy loop alone — and a pure `cp` undoes only half of it. If CE ran `git add`, its bytes
  // stay in the index: the file reads back as the user left it, cleanup reports success, and
  // `git diff --cached` still carries CE's work into whatever the core attaches next.
  withTmp((dir) => {
    const repo = join(dir, 'repo')
    const runTmp = join(dir, 'runtmp')
    mkdirSync(repo)
    mkdirSync(runTmp)
    git(repo, 'init', '-q')
    mkdirSync(join(repo, 'docs', 'plans'), { recursive: true })
    writeFileSync(join(repo, 'keep.txt'), 'committed keep\n')
    git(repo, 'add', '-A')
    git(repo, 'commit', '-qm', 'seed')

    // The user's own in-progress work, unstaged: ` M keep.txt`.
    const userBytes = 'committed keep\nthe user own in-progress work\n'
    writeFileSync(join(repo, 'keep.txt'), userBytes)

    const [snapshot, cleanup] = stagingBlocks(draftBody())
    execFileSync('bash', ['-euo', 'pipefail', '-c', snapshot], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })
    const before = git(repo, 'status', '--porcelain')
    assert.equal(before, ' M keep.txt\n', 'the fixture must start from an unstaged modification')

    // CE gap-fills the same file and stages it: ` M` becomes `M `.
    writeFileSync(join(repo, 'keep.txt'), `${userBytes}CE gap-fill\n`)
    git(repo, 'add', 'keep.txt')

    execFileSync('bash', ['-euo', 'pipefail', '-c', cleanup], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    assert.equal(
      readFileSync(join(repo, 'keep.txt'), 'utf8'),
      userBytes,
      'the worktree bytes must come back exactly as the user left them',
    )
    assert.equal(
      git(repo, 'diff', '--cached'),
      '',
      "cleanup must leave nothing of CE's in the index — a restored file with a staged diff is not restored",
    )
    assert.equal(
      git(repo, 'status', '--porcelain'),
      before,
      'after cleanup the status must be byte-identical to the pre-run snapshot, staged column included',
    )
  })
})

test('BOS-813 fixtures: cleanup leaves a deletion the user had already staged alone', () => {
  // Rename detection pairs by CONTENT, not by authorship. A file the user had already deleted gets
  // paired with any CE addition that happens to carry its bytes, and the entry then reads as a
  // rename CE performed — origin included. Restoring that origin resurrects a deletion the user
  // meant, from a file CE never touched. Only the pre-run snapshot can tell the two apart.
  withTmp((dir) => {
    const repo = join(dir, 'repo')
    const runTmp = join(dir, 'runtmp')
    mkdirSync(repo)
    mkdirSync(runTmp)
    git(repo, 'init', '-q')
    mkdirSync(join(repo, 'docs', 'plans'), { recursive: true })
    mkdirSync(join(repo, 'd'))
    // Long enough that git's similarity detection pairs it confidently with an identical copy.
    const shared = Array.from({ length: 40 }, (_, i) => `shared line ${i}`).join('\n') + '\n'
    writeFileSync(join(repo, 'd', 'other.txt'), shared)
    git(repo, 'add', '-A')
    git(repo, 'commit', '-qm', 'seed')

    // The user removes it themselves, before the run: `D  d/other.txt`.
    git(repo, 'rm', '-q', 'd/other.txt')

    const [snapshot, cleanup] = stagingBlocks(draftBody())
    execFileSync('bash', ['-euo', 'pipefail', '-c', snapshot], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })
    const before = git(repo, 'status', '--porcelain')
    assert.equal(before, 'D  d/other.txt\n', 'the fixture must start from a staged deletion')

    // CE adds an unrelated file that happens to carry the same content, and stages it. Git now
    // reports the pair as one rename entry whose origin is the user's deletion.
    writeFileSync(join(repo, 'CE-NEW.md'), shared)
    git(repo, 'add', 'CE-NEW.md')
    assert.match(
      git(repo, 'status', '--porcelain'),
      /^R {2}d\/other\.txt -> CE-NEW\.md$/m,
      'the fixture must reproduce the rename pairing, otherwise it proves nothing',
    )

    execFileSync('bash', ['-euo', 'pipefail', '-c', cleanup], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    assert.equal(
      existsSync(join(repo, 'd', 'other.txt')),
      false,
      'cleanup must not resurrect a file the user deleted and CE never touched',
    )
    assert.equal(existsSync(join(repo, 'CE-NEW.md')), false, "CE's own addition must not survive")
    assert.equal(
      git(repo, 'status', '--porcelain'),
      before,
      "the user's staged deletion must still be staged after cleanup",
    )
  })
})

test('BOS-813 fixtures: cleanup does not revert an UNSTAGED deletion CE happened to stage', () => {
  // The step-3 skip is a byte-compare against the pre-run status entry, and CE restages the user's
  // own unstaged deletion merely by running `git add -A`: ` D gone.txt` becomes `D  gone.txt`, the
  // compare misses it, and the HEAD restore puts a file the user deliberately removed back on disk
  // — with a clean status afterwards, so nothing survives to show the deletion was ever meant.
  withTmp((dir) => {
    const repo = join(dir, 'repo')
    const runTmp = join(dir, 'runtmp')
    mkdirSync(repo)
    mkdirSync(runTmp)
    git(repo, 'init', '-q')
    mkdirSync(join(repo, 'docs', 'plans'), { recursive: true })
    writeFileSync(join(repo, 'gone.txt'), 'the user does not want this file\n')
    git(repo, 'add', '-A')
    git(repo, 'commit', '-qm', 'seed')

    // The user removes it from the worktree only — the deletion is NOT staged: ` D gone.txt`.
    rmSync(join(repo, 'gone.txt'))

    const [snapshot, cleanup] = stagingBlocks(draftBody())
    execFileSync('bash', ['-euo', 'pipefail', '-c', snapshot], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })
    const before = git(repo, 'status', '--porcelain')
    assert.equal(before, ' D gone.txt\n', 'the fixture must start from an UNSTAGED deletion')

    // CE writes its plan and stages the tree wholesale, sweeping the user's deletion in with it.
    writeFileSync(join(repo, 'docs', 'plans', '2026-08-09-001-ce-plan.md'), '# CE plan\n')
    git(repo, 'add', '-A')
    assert.match(
      git(repo, 'status', '--porcelain'),
      /^D {2}gone\.txt$/m,
      'the fixture must reproduce the restaging, otherwise it proves nothing',
    )

    execFileSync('bash', ['-euo', 'pipefail', '-c', cleanup], {
      cwd: repo,
      env: { ...process.env, RUN_TMP: runTmp },
      encoding: 'utf8',
    })

    assert.equal(
      existsSync(join(repo, 'gone.txt')),
      false,
      'cleanup must not resurrect a file the user deleted and CE never touched',
    )
    assert.equal(
      git(repo, 'status', '--porcelain'),
      before,
      'the deletion must read back UNSTAGED, exactly as the snapshot recorded it',
    )
  })
})

test('BOS-813 fixtures: CE is invoked under its plugin-qualified skill name', () => {
  // CE ships as a Claude Code plugin, so the Skill tool resolves `compound-engineering:ce-plan`
  // and nothing else. A bare `ce-plan` resolves to nothing, the preflight reads that as "CE
  // unavailable", and every dispatch falls through to the core fallback with all gates still
  // green — the feature ships inert. Pin the qualified name at each invocation instruction.
  const body = draftBody()
  const invocations = body.split('\n').filter((line) => /Invoke\s+`[^`]*ce-plan`/.test(line))
  assert.ok(invocations.length >= 2, 'both modes must carry an explicit ce-plan invocation')
  for (const line of invocations) {
    assert.match(
      line,
      /Invoke\s+`compound-engineering:ce-plan`/,
      'every ce-plan invocation must name the plugin-qualified skill',
    )
  }
  assert.match(
    body.replace(/\s+/g, ' '),
    /CE\s+ships\s+as\s+a\s+Claude\s+Code \*\*plugin\*\*[^.]*plugin-qualified\s+names/,
    'the preflight must say why the qualified name is required',
  )
})

test('BOS-813 fixtures: each mode states its own mode to CE instead of relying on the ambient flag', () => {
  // `disable-model-invocation: true` sits in this extension's frontmatter and reads identically on
  // both paths, so it cannot be what tells CE which mode it is in. If the interactive dispatch says
  // nothing, CE's pipeline trigger fires and the native interview — the whole point of AC#2's
  // interactive half — is silently skipped. Each mode must say so in the invocation itself.
  const body = draftBody()
  assert.match(
    body,
    /^disable-model-invocation: true$/m,
    'the premise of this gate is the frontmatter flag; if it goes, revisit both mode sections',
  )
  const section = (heading) => {
    const start = body.indexOf(`### ${heading}\n`)
    assert.notEqual(start, -1, `the SKILL.md must keep a "${heading}" mode section`)
    const rest = body.slice(start + heading.length + 5)
    const end = rest.search(/^##+ /m)
    return (end === -1 ? rest : rest.slice(0, end)).replace(/\s+/g, ' ')
  }
  assert.match(
    section('Interactive'),
    /this\s+dispatch\s+is\s+interactive[^.]*native\s+interview[^.]*do\s+not\s+take\s+the\s+pipeline\s+path/i,
    'the interactive dispatch must tell CE to run its interview, not the pipeline path',
  )
  assert.match(
    section('Headless'),
    /this\s+is\s+a\s+pipeline\s+run[^.]*take\s+the\s+non-interactive\s+path/i,
    'the headless dispatch must tell CE it is a pipeline run',
  )
  for (const heading of ['Interactive', 'Headless']) {
    assert.match(
      section(heading),
      /ambient `disable-model-invocation`|`disable-model-invocation` context\s+CE\s+also\s+keys\s+off/i,
      `the ${heading} section must record why the ambient flag is not the signal`,
    )
  }
})
