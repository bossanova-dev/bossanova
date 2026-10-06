#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath, pathToFileURL } from 'node:url'

import {
  SKILL_DRIFT_ADVISORY_SENTENCE,
  SKILL_DRIFT_CORE_CLOSURE,
  SKILL_DRIFT_DIRECTIONS,
  SKILL_DRIFT_KINDS,
  SKILL_DRIFT_VERDICTS,
  classifySkillDrift,
  inferRunningCore,
  installedReferenceChecker,
  isUnsupportedFlagOutput,
  main,
  parseSkillGateOutput,
  renderSkillDriftVerdict,
  rowInScope,
  scopeForCores,
  verdictForDriftRow,
} from './skill-drift-verdict.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./skill-drift-verdict.mjs', import.meta.url))

const row = (path, kind, direction) => `  - ${path} (${kind}, ${direction})`
const gate = (...lines) => ['boss skills gate: claude skill drift detected', ...lines].join('\n')
const classify = (output, exitStatus = 1) => classifySkillDrift({ exitStatus, output })
const render = (output, exitStatus = 1) =>
  renderSkillDriftVerdict(classify(output, exitStatus)).join('\n')

// The gate's live output on a real checkout, captured verbatim from
// `boss skills check --gate` (both agents, exit 1). It is here rather than hand-written because a
// parser pinned only against invented bytes agrees with itself: this fixture carries the shapes
// nobody would think to invent — two agent sections, a `run` line between them, and the CLI's own
// two trailing error lines, none of which may be read as a drift row.
const LIVE_GATE_OUTPUT = `boss skills gate: claude skill drift detected
  - boss-build/toolbox/bs-epic-lib.mjs (content, behind)
  - boss-build/toolbox/callback/boss.mjs (content, behind)
  - boss-build/toolbox/dag-scheduler.mjs (content, behind)
  - boss-build/toolbox/session/adapter.mjs (content, behind)
  - boss-build/toolbox/session/boss.mjs (content, behind)
  - boss-build/toolbox/skill-extensions.mjs (content, behind)
  - boss-epic/SKILL.md (content, behind)
  - boss-epic/references/epic-driver.md (absent, lossless)
  - boss-epic/references/merge-recovery.md (content, behind)
  - boss-epic/references/multi-root.md (absent, lossless)
  - boss-epic/toolbox/bs-epic-lib.mjs (content, behind)
  - boss-epic/toolbox/callback/boss.mjs (content, behind)
  - boss-epic/toolbox/dag-scheduler.mjs (content, behind)
  - boss-epic/toolbox/epic-driver.mjs (absent, lossless)
  - boss-epic/toolbox/progress-comment.mjs (content, behind)
  - boss-epic/toolbox/session/adapter.mjs (content, behind)
  - boss-epic/toolbox/session/boss.mjs (content, behind)
  - boss-epic/toolbox/skill-extensions.mjs (content, behind)
  - boss-finalize/toolbox/dag-scheduler.mjs (content, behind)
  - boss-plan/toolbox/bs-epic-lib.mjs (content, behind)
  - boss-plan/toolbox/dag-scheduler.mjs (content, behind)
  - boss-plan/toolbox/skill-extensions.mjs (content, behind)
  - boss-repair/SKILL.md (content, behind)
  - boss-repair/toolbox/callback/boss.mjs (content, behind)
  - boss-repair/toolbox/dag-scheduler.mjs (content, behind)
  - boss-repair/toolbox/session/adapter.mjs (content, behind)
  - boss-repair/toolbox/session/boss.mjs (content, behind)
  - boss-repair/toolbox/skill-extensions.mjs (content, behind)
  - boss-review/toolbox/dag-scheduler.mjs (content, behind)
  - boss-review/toolbox/skill-extensions.mjs (content, behind)
  run \`BOSS_TRUST_CHECKOUT_SKILLS=1 /Users/dave/.bossanova/worktrees/bossanova/cron-boss-build-1789934400/bin/boss skills install\`
boss skills gate: codex skill drift detected
  - boss-build/toolbox/bs-epic-lib.mjs (content, behind)
  - boss-build/toolbox/callback/boss.mjs (content, behind)
  - boss-build/toolbox/dag-scheduler.mjs (content, behind)
  - boss-build/toolbox/session/adapter.mjs (content, behind)
  - boss-build/toolbox/session/boss.mjs (content, behind)
  - boss-build/toolbox/skill-extensions.mjs (content, behind)
  - boss-epic/SKILL.md (content, behind)
  - boss-epic/references/epic-driver.md (absent, lossless)
  - boss-epic/references/merge-recovery.md (content, behind)
  - boss-epic/references/multi-root.md (absent, lossless)
  - boss-epic/toolbox/bs-epic-lib.mjs (content, behind)
  - boss-epic/toolbox/callback/boss.mjs (content, behind)
  - boss-epic/toolbox/dag-scheduler.mjs (content, behind)
  - boss-epic/toolbox/epic-driver.mjs (absent, lossless)
  - boss-epic/toolbox/progress-comment.mjs (content, behind)
  - boss-epic/toolbox/session/adapter.mjs (content, behind)
  - boss-epic/toolbox/session/boss.mjs (content, behind)
  - boss-epic/toolbox/skill-extensions.mjs (content, behind)
  - boss-finalize/toolbox/dag-scheduler.mjs (content, behind)
  - boss-plan/toolbox/bs-epic-lib.mjs (content, behind)
  - boss-plan/toolbox/dag-scheduler.mjs (content, behind)
  - boss-plan/toolbox/skill-extensions.mjs (content, behind)
  - boss-repair/SKILL.md (content, behind)
  - boss-repair/toolbox/callback/boss.mjs (content, behind)
  - boss-repair/toolbox/dag-scheduler.mjs (content, behind)
  - boss-repair/toolbox/session/adapter.mjs (content, behind)
  - boss-repair/toolbox/session/boss.mjs (content, behind)
  - boss-repair/toolbox/skill-extensions.mjs (content, behind)
  - boss-review/toolbox/dag-scheduler.mjs (content, behind)
  - boss-review/toolbox/skill-extensions.mjs (content, behind)
  run \`BOSS_TRUST_CHECKOUT_SKILLS=1 /Users/dave/.bossanova/worktrees/bossanova/cron-boss-build-1789934400/bin/boss skills install\`
boss: skill drift detected
skill drift detected
`

