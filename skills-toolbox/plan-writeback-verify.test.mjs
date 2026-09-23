#!/usr/bin/env node

// Unit + CLI coverage for skills-toolbox/plan-writeback-verify.mjs (BOS-1199).
//
// The helper MEASURES round-trip fidelity of a description write instead of assuming it. Two
// recorded field beliefs contradict each other — one says the tracker normalizes markdown on write
// so a description never round-trips, the other says the normalization belongs to the write
// transport and a raw write does round-trip byte-identically — and this suite exists so neither is
// encoded. Tier 1 is the outcome the raw-transport claim predicts, tier 2 the outcome the
// normalizing-transport claim predicts, and the run reports which one it observed.
//
// The tier BOUNDARIES are the whole product here, so they are pinned first and each of the three
// ways tier 2 collapses to tier 3 has its own named test. Node builtins only — cron worktrees are
// dependency-free.

import assert from 'node:assert/strict'
import test from 'node:test'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { region } from '../scripts/gate-region-lib.mjs'

import {
  DEFAULT_CONFIG,
  DESCRIPTION_NORMALIZATION_TRANSFORMS,
  mergeConfig,
} from './skill-config.mjs'
import {
  DESCRIPTION_TRANSFORM_NORMALIZERS,
  WRITEBACK_DESCRIPTION_MODES,
  WRITEBACK_VERDICTS,
  WRITEBACK_CAUSES,
  normalizeDescription,
  parseWritebackVerifyArgs,
  verifyWriteback,
} from './plan-writeback-verify.mjs'

// Pin this suite's gate-outcome destination. Why, and the test enforcing it: gate-outcome.test.mjs.
process.env.BOSS_GATE_OUTCOME_FILE = path.join(
  mkdtempSync(path.join(tmpdir(), 'gate-outcome-suite-')),
  'outcomes.tsv',
)
const HELPER = fileURLToPath(new URL('./plan-writeback-verify.mjs', import.meta.url))
const SKILL_CONFIG_SOURCE = fileURLToPath(new URL('./skill-config.mjs', import.meta.url))
const UPLOAD = 'https://uploads.linear.app/abc-123/screenshot.png'

// Derived, never restated. A hand-copied list here would be a fifth place the 5-id vocabulary is
// written down, and the one place a missing id looks like deliberate scope rather than drift.
const ALL_TRANSFORMS = new Set(DESCRIPTION_NORMALIZATION_TRANSFORMS)

const NOTES = `Reporter context.\n\n- first observation\n- second observation\n`

/**
 * The durable invariant for a difference the transform vocabulary cannot account for: it must never
 * be CERTIFIED as a clean round trip.
 *
 * Asserted this way rather than by pinning a verdict name, because the verdict name is exactly what
 * moved. These cases used to be `drift` and are now `unattributed` — advisory rather than fatal,
 * since every content conjunct (contract, uploads, verbatim block) has already passed on the same
 * bytes. What each of these tests is really defending is the tier-2 boundary: an undeclared
 * reshaping must not be laundered into `normalized-equivalent` or `byte-exact`. That is unchanged,
 * and it is what this helper pins, so a future severity change cannot quietly erode it.
 */
function assertNotCertifiedEquivalent(result) {
  assert.equal(result.verdict, WRITEBACK_VERDICTS.UNATTRIBUTED)
  assert.notEqual(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.notEqual(result.verdict, WRITEBACK_VERDICTS.BYTE_EXACT)
  assert.equal(result.cause, WRITEBACK_CAUSES.UNATTRIBUTED)
}

/** A minimal description that satisfies the DEFAULT_CONFIG child-plan contract. */
function description({ notes = NOTES, sections = {} } = {}) {
  const body = (heading, fallback) => sections[heading] ?? fallback
  return [
    `## Summary\n\n${body('## Summary', 'Measure write-back fidelity instead of assuming it.')}`,
    `## Approach\n\n${body('## Approach', 'Read the stored description back and compare.')}`,
    `## Key changes\n\n${body('## Key changes', `- \`skills-toolbox/plan-writeback-verify.mjs\`\n\n![shot](${UPLOAD})`)}`,
    `## Testing\n\n${body('## Testing', '- unit coverage over every tier boundary')}`,
    `## Risks / unknowns\n\n${body('## Risks / unknowns', '- a tolerance set that is too wide')}`,
    `## Acceptance criteria\n\n${body('## Acceptance criteria', '- [ ] the verdict is measured')}`,
    `## Required proof\n\n${body('## Required proof', '- [ ] (backend-only) no screenshot applicable')}`,
    `## Planning\n\n${body('## Planning', '- Contract: v1')}`,
    `## Original notes\n\n${notes}`,
  ].join('\n\n')
}

const configWith = (tolerated) =>
  mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        descriptionNormalization: { tolerated: [...tolerated] },
      },
    },
  })

const verify = (intendedText, storedText, tolerated = ALL_TRANSFORMS) =>
  verifyWriteback({ config: configWith(tolerated), intendedText, storedText })

// ---------------------------------------------------------------------------
// Tier boundaries.
// ---------------------------------------------------------------------------

test('tier 1: identical intended and stored text yields byte-exact and exit zero', () => {
  const text = description()
  const result = verify(text, text)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.BYTE_EXACT)
  assert.equal(result.exitCode, 0)
})

test('tier 2: a declared marker substitution yields normalized-equivalent and exit zero', () => {
  const intended = description()
  const stored = intended.replace(/^- /gm, '* ')
  assert.notEqual(stored, intended, 'the fixture must actually differ')
  const result = verify(intended, stored, ['unordered-list-marker-substitution'])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(result.exitCode, 0)
})

test('tier 3: a marker substitution absent from the declared set yields drift', () => {
  // Proves the tolerance set is load-bearing rather than decorative: the SAME bytes that pass above
  // must fail once the transform is not declared.
  const intended = description()
  const stored = intended.replace(/^- /gm, '* ')
  const result = verify(intended, stored, ['terminal-newline-trimming'])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.notEqual(result.exitCode, 0)
})

test('tier 2: a declared emphasis-delimiter whitespace migration is tolerated', () => {
  const intended = description({ sections: { '## Summary': 'A **1.** step and more prose.' } })
  const stored = intended.replace('**1.** step', '**1. ** step')
  assert.notEqual(stored, intended)
  const result = verify(intended, stored, ['emphasis-delimiter-whitespace-migration'])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
})

test('tier 2: a declared emphasis-span restructuring around an inline code span is tolerated', () => {
  // Measured at plan time: the tracker rewrote a bold span containing an inline code span into
  // bold + code + bold, adding four bytes. It renders identically and is not corruption.
  const intended = description({
    sections: { '## Summary': 'The **helper `plan-writeback-verify.mjs` runs once** per run.' },
  })
  const stored = intended.replace(
    '**helper `plan-writeback-verify.mjs` runs once**',
    '**helper **`plan-writeback-verify.mjs`** runs once**',
  )
  assert.notEqual(stored, intended)
  const result = verify(intended, stored, ['emphasis-span-restructuring'])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(result.exitCode, 0)
})

