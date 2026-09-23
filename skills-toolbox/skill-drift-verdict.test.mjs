#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  SKILL_DRIFT_ADVISORY_SENTENCE,
  SKILL_DRIFT_DIRECTIONS,
  SKILL_DRIFT_KINDS,
  SKILL_DRIFT_VERDICTS,
  classifySkillDrift,
  isUnsupportedFlagOutput,
  main,
  parseSkillGateOutput,
  renderSkillDriftVerdict,
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

test('the kind and direction vocabularies match the gate: exactly five kinds, four directions', () => {
  assert.deepEqual([...SKILL_DRIFT_KINDS].sort(), [
    'absent',
    'broken-symlink',
    'content',
    'mode',
    'unexpected',
  ])
  assert.deepEqual([...SKILL_DRIFT_DIRECTIONS].sort(), [
    'behind',
    'lossless',
    'unknown',
    'unrecoverable',
  ])
})

// ---------------------------------------------------------------------------
// Totality: every kind x every direction

test('every one of the five kinds x four directions maps to a verdict, with no pair unasserted', () => {
  // The table is written out in full rather than derived from the same sets the module uses: a
  // table generated from CAPABILITY_KINDS would agree with any reshuffling of it.
  const expected = {
    'absent|lossless': 'blocking',
    'absent|behind': 'blocking',
    'absent|unrecoverable': 'blocking',
    'absent|unknown': 'blocking',
    'mode|lossless': 'blocking',
    'mode|behind': 'blocking',
    'mode|unrecoverable': 'blocking',
    'mode|unknown': 'blocking',
    'broken-symlink|lossless': 'blocking',
    'broken-symlink|behind': 'blocking',
    'broken-symlink|unrecoverable': 'blocking',
    'broken-symlink|unknown': 'blocking',
    'content|lossless': 'advisory',
    'content|behind': 'advisory',
    'content|unrecoverable': 'advisory-withheld',
    'content|unknown': 'advisory-withheld',
    'unexpected|lossless': 'advisory',
    'unexpected|behind': 'advisory',
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
  assert.equal(pairs.length, 20)
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

test('a content/behind row is advisory and keeps the greppable advisory sentence', () => {
  const output = gate(row('boss-epic/SKILL.md', 'content', 'behind'), '  run `boss skills install`')
  const result = classify(output)
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
  const text = render(gate(row('a/b.mjs', 'content', 'behind')))
  assert.equal(
    text,
    'warning: installed boss skills drift from checkout source; see gate output above — ' +
      'bookkeeping only, work state unaffected',
  )
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
  assert.ok(text.includes(SKILL_DRIFT_ADVISORY_SENTENCE), text)
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
  const advisory = main(['classify', '--status', '1'], {
    stdin: gate(row('a/b.mjs', 'content', 'behind')),
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
// Published-core portability

test('the helper stays publishable: node builtins and main-module.mjs only, no project identity', () => {
  const source = readFileSync(SCRIPT_PATH, 'utf8')
  const imports = [...source.matchAll(/^import .*? from '([^']+)'$/gm)].map((m) => m[1])
  assert.deepEqual([...new Set(imports)].sort(), ['./main-module.mjs', 'node:fs'])
  // A vendored core ships into every repo on the machine, so a tracker id, a project MCP server
  // or a repo path here would leak out of this project entirely.
  assert.ok(!/bossanova|mcp__|Internal Bossanova/i.test(source), 'project identity leaked')
  assert.ok(!/\b[A-Z]{2,5}-\d{2,}\b/.test(source), 'tracker id leaked')
})
