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
  normalizeDescription,
  parseWritebackVerifyArgs,
  verifyWriteback,
} from './plan-writeback-verify.mjs'

const HELPER = fileURLToPath(new URL('./plan-writeback-verify.mjs', import.meta.url))
const SKILL_CONFIG_SOURCE = fileURLToPath(new URL('./skill-config.mjs', import.meta.url))
const UPLOAD = 'https://uploads.linear.app/abc-123/screenshot.png'

// Derived, never restated. A hand-copied list here would be a fifth place the 5-id vocabulary is
// written down, and the one place a missing id looks like deliberate scope rather than drift.
const ALL_TRANSFORMS = new Set(DESCRIPTION_NORMALIZATION_TRANSFORMS)

const NOTES = `Reporter context.\n\n- first observation\n- second observation\n`

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
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
})

test('a drift verdict names the differing line and column', () => {
  const intended = description()
  const stored = intended.replace('Measure write-back', 'Measure writeback')
  const result = verify(intended, stored)
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
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

test('CLI: a tier-3 run exits non-zero and leaves both input files byte-unmodified', () => {
  const intended = description()
  const stored = intended.replace('Measure write-back', 'Measure writeback')
  const { res, intended: intendedPath, stored: storedPath } = runCli(intended, stored)
  assert.notEqual(res.status, 0)
  assert.match(res.stdout, /^writeback-verdict: drift$/m)
  assert.match(res.stderr, /line \d+, column \d+/)
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
  assert.equal(result.verdict, WRITEBACK_VERDICTS.DRIFT)
  assert.notEqual(result.exitCode, 0)
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