// Counted from the fixture above, not asserted from the parser's own output: a count the parser
// produces cannot falsify the parser.
const LIVE_TOTAL_ROWS = 60
const LIVE_CONTENT_BEHIND = 54
const LIVE_ABSENT_LOSSLESS = 6

// ---------------------------------------------------------------------------
// Vocabulary

test('the verdict vocabulary is exactly five values and is frozen', () => {
  assert.ok(Object.isFrozen(SKILL_DRIFT_VERDICTS))
  assert.deepEqual([...Object.values(SKILL_DRIFT_VERDICTS)].sort(), [
    'advisory',
    'advisory-withheld',
    'blocking',
    'clean',
    'undecided',
  ])
})

test('the kind and direction vocabularies match the gate: exactly five kinds, five directions', () => {
  assert.deepEqual([...SKILL_DRIFT_KINDS].sort(), [
    'absent',
    'broken-symlink',
    'content',
    'mode',
    'unexpected',
  ])
  assert.deepEqual([...SKILL_DRIFT_DIRECTIONS].sort(), [
    'ahead',
    'behind',
    'lossless',
    'unknown',
    'unrecoverable',
  ])
})

// ---------------------------------------------------------------------------
// Totality: every kind x every direction

test('every one of the five kinds x five directions maps to a verdict, with no pair unasserted', () => {
  // The table is written out in full rather than derived from the same sets the module uses: a
  // table generated from CAPABILITY_KINDS would agree with any reshuffling of it.
  const expected = {
    'absent|lossless': 'blocking',
    'absent|behind': 'blocking',
    'absent|ahead': 'blocking',
    'absent|unrecoverable': 'blocking',
    'absent|unknown': 'blocking',
    'mode|lossless': 'blocking',
    'mode|behind': 'blocking',
    'mode|ahead': 'blocking',
    'mode|unrecoverable': 'blocking',
    'mode|unknown': 'blocking',
    'broken-symlink|lossless': 'blocking',
    'broken-symlink|behind': 'blocking',
    'broken-symlink|ahead': 'blocking',
    'broken-symlink|unrecoverable': 'blocking',
    'broken-symlink|unknown': 'blocking',
    'content|lossless': 'advisory',
    'content|behind': 'advisory',
    'content|ahead': 'advisory-withheld',
    'content|unrecoverable': 'advisory-withheld',
    'content|unknown': 'advisory-withheld',
    'unexpected|lossless': 'advisory',
    'unexpected|behind': 'advisory',
    'unexpected|ahead': 'advisory-withheld',
    'unexpected|unrecoverable': 'advisory-withheld',
    'unexpected|unknown': 'advisory-withheld',
  }
  const pairs = []
  for (const kind of SKILL_DRIFT_KINDS) {
    for (const direction of SKILL_DRIFT_DIRECTIONS) {
      const key = `${kind}|${direction}`
      pairs.push(key)
      assert.equal(verdictForDriftRow(kind, direction), expected[key], key)
      // The same pair, routed through the whole classifier rather than the row predicate, so a
      // split between the two cannot hide here.
      assert.equal(classify(gate(row('x/y.mjs', kind, direction))).verdict, expected[key], key)
    }
  }
  assert.equal(pairs.length, 25)
  assert.deepEqual(pairs.sort(), Object.keys(expected).sort())
})

// ---------------------------------------------------------------------------
// The split this module exists for