test('tier 3 via the semantic conjunct: declared transforms only, but a section is missing', () => {
  const intended = description()
  const stored = intended.replace(/^- /gm, '* ').replace(/## Required proof\n\n[^\n]*\n\n/, '')
  const result = verify(intended, stored)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.match(result.reason, /contract/i)
  assert.match(result.reason, /## Required proof/)
})

test('tier 3 via the asset conjunct: declared transforms only, but an upload identity is lost', () => {
  const intended = description()
  const stored = intended.replace(/^- /gm, '* ').replace(`![shot](${UPLOAD})`, '[screenshot]')
  const result = verify(intended, stored)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.match(result.reason, /upload identit/i)
  assert.ok(result.reason.includes(UPLOAD), 'the lost identity must be named')
})

test('tier 3 via the notes conjunct: declared transforms only, but the verbatim block lost a line', () => {
  const intended = description()
  const stored = intended.replace(/^- /gm, '* ').replace('* second observation\n', '')
  const result = verify(intended, stored)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.match(result.reason, /Original notes/)
})

test('tier 3: a literal corrupted emphasis run is drift, never a tolerated transform', () => {
  // The observed corruption: an emphasis span whose delimiters sit on different source lines stores
  // as a literal `****`. Every transform is declared here, so this pins that the vocabulary cannot
  // launder corruption into tier 2.
  const intended = description({
    sections: { '## Summary': 'found **7 of the 17\nalready fixed** and more prose.' },
  })
  const stored = intended.replace(
    'found **7 of the 17\nalready fixed**',
    'found **7 of the 17****\n****already fixed**',
  )
  const result = verify(intended, stored)
  assertNotCertifiedEquivalent(result)
})

test('a drift verdict names the differing line and column', () => {
  const intended = description()
  const stored = intended.replace('Measure write-back', 'Measure writeback')
  const result = verify(intended, stored)
  assertNotCertifiedEquivalent(result)
  assert.match(result.reason, /line \d+, column \d+/)
  assert.equal(result.line, 3, 'the differing line is the Summary body')
  assert.ok(result.column > 0)
})

// ---------------------------------------------------------------------------
// Refusals: an unverifiable input is never a pass and never drift.
// ---------------------------------------------------------------------------

test('an empty stored description is refused rather than reported as a vacuous pass', () => {
  const result = verify('', '')
  assert.equal(result.verdict, null, 'a refusal emits no verdict at all')
  assert.notEqual(result.exitCode, 0)
  assert.match(result.reason, /empty/)
})

// ---------------------------------------------------------------------------
// normalizeDescription — the transform vocabulary itself.
// ---------------------------------------------------------------------------

test('normalizeDescription applies only the declared transforms', () => {
  const text = '- one\n- two   \n'
  assert.equal(normalizeDescription(text, new Set()), text, 'an empty set is the identity')
  assert.equal(
    normalizeDescription(text, new Set(['unordered-list-marker-substitution'])),
    '* one\n* two   \n',
  )
  assert.equal(
    normalizeDescription(text, new Set(['trailing-whitespace-trimming'])),
    '- one\n- two\n',
  )
  assert.equal(normalizeDescription('body\n\n\n', new Set(['terminal-newline-trimming'])), 'body')
})

test('normalizeDescription is idempotent for every transform in the vocabulary', () => {
  const text = '- a **1.** b **c `d` e** f   \n\n\n'
  const once = normalizeDescription(text, ALL_TRANSFORMS)
  assert.equal(normalizeDescription(once, ALL_TRANSFORMS), once)
})

// ---------------------------------------------------------------------------
// CLI.
// ---------------------------------------------------------------------------

test('parseWritebackVerifyArgs requires both inputs', () => {
  assert.deepEqual(parseWritebackVerifyArgs(['--intended', '/tmp/a.md', '--stored', '/tmp/b.md']), {
    intended: '/tmp/a.md',
    stored: '/tmp/b.md',
    mode: 'child-plan',
  })
  assert.throws(() => parseWritebackVerifyArgs(['--stored', '/tmp/b.md']), /--intended/)
  assert.throws(() => parseWritebackVerifyArgs(['--intended', '/tmp/a.md']), /--stored/)
})

function runCli(intendedText, storedText, { omitStored = false } = {}) {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-writeback-verify-'))
  const intended = path.join(dir, 'intended.md')
  const stored = path.join(dir, 'stored.md')
  writeFileSync(intended, intendedText)
  if (!omitStored) writeFileSync(stored, storedText)
  const res = spawnSync(
    process.execPath,
    [HELPER, '--intended', intended, '--stored', stored],
    // Run from the repo root so the CLI resolves this repo's declared tolerance set.
    { encoding: 'utf8', cwd: fileURLToPath(new URL('..', import.meta.url)) },
  )
  return { res, intended, stored }
}

test('CLI: byte-identical inputs print one machine verdict line and exit zero', () => {
  const text = description()
  const { res } = runCli(text, text)
  assert.equal(res.status, 0)
  assert.match(res.stdout, /^writeback-verdict: byte-exact$/m)
  const verdictLines = res.stdout
    .split('\n')
    .filter((line) => line.startsWith('writeback-verdict:'))
  assert.equal(verdictLines.length, 1, 'exactly one verdict line')
})

test('CLI: a declared transform prints normalized-equivalent and exits zero', () => {
  // This repo declares the marker substitution, so the CLI must reach tier 2 from real config.
  const intended = description()
  const stored = intended.replace(/^- /gm, '* ')
  const { res } = runCli(intended, stored)
  assert.equal(res.status, 0, res.stderr)
  assert.match(res.stdout, /^writeback-verdict: normalized-equivalent$/m)
})

test('CLI: a content-loss run exits non-zero and leaves both input files byte-unmodified', () => {
  // Retargeted onto a CONTENT-loss fixture. The original changed one word of Summary prose, which
  // is now the advisory `unattributed` verdict; the guarantee this test exists for — the helper
  // never writes anything, whatever it decides — belongs on the branch that still fails.
  const intended = description()
  const stored = intended.replace('first observation', 'FIRST observation')
  const { res, intended: intendedPath, stored: storedPath } = runCli(intended, stored)
  assert.notEqual(res.status, 0)
  assert.match(res.stdout, /^writeback-verdict: drift$/m)
  assert.match(res.stderr, /line \d+, column \d+/)
  assert.equal(readFileSync(intendedPath, 'utf8'), intended, 'no corrective rewrite of the intent')
  assert.equal(readFileSync(storedPath, 'utf8'), stored, 'no corrective rewrite of the stored text')
})

test('CLI: an advisory run exits ZERO, still says so on stderr, and rewrites nothing', () => {
  // The severity split's headline behaviour: a difference with no detected content loss no longer
  // strands a run whose tracker writes have all landed. It must still be audible — a silent pass
  // here would be a gate that stopped reporting rather than a gate that stopped over-reacting.
  const intended = description()
  const stored = intended.replace('Measure write-back', 'Measure writeback')
  const { res, intended: intendedPath, stored: storedPath } = runCli(intended, stored)
  assert.equal(res.status, 0, res.stderr)
  assert.match(res.stdout, /^writeback-verdict: unattributed$/m)
  assert.match(res.stderr, /^writeback-verdict: unattributed$/m)
  assert.match(res.stderr, /do NOT attempt a corrective rewrite/)
  assert.ok(!res.stdout.includes('verdict: drift'), 'no content loss was detected, so not drift')
  assert.equal(readFileSync(intendedPath, 'utf8'), intended, 'no corrective rewrite of the intent')
  assert.equal(readFileSync(storedPath, 'utf8'), stored, 'no corrective rewrite of the stored text')
})

test('CLI: an absent stored file reports a read failure, never drift and never a pass', () => {
  const { res } = runCli(description(), '', { omitStored: true })
  assert.notEqual(res.status, 0)
  assert.match(res.stderr, /cannot read stored description/)
  assert.ok(!res.stdout.includes('writeback-verdict:'), 'a read failure emits no verdict')
  assert.ok(!res.stderr.includes('drift'), 'a read failure must not be reported as drift')
})

test('CLI: an empty stored file reports the refusal, never drift and never a pass', () => {
  const { res } = runCli(description(), '')
  assert.notEqual(res.status, 0)
  assert.match(res.stderr, /stored description is empty/)
  assert.ok(!res.stdout.includes('writeback-verdict:'), 'a refusal emits no verdict')
  assert.ok(!res.stderr.includes('drift'), 'a refusal must not be reported as drift')
})

// ---------------------------------------------------------------------------
// The emphasis-span-restructuring canonicalizer must PRESERVE emphasis.
//
// The first spelling of this transform deleted every delimiter run adjacent to a backtick,
// unconditionally. That made a stored description which genuinely LOST the bold around an inline
// code span canonicalize identically to the intended text, so the fidelity gate certified real
// content loss as `normalized-equivalent` and exited zero. These tests pin the asymmetry: a MOVED
// delimiter is tolerated, a DELETED one is drift.
// ---------------------------------------------------------------------------

const emphasis = (summary) => description({ sections: { '## Summary': summary } })

test('emphasis-span-restructuring: a split span and the single span it came from agree', () => {
  const intended = 'The **helper `plan-writeback-verify.mjs` runs once** per run.'
  const stored = 'The **helper **`plan-writeback-verify.mjs`** runs once** per run.'
  assert.notEqual(intended, stored)
  assert.equal(
    normalizeDescription(intended, new Set(['emphasis-span-restructuring'])),
    normalizeDescription(stored, new Set(['emphasis-span-restructuring'])),
  )
})

test('emphasis-span-restructuring: DELETED emphasis around a code span is NOT canonicalized away', () => {
  // The regression. Both reviewers measured this exact pair reducing to one string.
  const tolerated = new Set(['emphasis-span-restructuring'])
  for (const [intended, stored] of [
    ['The **`foo.mjs`** helper.', 'The `foo.mjs` helper.'],
    ['see **`code`** here', 'see `code` here'],
    ['a _`c`_ b', 'a `c` b'],
  ]) {
    assert.notEqual(
      normalizeDescription(intended, tolerated),
      normalizeDescription(stored, tolerated),
      `losing the emphasis around \`${intended}\` must survive canonicalization as a difference`,
    )
  }
})

test('emphasis-span-restructuring: bold demoted to italic around a code span is NOT canonicalized away', () => {
  // The fence that keeps the engine from backtracking a `**` run down to a single `*`. Without it
  // `**`x`**` rewrote to `*`x`*` and collided with a genuine italic.
  const tolerated = new Set(['emphasis-span-restructuring'])
  assert.notEqual(
    normalizeDescription('The **`foo.mjs`** helper.', tolerated),
    normalizeDescription('The *`foo.mjs`* helper.', tolerated),
  )
})

test('emphasis-span-restructuring is idempotent', () => {
  const tolerated = new Set(['emphasis-span-restructuring'])
  const once = normalizeDescription('The **a **`c`** b** end.', tolerated)
  assert.equal(normalizeDescription(once, tolerated), once)
})

test('tier 3: a stored description that LOST emphasis around a code span is drift, not tier 2', () => {
  // End-to-end through the verdict, with the transform DECLARED — the shape this repo opted into.
  const intended = emphasis('The **`plan-writeback-verify.mjs`** helper runs once.')
  const stored = intended.replace('**`plan-writeback-verify.mjs`**', '`plan-writeback-verify.mjs`')
  assert.notEqual(stored, intended)
  const result = verify(intended, stored, ['emphasis-span-restructuring'])
  assertNotCertifiedEquivalent(result)
})

// ---------------------------------------------------------------------------
// The `mode` seam.
// ---------------------------------------------------------------------------

test('verifyWriteback defaults to the child-plan contract', () => {
  const text = description()
  assert.equal(verify(text, text).verdict, WRITEBACK_VERDICTS.BYTE_EXACT)
})

test('verifyWriteback rejects an unknown mode instead of falling back to child-plan', () => {
  const text = description()
  assert.throws(
    () =>
      verifyWriteback({
        config: configWith(ALL_TRANSFORMS),
        intendedText: text,
        storedText: text,
        mode: 'parent',
      }),
    /--mode must be one of/,
  )
})

test('verifyWriteback in epic-parent mode validates against the PARENT contract, not the child one', () => {
  // The finding: a CORRECTLY stored epic-parent overview was adjudicated against the hard-coded
  // child-plan contract, so it came back `drift` at the worst possible moment — already stored,
  // non-zero exit, scratch retained, corrective rewrite forbidden. Same bytes, two modes, two
  // verdicts is the whole point of the seam.
  const parent = [
    '## Summary\n\nDecompose the epic into shippable children.',
    '## Child tickets\n\n- one child per shippable slice',
    '## Planning\n\n- Contract: v1',
    `## Original notes\n\n${NOTES}`,
  ].join('\n\n')
  const stored = parent.replace(/^- /gm, '* ')
  assert.notEqual(stored, parent, 'the fixture must actually differ')

  const asChild = verifyWriteback({
    config: configWith(ALL_TRANSFORMS),
    intendedText: parent,
    storedText: stored,
    mode: 'child-plan',
  })
  assert.equal(asChild.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.match(asChild.reason, /contract/i)

  const asParent = verifyWriteback({
    config: configWith(ALL_TRANSFORMS),
    intendedText: parent,
    storedText: stored,
    mode: 'epic-parent',
  })
  assert.equal(asParent.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(asParent.exitCode, 0)
  assert.deepEqual(WRITEBACK_DESCRIPTION_MODES, ['child-plan', 'epic-parent'])
})

test('parseWritebackVerifyArgs defaults mode to child-plan and accepts --mode epic-parent', () => {
  assert.equal(parseWritebackVerifyArgs(['--intended', 'a', '--stored', 'b']).mode, 'child-plan')
  assert.equal(
    parseWritebackVerifyArgs(['--intended', 'a', '--stored', 'b', '--mode', 'epic-parent']).mode,
    'epic-parent',
  )
  assert.throws(
    () => parseWritebackVerifyArgs(['--intended', 'a', '--stored', 'b', '--mode', 'nonsense']),
    /--mode must be one of child-plan, epic-parent/,
  )
})

// ---------------------------------------------------------------------------
// The transform vocabulary is single-sourced.
// ---------------------------------------------------------------------------

test('every declarable transform id has a normalizer, and every normalizer is declarable', () => {
  // Bidirectional. A vocabulary id with no normalizer validates in config, is declarable as
  // tolerated, and is then SILENTLY INERT; a normalizer with no id can never be reached.
  assert.deepEqual(
    Object.keys(DESCRIPTION_TRANSFORM_NORMALIZERS).sort(),
    [...DESCRIPTION_NORMALIZATION_TRANSFORMS].sort(),
  )
  for (const id of DESCRIPTION_NORMALIZATION_TRANSFORMS) {
    assert.equal(
      typeof DESCRIPTION_TRANSFORM_NORMALIZERS[id],
      'function',
      `${id} needs a normalizer`,
    )
  }
})

test('every declarable transform id is documented in the skill-config vocabulary JSDoc', () => {
  // The third hand-written restatement is prose. Gate it too: an id added to the array without a
  // bullet leaves the only human-readable definition of the tolerance silently incomplete.
  const source = readFileSync(SKILL_CONFIG_SOURCE, 'utf8')
  // region() throws when either marker moves, instead of indexOf's -1 silently yielding a slice
  // that still satisfies the loop below. An extraction that cannot fail cannot gate anything —
  // the same defect class this helper exists to catch on the tracker's side.
  const doc = region(
    source,
    'The CLOSED vocabulary of description-normalization transform ids',
    'export const DESCRIPTION_NORMALIZATION_TRANSFORMS',
    'skill-config.mjs vocabulary JSDoc',
  )
  for (const id of DESCRIPTION_NORMALIZATION_TRANSFORMS) {
    assert.ok(doc.includes(`\`${id}\``), `${id} is undocumented in the vocabulary JSDoc`)
  }
})

// ---------------------------------------------------------------------------
// BOS-1209: one gate-outcome line per invocation, reusing the three verdicts as
// its reason vocabulary. Recording is telemetry, so each case also asserts the
// verdict line and exit code a caller reads.
// ---------------------------------------------------------------------------

function runCliRecording(intendedText, storedText, outcomes) {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-writeback-verify-outcomes-'))
  const intended = path.join(dir, 'intended.md')
  const stored = path.join(dir, 'stored.md')
  writeFileSync(intended, intendedText)
  writeFileSync(stored, storedText)
  return spawnSync(process.execPath, [HELPER, '--intended', intended, '--stored', stored], {
    encoding: 'utf8',
    cwd: fileURLToPath(new URL('..', import.meta.url)),
    env: { ...process.env, BOSS_GATE_OUTCOME_FILE: outcomes },
  })
}

test('records exactly one gate-outcome line per invocation without changing the verdict', () => {
  const outcomes = path.join(
    mkdtempSync(path.join(tmpdir(), 'plan-writeback-verify-record-')),
    'outcomes.tsv',
  )
  const read = () =>
    readFileSync(outcomes, 'utf8')
      .split('\n')
      .filter((line) => line !== '')
      .map((line) => line.split('\t').slice(1))

  // The recorded reason is the CAUSE, not the verdict. A bare `drift` could not distinguish a lost
  // section from a bullet rewrite, so the question "has this gate ever caught real content loss?"
  // could only be answered by hand-diffing retained scratch directories — which is how the 55%
  // false-fire rate went unnoticed. Each conjunct now names itself in telemetry.
  const text = description()
  const pass = runCliRecording(text, text, outcomes)
  assert.equal(pass.status, 0, pass.stderr)
  assert.match(pass.stdout, /^writeback-verdict: byte-exact$/m)
  assert.deepEqual(read(), [['plan-writeback-verify', 'pass', 'equal']])

  // An advisory difference records a PASS carrying its own cause, so it stays countable.
  const advisory = runCliRecording(
    text,
    text.replace('Measure write-back', 'Measure writeback'),
    outcomes,
  )
  assert.equal(advisory.status, 0, advisory.stderr)
  assert.match(advisory.stdout, /^writeback-verdict: unattributed$/m)

  // Content loss records a FIRE naming which conjunct caught it.
  const fire = runCliRecording(
    text,
    text.replace('first observation', 'FIRST observation'),
    outcomes,
  )
  assert.notEqual(fire.status, 0)
  assert.match(fire.stdout, /^writeback-verdict: drift$/m)
  assert.deepEqual(read(), [
    ['plan-writeback-verify', 'pass', 'equal'],
    ['plan-writeback-verify', 'pass', 'unattributed'],
    ['plan-writeback-verify', 'fire', 'notes'],
  ])
})

test('each content-loss conjunct records its own distinct cause', () => {
  // The point of the cause vocabulary: three different failures that used to be indistinguishable
  // in telemetry are now three different tokens.
  const outcomes = path.join(
    mkdtempSync(path.join(tmpdir(), 'plan-writeback-verify-causes-')),
    'outcomes.tsv',
  )
  const causeOf = (stored) => {
    const res = runCliRecording(description(), stored, outcomes)
    assert.notEqual(res.status, 0, 'content loss must stay fatal')
    return readFileSync(outcomes, 'utf8').trim().split('\n').pop().split('\t')[3]
  }
  assert.equal(
    causeOf(description().replace('## Required proof', '## Not a contract section')),
    'contract',
  )
  assert.equal(causeOf(description().replace(`![shot](${UPLOAD})`, '')), 'uploads')
  assert.equal(causeOf(description().replace('first observation', 'FIRST observation')), 'notes')
})

test('an unreadable stored description records one fire line and stays non-zero', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-writeback-verify-record-'))
  const outcomes = path.join(dir, 'outcomes.tsv')
  const intended = path.join(dir, 'intended.md')
  writeFileSync(intended, description())
  const res = spawnSync(
    process.execPath,
    [HELPER, '--intended', intended, '--stored', path.join(dir, 'absent.md')],
    {
      encoding: 'utf8',
      cwd: fileURLToPath(new URL('..', import.meta.url)),
      env: { ...process.env, BOSS_GATE_OUTCOME_FILE: outcomes },
    },
  )
  assert.notEqual(res.status, 0)
  assert.match(res.stderr, /cannot read stored description/)
  const recorded = readFileSync(outcomes, 'utf8').split('\n').filter(Boolean)
  assert.equal(recorded.length, 1, 'the latch must permit exactly one line')
  assert.deepEqual(recorded[0].split('\t').slice(1), [
    'plan-writeback-verify',
    'fire',
    'unreadable-stored',
  ])
})

// ---------------------------------------------------------------------------
// BOS-1214 — the round-trip invariant, from both directions.
//
// The boss-plan body no longer derives byte mechanics; it states what must survive a tracker
// round-trip and defers the decision to this helper. That deferral is only safe if the helper
// keeps deciding the same way, so these tests pin the invariant in BOTH directions: a round trip
// that reshaped nothing but the declared markdown still verifies, and a round trip that lost
// content still fails. A framing that reads as softer must not BE softer.
// ---------------------------------------------------------------------------

test('BOS-1214: a renormalized round trip verifies — declared reshaping is not content loss', () => {
  // The case the byte framing made look impossible. The tracker rewrites every `-` bullet to `*`
  // and trims the terminal newline; not one word of the description changed.
  const intended = description()
  const stored = `${intended.replace(/^- /gm, '* ')}\n\n`.replace(/\n+$/, '')
  assert.notEqual(stored, intended, 'the fixture must actually differ from the intended bytes')
  const result = verify(intended, stored, [
    'unordered-list-marker-substitution',
    'terminal-newline-trimming',
  ])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(result.exitCode, 0)
})

// AC2 ADJUDICATION — read this before reconciling the two tests above and below with the
// acceptance criterion. AC2 asks for "a test that rewrites bullet markers AND escapes entities"
// and verifies. That criterion is not satisfiable as written: entity escaping is deliberately
// absent from the closed tolerance vocabulary, so a round trip that escapes entities is `drift`
// by construction. Making it verify would mean declaring an `entity-escaping` id in
// skill-config.mjs's DESCRIPTION_NORMALIZATION_TRANSFORMS — a file outside this plan's Key
// changes — AND writing its normalizer in plan-writeback-verify.mjs's
// DESCRIPTION_TRANSFORM_NORMALIZERS, which is in Key changes; the load-time cross-check throws if
// either side lands without the other. That would widen what the gate ignores for a transform
// this transport has never been observed to perform — the exact loosening the plan's Risks
// section names as the thing to avoid. So AC2 is split: its
// headline claim ("a renormalized round trip verifies") is proved by the test above, and its
// "escapes entities" clause is proved in the only direction that is true — as drift, below.
// The criterion is mis-scoped, not unimplemented; it needs amending, not a wider tolerance set.
test('BOS-1214: an UNDECLARED reshaping — entity escaping — is drift, not a tolerated round trip', () => {
  // The other direction, and the reason the test above is not a licence. Entity escaping is a
  // markdown renormalization a transport could plausibly perform, and it is deliberately ABSENT
  // from the closed tolerance vocabulary, so the helper must report it as drift rather than wave
  // it through as "the tracker renormalizes". The tolerance set is what decides — never the
  // intuition that a difference looks cosmetic.
  const intended = description({
    sections: { '## Approach': 'Compare stored & intended, then <report> the verdict.' },
  })
  const stored = intended
    .replace(/^- /gm, '* ')
    .replace('stored & intended', 'stored &amp; intended')
    .replace('<report>', '&lt;report&gt;')
  assert.ok(stored.includes('&amp;'), 'the fixture must actually escape an entity')
  assert.ok(
    !ALL_TRANSFORMS.has('entity-escaping'),
    'this test is only meaningful while entity escaping is undeclared',
  )
  const result = verify(intended, stored)
  assertNotCertifiedEquivalent(result)
})

test('BOS-1214: a dropped WORD still fails, however tolerant the declared transform set is', () => {
  // The loosened framing must not loosen the check. Every transform in the vocabulary is declared
  // here — the widest tolerance this helper can ever be configured with — and a single deleted
  // word in `## Original notes` must still be drift.
  const intended = description()
  const stored = intended.replace('- first observation', '- first')
  assert.notEqual(stored, intended, 'the fixture must actually drop a word')
  const result = verify(intended, stored, ALL_TRANSFORMS)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.notEqual(result.exitCode, 0)
  assert.match(result.reason, /`## Original notes`/)
})

test('BOS-1214: a dropped IMAGE URL still fails, and the reason names the lost identity', () => {
  // The most expensive loss the gate exists to catch: the tracker keeps no description history,
  // so a dropped upload URL is permanent. Widest tolerance again, and the stored text is a valid
  // description in every other respect — only the image is gone.
  const intended = description()
  const stored = intended.replace(`![shot](${UPLOAD})`, '[screenshot: a shot of the failure]')
  assert.ok(!stored.includes(UPLOAD), 'the fixture must actually drop the upload URL')
  const result = verify(intended, stored, ALL_TRANSFORMS)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.notEqual(result.exitCode, 0)
  assert.match(result.reason, /upload identit/)
  assert.ok(result.reason.includes(UPLOAD), 'the reason must name the URL that was lost')
})

test('BOS-1214 CLI: a failing run repeats the machine verdict on stderr', () => {
  // The body defers to the verdict, so a caller must be able to READ the verdict on the stream it
  // captured. Every failure branch in the skill bodies reads stderr; before this the verdict word
  // existed on stdout alone and a stderr-only caller had to infer `drift` from the exit status.
  const intended = description()
  const stored = intended.replace('first observation', 'FIRST observation')
  const { res } = runCli(intended, stored)
  assert.notEqual(res.status, 0)
  assert.match(res.stdout, /^writeback-verdict: drift$/m)
  assert.match(res.stderr, /^writeback-verdict: drift$/m)
})

test('BOS-1214 CLI: a PASSING run leaves stderr clean — the echo is failure-only', () => {
  // The stderr echo must not become a second unconditional channel: a passing run that wrote to
  // stderr would train callers to read failure into a clean round trip.
  const intended = description()
  const stored = intended.replace(/^- /gm, '* ')
  const { res } = runCli(intended, stored)
  assert.equal(res.status, 0, res.stderr)
  assert.match(res.stdout, /^writeback-verdict: normalized-equivalent$/m)
  assert.equal(res.stderr.trim(), '', 'a passing run says nothing on stderr')
})

// ---------------------------------------------------------------------------
// BOS-1223 U1 — the table-delimiter-row canonicalizer must recognise a delimiter
// ROW, not a line of dashes.
//
// Per the GFM tables extension a delimiter cell is hyphens with an optional leading or trailing
// colon: the colons carry alignment and the dash count carries nothing. A transport that rewrites
// `| --- |` as `| -- |` therefore changes no meaning, and canonicalizing the run length is safe.
// Safe for a delimiter ROW and for the dashes only, though — which is what the near-misses below
// separate from "rewrite every dash run in sight". A LOST alignment colon and a LOST cell are real
// semantic changes and must still read as drift; a horizontal rule and a fenced line are not
// delimiter rows at all.
// ---------------------------------------------------------------------------

const TABLE_TRANSFORM = 'table-delimiter-row-normalization'
const tableOnly = new Set([TABLE_TRANSFORM])
const canonRow = (text) => normalizeDescription(text, tableOnly)

const tableDoc = (delimiterRow) => `| id | role |\n${delimiterRow}\n| 1 | two |\n`

test('table-delimiter-row: rows differing only in dash-run length canonicalize equal', () => {
  // The measured pair: a raw upload carries three dashes, the stored copy two.
  const intended = tableDoc('| --- | --- |')
  const stored = tableDoc('| -- | -- |')
  assert.notEqual(intended, stored, 'the fixture must actually differ')
  assert.equal(canonRow(intended), canonRow(stored))
})

test('table-delimiter-row: one dash and many dashes canonicalize equal — the count carries nothing', () => {
  assert.equal(canonRow(tableDoc('| - | - |')), canonRow(tableDoc('| ---------- | - |')))
})

test('table-delimiter-row: a row with no leading or trailing pipe is still a delimiter row', () => {
  assert.equal(canonRow(tableDoc('--- | ---')), canonRow(tableDoc('-- | --')))
})

test('table-delimiter-row: alignment colons are PRESERVED while the dash run is canonicalized', () => {
  // Colons survive verbatim, so a colon-bearing row differing only in dash count still agrees.
  assert.equal(canonRow(tableDoc('| :--: | ---: |')), canonRow(tableDoc('| :-: | -: |')))
  assert.match(canonRow(tableDoc('| :--: | ---: |')), /\| :-+: \| -+: \|/)
})

test('table-delimiter-row near-miss: a DROPPED alignment colon is NOT canonicalized away', () => {
  // Dash count carries nothing; a colon carries alignment. Losing one is semantic loss and must
  // survive canonicalization as a difference, in both the leading and the trailing position.
  for (const [withColon, without] of [
    ['| :--- | --- |', '| --- | --- |'],
    ['| ---: | --- |', '| --- | --- |'],
    ['| :---: | --- |', '| :--- | --- |'],
  ]) {
    assert.notEqual(
      canonRow(tableDoc(withColon)),
      canonRow(tableDoc(without)),
      `losing the alignment colon in \`${withColon}\` must remain a difference`,
    )
  }
})

test('table-delimiter-row near-miss: a DROPPED cell is NOT canonicalized away', () => {
  assert.notEqual(canonRow(tableDoc('| --- | --- | --- |')), canonRow(tableDoc('| --- | --- |')))
})

test('table-delimiter-row near-miss: a horizontal rule and a prose dash line are returned unchanged', () => {
  // No pipe, so no delimiter row — a single-column delimiter row without pipes is
  // indistinguishable from a thematic break, and the safe reading is "not a table".
  for (const text of [
    '---\n',
    '-----\n',
    'Heading\n---\n',
    'a clause -- and another -- in prose\n',
    '- a list item\n',
    'run `a | b` -- fast\n',
  ]) {
    assert.equal(canonRow(text), text, `\`${text.trim()}\` is not a delimiter row`)
  }
})

test('table-delimiter-row: a delimiter row inside a fenced code block is not rewritten', () => {
  // Fenced content is literal text; rewriting it would change what the block SHOWS. The fence must
  // also close, so a real row after it is still canonicalized.
  const fenced = [
    '```',
    '| a | b |',
    '| -- | -- |',
    '```',
    '',
    '| a | b |',
    '| -- | -- |',
    '',
  ].join('\n')
  const out = canonRow(fenced)
  assert.ok(out.includes('```\n| a | b |\n| -- | -- |\n```'), 'the fenced row survives verbatim')
  assert.ok(out.endsWith('| a | b |\n| --- | --- |\n'), 'the row after the fence is canonicalized')
})

test('table-delimiter-row is idempotent', () => {
  const once = canonRow(tableDoc('| -- | :- |'))
  assert.equal(canonRow(once), once)
})

test('tier 2: a declared table-delimiter-row reshaping reaches normalized-equivalent', () => {
  const intended = emphasis(`A table:\n\n${tableDoc('| --- | --- |')}`)
  const stored = emphasis(`A table:\n\n${tableDoc('| -- | -- |')}`)
  assert.notEqual(intended, stored, 'the fixture must actually differ')
  const result = verify(intended, stored, [TABLE_TRANSFORM])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(result.exitCode, 0)
})

test('tier 3: the same table-delimiter difference is drift when the id is NOT declared', () => {
  // Proves the new id is load-bearing rather than decorative: the same bytes, one declaration apart.
  const intended = emphasis(`A table:\n\n${tableDoc('| --- | --- |')}`)
  const stored = emphasis(`A table:\n\n${tableDoc('| -- | -- |')}`)
  const result = verify(intended, stored, ['terminal-newline-trimming'])
  assertNotCertifiedEquivalent(result)
})

// ---------------------------------------------------------------------------
// BOS-1223 U2 — the emphasis canonicaliser must recognise the SPACED split form,
// at any inline-code-span count.
//
// The transform was anchored on the CONTIGUOUS shape, `D A D` + code + `D B D` with nothing at the
// split point. The shape the transport actually emits puts whitespace there and repeats it once per
// code span, so neither measured pair was recognised and both reported drift. Re-pointing a narrow
// recogniser at the measured shape is not the same as relaxing it: the merge stays merge-only and
// presence-preserving, and the two ways the signal degrades — emphasis DELETED and emphasis
// DEMOTED — are pinned here in the spaced context as well as the contiguous one. They are the tests
// that make the tolerance safe rather than merely permissive; the prior incident in this exact
// function was a widening that passed without them.
// ---------------------------------------------------------------------------

const emphasisOnly = new Set(['emphasis-span-restructuring'])
const canonEm = (text) => normalizeDescription(text, emphasisOnly)
const agree = (intended, stored, why) => {
  assert.notEqual(intended, stored, 'the fixture must actually differ')
  assert.equal(canonEm(intended), canonEm(stored), why)
}

test('emphasis-span-restructuring: the measured ONE-code-span spaced pair agrees', () => {
  agree(
    'depends on the **exit code of `launchctl list` alone**, never on the plist.',
    'depends on the **exit code of** `launchctl list` **alone**, never on the plist.',
    'the spaced split is the shape the transport emits',
  )
})

test('emphasis-span-restructuring: the measured TWO-code-span spaced pair agrees', () => {
  agree(
    '**Cancel a CHILD context scoped to `p.srv.Shutdown`, never the one `Shutdown` was handed.**',
    '**Cancel a CHILD context scoped to** `p.srv.Shutdown`**, never the one** `Shutdown` **was handed.**',
    'the rule repeats across code spans, and tolerates a space on one side only',
  )
})

test('emphasis-span-restructuring: a THREE-code-span span agrees — the rule is not bounded to two', () => {
  agree(
    '**one `a` two `b` three `c` four**',
    '**one** `a` **two** `b` **three** `c` **four**',
    'nothing in the rule may cap the number of code spans',
  )
})

test('emphasis-span-restructuring: the CONTIGUOUS split form still agrees after the repair', () => {
  // The regression check on the repair itself: re-pointing at the spaced shape must not trade away
  // the shape the pattern already recognised.
  agree(
    'The **helper `plan-writeback-verify.mjs` runs once** per run.',
    'The **helper **`plan-writeback-verify.mjs`** runs once** per run.',
    'the no-whitespace split is still recognised',
  )
})

test('emphasis-span-restructuring: underscore delimiters behave as asterisk delimiters do', () => {
  agree('_the `foo` helper_', '_the_ `foo` _helper_', 'the delimiter character is not privileged')
  agree('__the `foo` helper__', '__the__ `foo` __helper__', 'nor is the run length')
})

test('emphasis-span-restructuring near-miss: DELETED emphasis in the SPACED form is NOT merged away', () => {
  // The failure the prior incident actually shipped, re-pinned against the newly recognised shape:
  // a stored copy that lost the emphasis entirely must not reduce to the intended string.
  for (const [intended, stored] of [
    ['**exit code of** `launchctl list` **alone**', 'exit code of `launchctl list` alone'],
    ['**exit code of `launchctl list` alone**', 'exit code of `launchctl list` alone'],
    ['**one** `a` **two** `b` **three**', 'one `a` two `b` three'],
  ]) {
    assert.notEqual(
      canonEm(intended),
      canonEm(stored),
      `losing the emphasis in \`${intended}\` must survive canonicalization as a difference`,
    )
  }
})

test('emphasis-span-restructuring near-miss: DEMOTED emphasis in the SPACED form is NOT merged away', () => {
  // The lookaround fences, re-pinned on the widened pattern: without them the engine backtracks a
  // `**` run down to `*` and certifies bold as equal to italic.
  assert.notEqual(
    canonEm('**exit code of** `launchctl list` **alone**'),
    canonEm('*exit code of* `launchctl list` *alone*'),
  )
  assert.notEqual(
    canonEm('**exit code of `launchctl list` alone**'),
    canonEm('*exit code of `launchctl list` alone*'),
  )
})

test('emphasis-span-restructuring: a MISMATCHED delimiter pair is not merged — the backreference holds', () => {
  const mixed = '**a** `c` *b*'
  assert.equal(canonEm(mixed), mixed, 'the outer pair must be the SAME delimiter run')
})

test('emphasis-span-restructuring: a split spanning a line break is NOT merged', () => {
  const across = '**a**\n`c`\n**b**'
  assert.equal(canonEm(across), across, 'the rule stays bounded to one line')
})

test('emphasis-span-restructuring: an authored two-bold-span shape agrees on both sides (R5a)', () => {
  // Real stored text carries numbered-item shapes structurally identical to a split. The merge
  // fuses both and no pattern can separate them — but it runs on the intended and the stored side
  // alike, so it cannot manufacture a difference. Position is lost; presence and run length are not.
  const authored = '**1.** `boss daemon status` **reports what is serving.**'
  assert.equal(canonEm(authored), canonEm(authored))
  const result = verify(emphasis(authored), emphasis(authored), ['emphasis-span-restructuring'])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.BYTE_EXACT)
})

test('emphasis-span-restructuring: the bounded loop CONVERGES rather than exhausting its passes', () => {
  // Six code spans in one span: if the merge needed one pass per span it would run out of passes
  // and return a half-merged string, which is neither idempotent nor order-independent.
  const stored = '**a** `1` **b** `2` **c** `3` **d** `4` **e** `5` **f** `6` **g**'
  const intended = '**a `1` b `2` c `3` d `4` e `5` f `6` g**'
  assert.equal(canonEm(stored), canonEm(intended))
  assert.equal(canonEm(canonEm(stored)), canonEm(stored), 'and the result is a fixpoint')
})

test('tier 2: a declared SPACED emphasis restructuring reaches normalized-equivalent', () => {
  const intended = emphasis('It depends on the **exit code of `launchctl list` alone**.')
  const stored = emphasis('It depends on the **exit code of** `launchctl list` **alone**.')
  assert.notEqual(intended, stored, 'the fixture must actually differ')
  const result = verify(intended, stored, ['emphasis-span-restructuring'])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(result.exitCode, 0)
})

// ---------------------------------------------------------------------------
// BOS-1223 review round — the two false-pass paths the widened recognisers opened.
//
// Both are the same defect class as the incident this file was built around: a canonicalizer that
// DELETES bytes rather than recognising a reshaping makes two genuinely different documents compare
// equal, and the gate then certifies real content loss as `normalized-equivalent` and exits zero.
// The emphasis segments could run ACROSS independently emphasised spans, so the merge deleted
// emphasis that was never a split joint; the table rule matched a delimiter row by SHAPE alone, so
// it canonicalized ordinary body-row content. These are the near-misses that make the two
// tolerances safe rather than merely permissive.
// ---------------------------------------------------------------------------

test('emphasis-span-restructuring near-miss: emphasis on a LATER code span is NOT merged away', () => {
  // Measured: the segments did not exclude the delimiter, so one match ran from the first `**`
  // across the split span and into `**`z`**`, and the merge deleted that span's bold. The stored
  // copy which genuinely LOST that bold then reduced to the same string — a false pass.
  const intended = '**a** `c` **b** then **`z`** and **`w`** done'
  const storedLost = '**a** `c` **b** then `z` and **`w`** done'
  assert.notEqual(
    canonEm(intended),
    canonEm(storedLost),
    'losing the bold around a later code span must survive canonicalization as a difference',
  )
})

test('emphasis-span-restructuring: a split span leaves a LATER emphasised code span intact', () => {
  // The positive half of the same bound: the merge fuses the split and stops there.
  assert.equal(
    canonEm('**a** `c` **b** then **`z`** and **`w`** done'),
    '**a `c` b** then **`z`** and **`w`** done',
  )
})

test('emphasis-span-restructuring: one match covers ONE emphasis span, not the line', () => {
  // The bound the comment claims. Two independently authored split spans on one line merge into
  // two spans — not into one span swallowing the text between them.
  assert.equal(canonEm('**a** `c` **b** and **d** `e` **f**'), '**a `c` b** and **d `e` f**')
})

test('emphasis-span-restructuring: a closing run is not read as the next span opening run', () => {
  // The flanking requirement, pinned directly: an already-merged span must be a fixpoint even when
  // an emphasised code span follows it on the same line.
  const merged = '**a `c` b** then **`z`** and **`w`** done'
  assert.equal(canonEm(merged), merged)
})

test('table-delimiter-row: a fence is closed only by its OWN character', () => {
  // Measured: the tracker toggled a bare boolean on any fence line, so `~~~` closed a ``` block and
  // the row after it was rewritten as though it were document text.
  const doc = '```\n~~~\n| -- |\n```\n'
  assert.equal(canonRow(doc), doc, 'a tilde line does not close a backtick fence')
})

test('table-delimiter-row: a shorter inner fence does not close a longer outer one', () => {
  // The four-backtick wrapper this repo's docs use to quote a markdown block containing a fence.
  const doc = '````\n| a | b |\n| -- | -- |\n```\n| a | b |\n| -- | -- |\n```\n| -- |\n````\n'
  assert.equal(canonRow(doc), doc, 'everything inside the ```` wrapper is literal')
})

test('table-delimiter-row: the fence still closes on its own character at its own length', () => {
  // The complement — the stricter tracking must not leave a block open forever.
  const doc = '````\n| a | b |\n| -- | -- |\n````\n\n| a | b |\n| -- | -- |\n'
  assert.ok(canonRow(doc).includes('````\n| a | b |\n| -- | -- |\n````'), 'the fenced row survives')
  assert.ok(canonRow(doc).endsWith('| a | b |\n| --- | --- |\n'), 'the row after it is rewritten')
})

test('table-delimiter-row near-miss: a dash-only BODY row is NOT canonicalized', () => {
  // In GFM a delimiter row is POSITIONAL — the row after the header row. `| - | - |` as a body cell
  // meaning "none" is ordinary content, and rewriting it made two different tables compare equal.
  const intended = '| a | b |\n| --- | --- |\n| - | - |\n'
  const stored = '| a | b |\n| --- | --- |\n| --- | --- |\n'
  assert.equal(canonRow(intended), intended, 'the body row is left exactly as authored')
  assert.notEqual(canonRow(intended), canonRow(stored), 'and the two tables still differ')
})

test('tier 3: a body row rewritten to a delimiter row is drift, not tier 2', () => {
  // End-to-end, with the transform DECLARED: content loss must reach the non-zero exit.
  const intended = emphasis('A table:\n\n| a | b |\n| --- | --- |\n| - | - |\n')
  const stored = emphasis('A table:\n\n| a | b |\n| --- | --- |\n| --- | --- |\n')
  const result = verify(intended, stored, [TABLE_TRANSFORM])
  assertNotCertifiedEquivalent(result)
})

test('table-delimiter-row: a delimiter-shaped line with NO header row before it is left alone', () => {
  for (const text of ['| --- | --- |\n', '\n| -- | -- |\n', '| --- | --- |\n| a | b |\n']) {
    assert.equal(canonRow(text), text, `\`${text.trim()}\` heads no table`)
  }
})

test('table-delimiter-row: the row right after a header row IS still normalized', () => {
  // The positive half — making the rule positional must not disable it.
  assert.equal(canonRow('| a | b |\n| -- | -- |\n'), '| a | b |\n| --- | --- |\n')
})

test('both repaired normalizers stay idempotent', () => {
  for (const [canon, text] of [
    [canonEm, '**a** `c` **b** then **`z`** and **`w`** done'],
    [canonEm, '**one** `a` **two** `b` **three**'],
    [canonRow, '````\n| a | b |\n| -- | -- |\n```\n````\n\n| a | b |\n| -- | -- |\n| - | - |\n'],
  ]) {
    const once = canon(text)
    assert.equal(canon(once), once, `\`${text}\` must reach a fixpoint in one application`)
  }
})

// ---------------------------------------------------------------------------
// BOS-1286 U1 — blank-line normalization at a list block boundary.
//
// The transform the planning skill's own append provokes, and the one the vocabulary had no name
// for: the tracker pushes a blank line between a list and the heading that followed it flush, and
// it removes the blank line between an appended bullet and the list above it. Two spellings of one
// reshaping, so ONE canonicalizer handles both and neither side has to know which reshaped.
//
// Unlike every other declared transform this one is NOT purely cosmetic — a blank line inside a
// list makes the list loose, which changes the rendered markup — so the near-misses below are not
// decoration. They are the only thing bounding the widening, and they say what the canonicalizer
// must never buy: a dropped list item, a paragraph absorbed between two lists, or a heading gone.
// ---------------------------------------------------------------------------

const BLANK_TRANSFORM = 'block-boundary-blank-line-normalization'
const blankOnly = new Set([BLANK_TRANSFORM])
const canonBlank = (text) => normalizeDescription(text, blankOnly)

test('block-boundary-blank-line: a list flush against the following heading agrees with the spaced form', () => {
  // The measured insertion direction: the run wrote the list flush, the tracker spaced it.
  assert.equal(canonBlank('* one\n* two\n## Next'), canonBlank('* one\n* two\n\n## Next'))
})

test('block-boundary-blank-line: a blank line between two list items agrees with the tight form', () => {
  // The measured removal direction: the appender left a blank line, the tracker reattached.
  assert.equal(canonBlank('* a\n\n* b'), canonBlank('* a\n* b'))
  assert.equal(canonBlank('1. a\n\n2. b'), canonBlank('1. a\n2. b'), 'ordered lists reshape too')
  assert.equal(canonBlank('* a\n\n\n* b'), canonBlank('* a\n* b'), 'a multi-line run collapses too')
})

test('block-boundary-blank-line near-miss: a PARAGRAPH between list items is never absorbed', () => {
  // The blank lines flanking a paragraph do not qualify at the paragraph end, so nothing is
  // dropped and the paragraph's own bytes survive: two lists split by prose cannot become one.
  const split = '* a\n\nA paragraph between them.\n\n* b'
  assert.equal(canonBlank(split), split, 'neither flanking blank line qualifies')
  assert.notEqual(
    canonBlank(split),
    canonBlank('* a\n* b'),
    'merging the two lists is NOT tolerated',
  )
})

test('block-boundary-blank-line near-miss: a DROPPED list item is NOT canonicalized away', () => {
  assert.notEqual(canonBlank('* a\n* b\n* c'), canonBlank('* a\n* c'))
  assert.notEqual(canonBlank('* a\n\n* b'), canonBlank('* a'))
})

test('block-boundary-blank-line near-miss: a DROPPED heading is NOT canonicalized away', () => {
  // The rule may delete the blank line before a heading; it may never delete the heading.
  assert.notEqual(canonBlank('* a\n\n## Next\n\ntext'), canonBlank('* a\n\ntext'))
})

test('block-boundary-blank-line: blank lines outside the recognised boundary are left alone', () => {
  for (const text of [
    'A paragraph.\n\n## Heading\n',
    '## Heading\n\n* a\n',
    '* a\n\nA trailing paragraph.\n',
    '* a\n\n| h |\n| - |\n',
    '* a long item\n  lazily continued\n\n## Heading\n',
    '* a\n\n',
    '\n\n* a\n',
  ]) {
    assert.equal(canonBlank(text), text, `\`${JSON.stringify(text)}\` is not a recognised boundary`)
  }
})

test('block-boundary-blank-line: a list boundary inside a fenced code block is not reshaped', () => {
  // Fenced content is literal; reshaping it would change what the block SHOWS. The fence must also
  // close, so a real boundary after it is still canonicalized.
  const fenced = ['```', '* a', '', '* b', '```', '', '* c', '', '## Next', ''].join('\n')
  const out = canonBlank(fenced)
  assert.ok(out.includes('```\n* a\n\n* b\n```'), 'the fenced boundary survives verbatim')
  assert.ok(out.endsWith('* c\n## Next\n'), 'the boundary after the fence is canonicalized')
})

test('block-boundary-blank-line near-miss: an INDENTED code block is literal and is not reshaped', () => {
  // Four spaces opens an indented code block, whose content is literal for the same reason a
  // fence's is. `    * a` there is DISPLAYED text, not a list item, so a blank line between two
  // such lines is part of what the block shows. Dropping it would certify two genuinely different
  // documents as equivalent — a false PASS, not the conservative false drift this rule settles for
  // everywhere else. Both marker families are pinned: the bound is on the indentation, not the
  // marker.
  const indented = 'text\n\n    * a\n\n    * b\n'
  assert.equal(canonBlank(indented), indented, 'the indented block is returned verbatim')
  assert.notEqual(canonBlank(indented), canonBlank('text\n\n    * a\n    * b\n'))
  assert.notEqual(
    canonBlank('text\n\n    1. a\n\n    2. b\n'),
    canonBlank('text\n\n    1. a\n    2. b\n'),
    'ordered markers are bounded by indentation too',
  )
  // The bound costs nothing a real boundary needs: three spaces is still a list item.
  assert.equal(canonBlank('   * a\n\n   * b\n'), canonBlank('   * a\n   * b\n'))
})

test('block-boundary-blank-line is idempotent', () => {
  for (const text of [
    '* a\n\n* b\n\n## Next\n',
    '* a\n\nprose\n\n* b\n',
    '```\n* a\n\n* b\n```\n\n* c\n\n## Next\n',
  ]) {
    const once = canonBlank(text)
    assert.equal(canonBlank(once), once, `\`${JSON.stringify(text)}\` must reach a fixpoint`)
  }
})

test('tier 2: a blank line INSERTED before the following heading reaches normalized-equivalent', () => {
  // AC2. The intended bytes put the list flush against the next heading; the stored copy has the
  // blank line the tracker pushed in. Only the new id is declared, so it alone carries the verdict.
  const spaced = description({ sections: { '## Summary': '- one\n- two' } })
  const intended = spaced.replace('- two\n\n## Approach', '- two\n## Approach')
  assert.notEqual(intended, spaced, 'the fixture must actually differ')
  const result = verify(intended, spaced, [BLANK_TRANSFORM])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(result.exitCode, 0)
})

test('tier 2: a blank line REMOVED above an appended bullet reaches normalized-equivalent', () => {
  // AC3, the complementary direction: the step-5(f) append left a blank line above its bullet and
  // the tracker reattached it to the list.
  const planning = (body) => description({ sections: { '## Planning': body } })
  const intended = planning('- Contract: v1\n\n- Dependencies: blocks DEMO-2')
  const stored = planning('- Contract: v1\n- Dependencies: blocks DEMO-2')
  assert.notEqual(intended, stored, 'the fixture must actually differ')
  const result = verify(intended, stored, [BLANK_TRANSFORM])
  assert.equal(result.verdict, WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT)
  assert.equal(result.exitCode, 0)
})

test('tier 3: the same blank-line reshaping is NOT certified when the id is not declared', () => {
  // Proves the new id is load-bearing rather than decorative: the same bytes, one declaration apart.
  const spaced = description({ sections: { '## Summary': '- one\n- two' } })
  const intended = spaced.replace('- two\n\n## Approach', '- two\n## Approach')
  assertNotCertifiedEquivalent(verify(intended, spaced, ['terminal-newline-trimming']))
})

test('tier 3: a dropped list item stays non-equivalent with EVERY transform declared', () => {
  // AC4 at the verdict level rather than the canonicalizer level — the bound has to hold through
  // the whole conjunction, not just in the normalizer's own unit test.
  const intended = description({
    sections: { '## Testing': '- unit coverage\n- integration coverage' },
  })
  const stored = description({ sections: { '## Testing': '- unit coverage' } })
  assertNotCertifiedEquivalent(verify(intended, stored))
})

test('tier 3: two lists merged across the paragraph that split them stay non-equivalent', () => {
  const intended = description({
    sections: { '## Testing': '- unit coverage\n\nThen, separately:\n\n- integration coverage' },
  })
  const stored = description({
    sections: { '## Testing': '- unit coverage\n- integration coverage' },
  })
  assertNotCertifiedEquivalent(verify(intended, stored))
})

// ---------------------------------------------------------------------------
// BOS-1286 U2 — the `unattributed` verdict locates its difference in the texts
// it actually compared.
//
// The coordinate used to be computed once, from the RAW texts, and reused by every branch. That is
// right for the three content-loss branches — the loss IS at the first raw difference — and wrong
// for this one, which only fires after normalization has already excused every declared transform.
// Measured on one run: 26 differing lines, 24 of them declared marker substitution, and the verdict
// named line 7, one of the 24. The reason then told the reader to open that location, where nothing
// was wrong. The fixture below reproduces that shape in miniature: the first RAW difference is a
// declared substitution several lines ABOVE the first unattributable one.
// ---------------------------------------------------------------------------

/** The 1-based line and column of the first differing byte — the helper's own rule, restated. */
function firstDifferenceAt(a, b) {
  let index = 0
  while (index < Math.max(a.length, b.length) && a.charAt(index) === b.charAt(index)) index += 1
  const before = a.slice(0, index)
  return { line: before.split('\n').length, column: index - (before.lastIndexOf('\n') + 1) + 1 }
}

/** Intended bytes whose first raw difference from `stored` is a DECLARED marker substitution. */
function substitutionAboveUnattributable() {
  const intended = description({ sections: { '## Summary': '- one\n- two' } })
  const stored = intended
    .replace('- one\n- two', '* one\n* two')
    .replace('Read the stored description back and compare.', 'Read the stored description.')
  return { intended, stored }
}

test('the unattributed verdict reports the first NORMALIZED difference, not the first raw one', () => {
  const { intended, stored } = substitutionAboveUnattributable()
  const result = verify(intended, stored)
  assertNotCertifiedEquivalent(result)

  const raw = firstDifferenceAt(intended, stored)
  const normalized = firstDifferenceAt(
    normalizeDescription(intended, ALL_TRANSFORMS),
    normalizeDescription(stored, ALL_TRANSFORMS),
  )
  assert.ok(
    raw.line < normalized.line,
    `the fixture must put the declared substitution ABOVE the unattributable difference ` +
      `(raw line ${raw.line}, normalized line ${normalized.line})`,
  )
  assert.deepEqual(
    { line: result.line, column: result.column },
    normalized,
    'the reported coordinate must index the normalized comparison this branch made',
  )
  assert.notEqual(result.line, raw.line, 'the excused raw difference must not be what is reported')
})

test('the unattributed reason states which comparison its coordinate indexes', () => {
  // Without this the fix trades one misleading pointer for another: a declared transform may change
  // line counts, so a normalized coordinate need not index the stored document either.
  const { intended, stored } = substitutionAboveUnattributable()
  const result = verify(intended, stored)
  assert.ok(
    result.reason.includes(`line ${result.line}, column ${result.column}`),
    'the reason must carry the same coordinate the result field reports',
  )
  assert.match(
    result.reason,
    /NORMALIZED/,
    'the reason must name the comparison as the normalized one',
  )
  assert.match(
    result.reason,
    /NOT\s+a\s+position\s+in\s+the\s+stored\s+document/,
    'the reason must deny that the coordinate indexes the stored document',
  )
})

test('a drift cause still reports the RAW first-difference coordinate', () => {
  // The other half of the split. The same fixture shape, plus a lost contract section: a content
  // loss IS at the first raw difference, so that branch must be untouched by the change above.
  const intended = description({ sections: { '## Summary': '- one\n- two' } })
  const stored = intended
    .replace('- one\n- two', '* one\n* two')
    .replace(/## Required proof\n\n[^\n]*\n\n/, '')
  const result = verify(intended, stored)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.equal(result.cause, WRITEBACK_CAUSES.CONTRACT)
  assert.deepEqual(
    { line: result.line, column: result.column },
    firstDifferenceAt(intended, stored),
    'a content-loss verdict keeps the raw coordinate',
  )
  assert.ok(result.reason.includes(`first difference at line ${result.line}`))
})