test('an absent row blocks, and its render never claims the work state is unaffected', () => {
  const result = classify(gate(row('boss-epic/toolbox/epic-driver.mjs', 'absent', 'lossless')))
  assert.equal(result.verdict, 'blocking')
  assert.equal(result.blocking, true)
  assert.equal(result.reason, 'absent-capability')
  const text = renderSkillDriftVerdict(result).join('\n')
  // The falsification: deleting the recording/capability split makes this row advisory, and an
  // advisory render carries the sentence. Asserting only the advisory row would stay green.
  assert.ok(!text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
  assert.match(text, /^BLOCKED: /)
  assert.match(text, /boss-epic\/toolbox\/epic-driver\.mjs/)
})

test('a mode row and a broken-symlink row each block', () => {
  for (const kind of ['mode', 'broken-symlink']) {
    const result = classify(gate(row('a/b.mjs', kind, 'lossless')))
    assert.equal(result.verdict, 'blocking', kind)
    assert.equal(result.blocking, true, kind)
    assert.ok(
      !renderSkillDriftVerdict(result).join('\n').includes(SKILL_DRIFT_ADVISORY_SENTENCE),
      kind,
    )
  }
})

test('a content/behind row under a core the run does not load keeps the greppable advisory sentence', () => {
  const output = gate(row('boss-epic/SKILL.md', 'content', 'behind'), '  run `boss skills install`')
  const result = classifySkillDrift({ exitStatus: 1, output, cores: ['boss-plan'] })
  assert.equal(result.verdict, 'advisory')
  assert.equal(result.blocking, false)
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.ok(text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
  // Byte-compatible with the line the three preflights printed before this module existed, so the
  // greps that look for it still find it.
  assert.equal(
    text,
    'warning: installed boss skills drift from checkout source; run: boss skills install — ' +
      'bookkeeping only, work state unaffected',
  )
})

test('an advisory report with no run line falls back to the see-gate-output wording', () => {
  const result = classifySkillDrift({
    exitStatus: 1,
    output: gate(row('boss-epic/b.mjs', 'content', 'behind')),
    cores: ['boss-plan'],
  })
  assert.equal(
    renderSkillDriftVerdict(result).join('\n'),
    'warning: installed boss skills drift from checkout source; see gate output above — ' +
      'bookkeeping only, work state unaffected',
  )
})

test('an in-scope content/behind row is advisory, named, and never called work-state-neutral', () => {
  for (const kind of ['content', 'unexpected']) {
    const output = gate(
      row('boss-plan/SKILL.md', kind, 'behind'),
      row('boss-epic/SKILL.md', 'content', 'behind'),
      '  run `boss skills install`',
    )
    const result = classifySkillDrift({ exitStatus: 1, output, cores: ['boss-plan'] })
    assert.equal(result.verdict, 'advisory', kind)
    assert.equal(result.blocking, false, kind)
    const text = renderSkillDriftVerdict(result).join('\n')
    assert.ok(!text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
    assert.ok(text.includes(`boss-plan/SKILL.md (${kind}, behind)`), text)
    assert.ok(
      !text.includes('boss-epic/SKILL.md'),
      `only in-scope paths are named as executed: ${text}`,
    )
    assert.match(text, /this run executes 1 installed path\(s\) that lag checkout source/)
    assert.match(text, /run: boss skills install/)
  }
})

test('whole-tree scope treats every content row as executed, so the sentence is withheld', () => {
  const text = render(gate(row('boss-epic/SKILL.md', 'content', 'behind')))
  assert.ok(!text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
  assert.match(text, /boss-epic\/SKILL\.md \(content, behind\)/)
})

test('a content/unrecoverable row is advisory-withheld and its render carries no runnable command', () => {
  const output = gate(
    row('a/b.mjs', 'content', 'unrecoverable'),
    '  reinstall withheld — it would overwrite installed content this checkout cannot restore:',
    '    a/b.mjs (content, unrecoverable)',
    '  next: move the listed paths out of the skills directory (or delete them if they are disposable); with nothing unrecoverable left, the reinstall command is offered again',
  )
  const result = classify(output)
  assert.equal(result.verdict, 'advisory-withheld')
  assert.equal(result.blocking, false)
  assert.equal(result.remedy.command, null)
  assert.equal(result.remedy.withheld, true)
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.ok(!/run: /.test(text), text)
  assert.ok(!text.includes('`'), text)
  // Whole-tree scope: the row is one this run may execute, so it is named and not called neutral.
  assert.ok(!text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
  assert.match(text, /a\/b\.mjs \(content, unrecoverable\)/)
  assert.match(text, /withheld its reinstall remedy/)

  // Out of scope, the same withheld report keeps today's sentence-bearing line.
  const outOfScope = renderSkillDriftVerdict(
    classifySkillDrift({
      exitStatus: 1,
      output: output.replaceAll('a/b.mjs', 'boss-epic/b.mjs'),
      cores: ['boss-plan'],
    }),
  ).join('\n')
  assert.ok(!/run: /.test(outOfScope), outOfScope)
  assert.ok(outOfScope.includes(SKILL_DRIFT_ADVISORY_SENTENCE), outOfScope)
})

test('a withheld lead suppresses a run line offered for the other agent', () => {
  const output = [
    'boss skills gate: claude skill drift detected',
    row('a/b.mjs', 'content', 'behind'),
    '  run `boss skills install`',
    'boss skills gate: codex skill drift detected',
    row('a/b.mjs', 'content', 'unrecoverable'),
    '  reinstall withheld — the installed-vs-checkout comparison failed, so whether a reinstall would overwrite unrecoverable content is undecided',
    '  next: re-run this check once the skills directory is readable and no longer changing underneath it',
  ].join('\n')
  const result = classify(output)
  assert.equal(result.remedy.command, null)
  assert.equal(result.verdict, 'advisory-withheld')
})

// ---------------------------------------------------------------------------
// Fail-closed arms

test('a non-zero gate with no parseable drift row is undecided, never advisory', () => {
  for (const output of [
    '',
    'boss: resolve reinstall safety: exit status 128',
    'boss skills gate: claude clean — compared 412 file(s) · skills dir: /s · payload source: /p',
    'boss skills gate: claude self-edited drift: boss-build/SKILL.md',
  ]) {
    const result = classify(output)
    assert.equal(result.verdict, 'undecided', output)
    assert.equal(result.blocking, true, output)
    assert.equal(result.reason, 'no-drift-row', output)
    assert.ok(!renderSkillDriftVerdict(result).join('\n').includes(SKILL_DRIFT_ADVISORY_SENTENCE))
  }
})

test('an unrecognised kind or direction token is undecided, never advisory', () => {
  for (const [kind, direction] of [
    ['newkind', 'behind'],
    ['content', 'sideways'],
    ['newkind', 'sideways'],
  ]) {
    const result = classify(gate(row('a/b.mjs', kind, direction)))
    assert.equal(result.verdict, 'undecided', `${kind}|${direction}`)
    assert.equal(result.blocking, true, `${kind}|${direction}`)
    assert.equal(result.reason, 'unreadable-drift-row', `${kind}|${direction}`)
  }
})

test('a drift row the parser cannot read at all is kept as a row and lands undecided', () => {
  const result = classify(gate('  - a/b.mjs'))
  assert.equal(result.rows.length, 1)
  assert.equal(result.rows[0].kind, null)
  assert.equal(result.verdict, 'undecided')
  // Not the no-row arm: an unreadable row must not read as a gate that enumerated nothing.
  assert.equal(result.reason, 'unreadable-drift-row')
})

test('output carrying the literal --gate is the old-binary usage error, classified undecided', () => {
  assert.equal(isUnsupportedFlagOutput('Error: unknown flag: --gate'), true)
  assert.equal(isUnsupportedFlagOutput(gate(row('a/b.mjs', 'content', 'behind'))), false)
  const result = classify('Error: unknown flag: --gate')
  assert.equal(result.verdict, 'undecided')
  assert.equal(result.reason, 'unsupported-flag')
  assert.equal(result.blocking, true)
})

test('exit 0 is clean and renders nothing, whatever the output says', () => {
  const result = classify(gate(row('a/b.mjs', 'absent', 'lossless')), 0)
  assert.equal(result.verdict, 'clean')
  assert.equal(result.blocking, false)
  assert.deepEqual(renderSkillDriftVerdict(result), [])
})

test('a mixed report takes its most severe row', () => {
  const both = gate(row('a/b.mjs', 'content', 'behind'), row('c/d.mjs', 'absent', 'lossless'))
  assert.equal(classify(both).verdict, 'blocking')
  const withUnknownToken = gate(
    row('a/b.mjs', 'content', 'behind'),
    row('c/d.mjs', 'nope', 'behind'),
  )
  assert.equal(classify(withUnknownToken).verdict, 'undecided')
  // A concrete stop outranks an unreadable row: both are terminal, and the one that can be named
  // is the one worth naming.
  const blockingAndUnreadable = gate(
    row('c/d.mjs', 'absent', 'lossless'),
    row('e/f.mjs', 'nope', 'behind'),
  )
  assert.equal(classify(blockingAndUnreadable).verdict, 'blocking')
})

// ---------------------------------------------------------------------------
// Parsing shapes the gate really emits

test('gate headers, coverage lines and withheld entries are never read as drift rows', () => {
  const parsed = parseSkillGateOutput(
    [
      'boss skills gate: claude skill drift detected (origin/HEAD unavailable; used git status fallback)',
      'boss skills gate: claude self-edited drift: boss-build/SKILL.md',
      'boss skills gate: codex clean — compared 9 file(s) · skills dir: /s · payload source: /p',
      row('a/b.mjs', 'content', 'behind'),
      '    a/b.mjs (content, behind)',
      '  run `x`',
    ].join('\n'),
  )
  assert.equal(parsed.rows.length, 1)
  assert.equal(parsed.rows[0].path, 'a/b.mjs')
})

test('the run line survives the gate unverified-direction suffix', () => {
  const parsed = parseSkillGateOutput(
    '  run `boss skills install` (reinstall direction unverified: no checkout history to compare the installed copy against)',
  )
  assert.equal(parsed.remedy.command, 'boss skills install')
})

// ---------------------------------------------------------------------------
// The live gate output

test('the live 60-row gate output classifies blocking and names the 6 absent paths as evidence', () => {
  const result = classify(LIVE_GATE_OUTPUT)
  assert.equal(result.rows.length, LIVE_TOTAL_ROWS)
  assert.equal(
    result.rows.filter((r) => r.kind === 'content' && r.direction === 'behind').length,
    LIVE_CONTENT_BEHIND,
  )
  assert.equal(
    result.rows.filter((r) => r.kind === 'absent' && r.direction === 'lossless').length,
    LIVE_ABSENT_LOSSLESS,
  )
  assert.equal(result.verdict, 'blocking')
  assert.equal(result.blocking, true)
  assert.equal(result.evidence.length, LIVE_ABSENT_LOSSLESS)
  assert.deepEqual([...new Set(result.evidence.map((r) => r.path))].sort(), [
    'boss-epic/references/epic-driver.md',
    'boss-epic/references/multi-root.md',
    'boss-epic/toolbox/epic-driver.mjs',
  ])
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.ok(!text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
  // R6: the repair a consumer is handed must include the plugin-rebuild leg, or a stale plugin
  // restores the old payload at daemon start and undoes it inside the same run.
  assert.match(text, /rebuild the plugin binaries/)
  assert.match(text, /make build plugins/)
  assert.match(text, /repair: BOSS_TRUST_CHECKOUT_SKILLS=1 /)
  // Each absent path is named exactly once even though two agent trees reported it.
  for (const p of [
    'boss-epic/references/epic-driver.md',
    'boss-epic/references/multi-root.md',
    'boss-epic/toolbox/epic-driver.mjs',
  ]) {
    assert.equal(text.split(`${p} (absent, lossless)`).length - 1, 1, p)
  }
})

test('the live fixture is the real thing: two agent sections and trailing CLI error lines', () => {
  assert.match(LIVE_GATE_OUTPUT, /^boss skills gate: claude skill drift detected$/m)
  assert.match(LIVE_GATE_OUTPUT, /^boss skills gate: codex skill drift detected$/m)
  assert.match(LIVE_GATE_OUTPUT, /^boss: skill drift detected$/m)
})

// ---------------------------------------------------------------------------
// CLI

test('the CLI classifies stdin, prints the render and exits 1 only when blocking', () => {
  const out = []
  const stdout = { write: (s) => out.push(s) }
  const advisory = main(['classify', '--status', '1', '--core', 'boss-plan'], {
    stdin: gate(row('boss-epic/b.mjs', 'content', 'behind')),
    stdout,
  })
  assert.equal(advisory, 0)
  assert.ok(out.join('').includes(SKILL_DRIFT_ADVISORY_SENTENCE))

  out.length = 0
  const blocking = main(['classify', '--status', '1'], {
    stdin: gate(row('a/b.mjs', 'absent', 'lossless')),
    stdout,
  })
  assert.equal(blocking, 1)
  assert.match(out.join(''), /^BLOCKED: /)

  out.length = 0
  assert.equal(main(['classify', '--status', '0'], { stdin: '', stdout }), 0)
  assert.equal(out.join(''), '')
})

test('a missing --status means non-zero, so a dropped flag cannot become a silent pass', () => {
  const out = []
  assert.equal(
    main(['classify'], {
      stdin: gate(row('a/b.mjs', 'absent', 'lossless')),
      stdout: { write: (s) => out.push(s) },
    }),
    1,
  )
})

test('an unknown verb exits 2 rather than classifying anything', () => {
  const out = []
  assert.equal(main(['explain'], { stdin: '', stdout: { write: (s) => out.push(s) } }), 2)
  assert.match(out.join(''), /expected: classify/)
})

test('the CLI runs as a real subprocess over the live output and exits 1', () => {
  const run = spawnSync(process.execPath, [SCRIPT_PATH, 'classify', '--status', '1'], {
    input: LIVE_GATE_OUTPUT,
    encoding: 'utf8',
  })
  assert.equal(run.status, 1)
  // Three distinct paths, six rows: the gate reports the same absent path once per installed
  // agent tree, and the render must not read as six different missing files.
  assert.match(
    run.stdout,
    /^BLOCKED: installed boss skills are missing 3 capability file\(s\) a later step invokes by path \(6 drift rows across the installed agent trees\): /,
  )
})

// ---------------------------------------------------------------------------
// Scope: the cores this run executes

// Captured from the updated Go gate (`TestRunSkillGate/each tree header states its own count and
// the run line builds first`, run with -v), with its temp checkout root replaced by /repo. It
// carries the shapes that test pins on the producer side: a per-tree count on each header and a
// run line that builds before it installs.
const GO_GATE_OUTPUT = `boss skills gate: claude skill drift detected — 2 path(s) in this agent tree
  - boss-build/SKILL.md (content, behind)
  - boss/SKILL.md (content, behind)
  run \`make -C /repo build plugins && BOSS_TRUST_CHECKOUT_SKILLS=1 /repo/bin/boss skills install\`
boss skills gate: codex skill drift detected — 1 path(s) in this agent tree
  - boss-build/SKILL.md (absent, lossless)
  run \`make -C /repo build plugins && BOSS_TRUST_CHECKOUT_SKILLS=1 /repo/bin/boss skills install\`
`

const withCores = (output, cores) => classifySkillDrift({ exitStatus: 1, output, cores })

test('the closure table names each consumer and the cores it invokes in-run', () => {
  assert.deepEqual(Object.keys(SKILL_DRIFT_CORE_CLOSURE).sort(), [
    'boss-build',
    'boss-plan',
    'boss-repair',
  ])
  assert.deepEqual([...SKILL_DRIFT_CORE_CLOSURE['boss-plan']], ['boss-plan'])
  assert.deepEqual([...SKILL_DRIFT_CORE_CLOSURE['boss-repair']], ['boss-repair'])
  assert.deepEqual([...SKILL_DRIFT_CORE_CLOSURE['boss-build']].sort(), [
    'boss-build',
    'boss-finalize',
    'boss-review',
  ])
  assert.ok(Object.isFrozen(SKILL_DRIFT_CORE_CLOSURE))
})

test('an unknown, empty or absent core widens to whole-tree scope', () => {
  assert.equal(scopeForCores([]), null)
  assert.equal(scopeForCores([null]), null)
  assert.equal(scopeForCores(['boss-epic']), null)
  assert.equal(scopeForCores(['boss-plan', 'boss-epic']), null)
  assert.equal(scopeForCores(['']), null)
  assert.deepEqual([...scopeForCores(['boss-plan', 'boss-repair'])].sort(), [
    'boss-plan',
    'boss-repair',
  ])
})

test('a row is in scope by its first path segment; a non-core segment stays in scope', () => {
  const scope = scopeForCores(['boss-plan'])
  assert.equal(rowInScope('boss-plan/toolbox/x.mjs', scope), true)
  assert.equal(rowInScope('boss-plan', scope), true)
  assert.equal(rowInScope('boss-epic/SKILL.md', scope), false)
  assert.equal(rowInScope('boss-old/', scope), false)
  assert.equal(rowInScope('boss/SKILL.md', scope), false)
  // The gate keys a wholly missing install on the namespace directory, which is no one core's.
  assert.equal(rowInScope('ns/', scope), true)
  assert.equal(rowInScope('boss-epic/SKILL.md', null), true)
})

test('an out-of-scope absent row exits 0 with a warning naming it', () => {
  const output = gate(row('boss-epic/toolbox/epic-driver.mjs', 'absent', 'lossless'))
  const result = withCores(output, ['boss-plan'])
  assert.equal(result.blocking, false)
  assert.equal(result.verdict, 'advisory')
  assert.equal(result.outOfScope.length, 1)
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.ok(!/^BLOCKED/m.test(text), text)
  assert.match(
    text,
    /warning: 1 installed path\(s\) are missing from cores this run does not load \(boss-epic\); they decide nothing here: boss-epic\/toolbox\/epic-driver\.mjs \(absent, lossless\)/,
  )
  // Every advisory row is out of scope, so the work state really is unaffected.
  assert.ok(text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
})

test('scoping is load-bearing: the same absent row blocks in scope and in whole-tree mode', () => {
  const output = gate(row('boss-epic/toolbox/epic-driver.mjs', 'absent', 'lossless'))
  assert.equal(withCores(output, ['boss-epic']).verdict, 'blocking') // unknown consumer
  assert.equal(withCores(output, []).verdict, 'blocking')
  const inScope = withCores(gate(row('boss-plan/toolbox/x.mjs', 'absent', 'lossless')), [
    'boss-plan',
  ])
  assert.equal(inScope.verdict, 'blocking')
  assert.equal(inScope.blocking, true)
})

test('an unparseable row stays undecided whatever core its path names', () => {
  const result = withCores(gate('  - boss-epic/b.mjs'), ['boss-plan'])
  assert.equal(result.verdict, 'undecided')
  assert.equal(result.blocking, true)
})

test('a row with an unknown kind or direction stays undecided under an out-of-scope core', () => {
  for (const line of [
    row('boss-epic/x', 'new-kind', 'behind'),
    row('boss-epic/x', 'content', 'new-direction'),
  ]) {
    const result = withCores(gate(line), ['boss-plan'])
    assert.equal(result.verdict, 'undecided', line)
    assert.equal(result.reason, 'unreadable-drift-row', line)
    assert.equal(result.blocking, true, line)
    assert.equal(result.outOfScope.length, 0, line)
  }
})

test('the boss-build closure puts boss-review and boss-finalize rows in scope', () => {
  for (const core of ['boss-review', 'boss-finalize', 'boss-build']) {
    assert.equal(
      withCores(gate(row(`${core}/toolbox/x.mjs`, 'absent', 'lossless')), ['boss-build']).verdict,
      'blocking',
      core,
    )
  }
  assert.equal(
    withCores(gate(row('boss-epic/toolbox/x.mjs', 'absent', 'lossless')), ['boss-build']).blocking,
    false,
  )
})

test('the Go gate output blocks boss-build, names only in-scope rows, and drops the redundant rebuild line', () => {
  const result = withCores(GO_GATE_OUTPUT, ['boss-build'])
  assert.equal(result.rows.length, 3)
  assert.equal(result.verdict, 'blocking')
  assert.deepEqual(
    result.evidence.map((r) => r.path),
    ['boss-build/SKILL.md'],
  )
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.match(
    text,
    /^ {2}repair: make -C \/repo build plugins && BOSS_TRUST_CHECKOUT_SKILLS=1 \/repo\/bin\/boss skills install$/m,
  )
  // The command already rebuilds the plugins, so the daemon-start instruction is not repeated.
  assert.ok(!text.includes('restores it at daemon start'), text)
  assert.ok(!text.includes('boss/SKILL.md'), text)
})

test('the Go gate output is advisory for boss-plan, with its foreign absent row warned about', () => {
  const result = withCores(GO_GATE_OUTPUT, ['boss-plan'])
  assert.equal(result.blocking, false)
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.ok(text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
  assert.match(text, /missing from cores this run does not load \(boss-build\)/)
  assert.match(text, /run: make -C \/repo build plugins && /)
})

// A vendored copy laid out the way the installer lays it out: the namespaced payload plus a
// top-level link to it. main-module.mjs travels with the helper, exactly as vendoring ships it.
function vendoredCopy(core) {
  const root = mkdtempSync(path.join(tmpdir(), 'skill-drift-verdict-'))
  const toolbox = path.join(root, 'ns', core, 'toolbox')
  mkdirSync(toolbox, { recursive: true })
  const here = path.dirname(SCRIPT_PATH)
  for (const file of ['skill-drift-verdict.mjs', 'main-module.mjs']) {
    copyFileSync(path.join(here, file), path.join(toolbox, file))
  }
  const skills = path.join(root, 'skills')
  mkdirSync(skills)
  symlinkSync(path.join(root, 'ns', core), path.join(skills, core))
  return {
    root,
    direct: path.join(toolbox, 'skill-drift-verdict.mjs'),
    viaLink: path.join(skills, core, 'toolbox', 'skill-drift-verdict.mjs'),
  }
}

test('the running core is inferred from the vendored location, through a symlink too', (t) => {
  const copy = vendoredCopy('boss-plan')
  t.after(() => rmSync(copy.root, { recursive: true, force: true }))
  assert.equal(inferRunningCore(pathToFileURL(copy.direct).href), 'boss-plan')
  assert.equal(inferRunningCore(pathToFileURL(copy.viaLink).href), 'boss-plan')
  // The canonical source copy sits in no core's toolbox.
  assert.equal(inferRunningCore(pathToFileURL(SCRIPT_PATH).href), null)
  assert.equal(inferRunningCore('not a url'), null)

  const foreignAbsent = gate(row('boss-epic/toolbox/epic-driver.mjs', 'absent', 'lossless'))
  for (const script of [copy.direct, copy.viaLink]) {
    const run = spawnSync(process.execPath, [script, 'classify', '--status', '1'], {
      input: foreignAbsent,
      encoding: 'utf8',
    })
    assert.equal(run.status, 0, run.stdout + run.stderr)
    assert.match(run.stdout, /missing from cores this run does not load \(boss-epic\)/)
  }
  // The running core's own absent file blocks when its installed body names it.
  writeFileSync(path.join(copy.root, 'ns', 'boss-plan', 'SKILL.md'), 'Run toolbox/x.mjs.\n')
  const ownAbsent = spawnSync(process.execPath, [copy.viaLink, 'classify', '--status', '1'], {
    input: gate(row('boss-plan/toolbox/x.mjs', 'absent', 'lossless')),
    encoding: 'utf8',
  })
  assert.equal(ownAbsent.status, 1, ownAbsent.stdout)
  assert.match(ownAbsent.stdout, /^BLOCKED: /)
})

test('--core overrides inference, repeats, and a valueless --core widens to the whole tree', (t) => {
  const copy = vendoredCopy('boss-plan')
  t.after(() => rmSync(copy.root, { recursive: true, force: true }))
  const reviewAbsent = gate(row('boss-review/toolbox/x.mjs', 'absent', 'lossless'))
  const status = (args) =>
    spawnSync(process.execPath, [copy.direct, 'classify', '--status', '1', ...args], {
      input: reviewAbsent,
      encoding: 'utf8',
    }).status
  // Inferred boss-plan: boss-review is foreign.
  assert.equal(status([]), 0)
  // Overridden to boss-build, whose closure loads boss-review.
  assert.equal(status(['--core', 'boss-build']), 1)
  assert.equal(status(['--core', 'boss-plan', '--core', 'boss-build']), 1)
  // A dropped value must not quietly keep the narrower inferred scope.
  assert.equal(status(['--core']), 1)
  assert.equal(status(['--core', 'boss-unknown']), 1)
})

// ---------------------------------------------------------------------------
// Direction wording: only a row the checkout is ahead of may be called lagging

const DOWNGRADE_LEAD =
  '  reinstall withheld — the installed copy is newer than this checkout (it is on origin/HEAD); a reinstall from here would downgrade it:'
const DESTRUCTIVE_LEAD =
  '  reinstall withheld — it would overwrite installed content this checkout cannot restore:'

test('a content/ahead row is withheld and worded as newer, never as lagging', () => {
  const output = gate(
    row('boss-plan/toolbox/tracker/cli.mjs', 'content', 'ahead'),
    DOWNGRADE_LEAD,
    '    boss-plan/toolbox/tracker/cli.mjs (content, ahead)',
    '  next: update this checkout to origin/HEAD, then re-run this check',
  )
  const result = classifySkillDrift({ exitStatus: 1, output, cores: ['boss-plan'] })
  assert.equal(result.verdict, 'advisory-withheld')
  assert.equal(result.blocking, false)
  assert.equal(result.remedy.withheld, true)
  assert.equal(result.remedy.command, null)
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.ok(!/\blag\b/.test(text), text)
  assert.match(
    text,
    /installed copies are NEWER than this checkout \(it is behind origin\/HEAD\): boss-plan\/toolbox\/tracker\/cli\.mjs \(content, ahead\) — do not reinstall from this checkout; update it and re-run/,
  )
  assert.ok(!/run: /.test(text), text)
})

test('an unrecoverable or unknown row is never called lagging', () => {
  for (const direction of ['unrecoverable', 'unknown']) {
    const text = render(gate(row('a/b.mjs', 'content', direction), DESTRUCTIVE_LEAD))
    assert.ok(!/\blag\b/.test(text), `${direction}: ${text}`)
    assert.match(
      text,
      new RegExp(
        `installed copies differ from checkout source in a way this checkout cannot place: a/b\\.mjs \\(content, ${direction}\\)`,
      ),
      direction,
    )
  }
})

test('a behind row keeps the lag sentence byte for byte', () => {
  const text = render(gate(row('a/b.mjs', 'content', 'behind'), '  run `boss skills install`'))
  assert.equal(
    text,
    'warning: installed boss skills drift from checkout source; this run executes 1 installed ' +
      "path(s) that lag checkout source, so its behaviour can differ from the checkout's: " +
      'a/b.mjs (content, behind) — run: boss skills install',
  )
})

test('a mixed-direction report names each direction group in its own clause', () => {
  const text = render(
    gate(
      row('a/behind.mjs', 'content', 'behind'),
      row('a/ahead.mjs', 'content', 'ahead'),
      row('a/lost.mjs', 'unexpected', 'unrecoverable'),
      DESTRUCTIVE_LEAD,
    ),
  )
  const line = text.split('\n')[0]
  assert.match(
    line,
    /this run executes 1 installed path\(s\) that lag checkout source[^;]*a\/behind\.mjs/,
  )
  assert.match(line, /installed copies are NEWER than this checkout[^;]*a\/ahead\.mjs/)
  assert.match(line, /cannot place: a\/lost\.mjs \(unexpected, unrecoverable\)/)
  // Each path lands in its own group only.
  assert.ok(!/lag checkout source[^;]*a\/ahead\.mjs/.test(line), line)
  assert.ok(!/lag checkout source[^;]*a\/lost\.mjs/.test(line), line)
  assert.match(line, /withheld its reinstall remedy/)
})

test('a blocking report withheld only for a downgrade points at updating the checkout', () => {
  const text = render(
    gate(row('a/x.mjs', 'absent', 'lossless'), row('a/b.mjs', 'content', 'ahead'), DOWNGRADE_LEAD),
  )
  assert.match(text, /^BLOCKED: /)
  assert.match(text, /repair: .*update the checkout to origin\/HEAD/)
  assert.ok(!text.includes('named as unrecoverable'), text)
  // With a genuine loss beside it, the destructive repair wording stays.
  const mixed = render(
    gate(
      row('a/x.mjs', 'absent', 'lossless'),
      row('a/b.mjs', 'content', 'ahead'),
      row('a/c.mjs', 'content', 'unrecoverable'),
      DESTRUCTIVE_LEAD,
    ),
  )
  assert.match(mixed, /named as unrecoverable first/)
})

// ---------------------------------------------------------------------------
// Unreferenced absent files: unreleased helpers the executed body never calls

const UNRELEASED = 'boss-plan/toolbox/unreleased-helper.mjs'

test('an in-scope absent file the installed cores never reference is advisory', () => {
  const asked = []
  const result = classifySkillDrift({
    exitStatus: 1,
    output: gate(row(UNRELEASED, 'absent', 'lossless'), '  run `boss skills install`'),
    cores: ['boss-plan'],
    isReferenced: (p) => {
      asked.push(p)
      return false
    },
  })
  assert.deepEqual(asked, [UNRELEASED])
  assert.equal(result.verdict, 'advisory')
  assert.equal(result.reason, 'unreferenced-absent')
  assert.equal(result.blocking, false)
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.ok(!/^BLOCKED/m.test(text), text)
  assert.match(
    text,
    /warning: checkout adds 1 file\(s\) the installed boss-plan never reference \(unreleased; they decide nothing here\): boss-plan\/toolbox\/unreleased-helper\.mjs \(absent, lossless\)/,
  )
})

test('an absent file stays blocking when referenced, unknown, throwing, or out of the absent kind', () => {
  const output = gate(row(UNRELEASED, 'absent', 'lossless'))
  for (const [label, isReferenced] of [
    ['referenced', () => true],
    ['unknown', () => null],
    ['default', undefined],
    [
      'throws',
      () => {
        throw new Error('scan failed')
      },
    ],
    ['non-boolean', () => 0],
  ]) {
    const result = classifySkillDrift({ exitStatus: 1, output, cores: ['boss-plan'], isReferenced })
    assert.equal(result.verdict, 'blocking', label)
    assert.equal(result.reason, 'absent-capability', label)
  }
  // mode and broken-symlink rows, and an absent directory key, are never downgraded.
  for (const line of [
    row(UNRELEASED, 'mode', 'lossless'),
    row(UNRELEASED, 'broken-symlink', 'lossless'),
    row('boss-plan/toolbox/newdir/', 'absent', 'lossless'),
    row('boss-plan/SKILL.md', 'absent', 'lossless'),
    row('boss-plan/references/SKILL.md', 'absent', 'lossless'),
  ]) {
    const result = classifySkillDrift({
      exitStatus: 1,
      output: gate(line),
      cores: ['boss-plan'],
      isReferenced: () => false,
    })
    assert.equal(result.verdict, 'blocking', line)
  }
})

test('an unreferenced absent row beside a referenced one still blocks on the referenced one', () => {
  const result = classifySkillDrift({
    exitStatus: 1,
    output: gate(
      row(UNRELEASED, 'absent', 'lossless'),
      row('boss-plan/toolbox/used.mjs', 'absent', 'lossless'),
    ),
    cores: ['boss-plan'],
    isReferenced: (p) => p.endsWith('used.mjs'),
  })
  assert.equal(result.verdict, 'blocking')
  assert.deepEqual(
    result.evidence.map((r) => r.path),
    ['boss-plan/toolbox/used.mjs'],
  )
  const text = renderSkillDriftVerdict(result).join('\n')
  assert.match(text, /checkout adds 1 file\(s\) the installed boss-plan never reference/)
})

test('the real reference checker answers null from the canonical source copy', () => {
  const check = installedReferenceChecker(pathToFileURL(SCRIPT_PATH).href, ['boss-plan'])
  assert.equal(check(UNRELEASED), null)
})

test('the real reference checker answers null for whole-tree scope', (t) => {
  const copy = vendoredCopy('boss-plan')
  t.after(() => rmSync(copy.root, { recursive: true, force: true }))
  const check = installedReferenceChecker(pathToFileURL(copy.direct).href, [null])
  assert.equal(check(UNRELEASED), null)
})

test('the real reference checker answers null when a core in scope is not installed', (t) => {
  const copy = vendoredCopy('boss-build')
  t.after(() => rmSync(copy.root, { recursive: true, force: true }))
  // boss-build's closure also loads boss-review and boss-finalize, which this fixture lacks.
  const check = installedReferenceChecker(pathToFileURL(copy.direct).href, ['boss-build'])
  assert.equal(check('boss-build/toolbox/unreleased-helper.mjs'), null)
})

test('the CLI over a vendored copy downgrades only an absent file its SKILL.md never names', (t) => {
  const copy = vendoredCopy('boss-plan')
  t.after(() => rmSync(copy.root, { recursive: true, force: true }))
  const skillMd = path.join(copy.root, 'ns', 'boss-plan', 'SKILL.md')
  const input = gate(row(UNRELEASED, 'absent', 'lossless'))
  const run = (script) =>
    spawnSync(process.execPath, [script, 'classify', '--status', '1'], { input, encoding: 'utf8' })

  writeFileSync(skillMd, '# boss-plan\n\nRun "$BOSS_PLAN_TOOLBOX/other.mjs".\n')
  for (const script of [copy.direct, copy.viaLink]) {
    const unreferenced = run(script)
    assert.equal(unreferenced.status, 0, unreferenced.stdout + unreferenced.stderr)
    assert.match(
      unreferenced.stdout,
      /checkout adds 1 file\(s\) the installed boss-plan never reference/,
    )
    assert.ok(!/^BLOCKED/m.test(unreferenced.stdout), unreferenced.stdout)
  }

  writeFileSync(skillMd, '# boss-plan\n\nRun "$BOSS_PLAN_TOOLBOX/unreleased-helper.mjs".\n')
  const referenced = run(copy.viaLink)
  assert.equal(referenced.status, 1, referenced.stdout)
  assert.match(referenced.stdout, /^BLOCKED: /)

  // A reference in a file type outside any extension allowlist still counts.
  writeFileSync(skillMd, '# boss-plan\n')
  const script = path.join(copy.root, 'ns', 'boss-plan', 'toolbox', 'run-step')
  writeFileSync(
    script,
    '#!/usr/bin/env python3\nimport unreleased_helper  # unreleased-helper.mjs\n',
  )
  const extensionless = run(copy.viaLink)
  assert.equal(extensionless.status, 1, extensionless.stdout)
  assert.match(extensionless.stdout, /^BLOCKED: /)
  rmSync(script)

  // The canonical source copy has no installed namespace to read, so it fails closed.
  writeFileSync(skillMd, '# boss-plan\n')
  const canonical = spawnSync(
    process.execPath,
    [SCRIPT_PATH, 'classify', '--status', '1', '--core', 'boss-plan'],
    { input, encoding: 'utf8' },
  )
  assert.equal(canonical.status, 1, canonical.stdout)
  assert.match(canonical.stdout, /^BLOCKED: /)
})

// ---------------------------------------------------------------------------
// Published-core portability

test('the helper stays publishable: node builtins and main-module.mjs only, no project identity', () => {
  const source = readFileSync(SCRIPT_PATH, 'utf8')
  const imports = [...source.matchAll(/^import .*? from '([^']+)'$/gm)].map((m) => m[1])
  assert.deepEqual([...new Set(imports)].sort(), [
    './main-module.mjs',
    'node:fs',
    'node:path',
    'node:url',
  ])
  // A vendored core ships into every repo on the machine, so a tracker id, a project MCP server
  // or a repo path here would leak out of this project entirely.
  assert.ok(!/bossanova|mcp__|Internal Bossanova/i.test(source), 'project identity leaked')
  assert.ok(!/\b[A-Z]{2,5}-\d{2,}\b/.test(source), 'tracker id leaked')
})
