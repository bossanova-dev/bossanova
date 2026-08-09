import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { CONVERGENCE_THRESHOLD, triageFindings } from './bs-review-triage.mjs'

const scriptPath = fileURLToPath(new URL('./bs-review-triage.mjs', import.meta.url))

/** A well-formed finding, per the repo's reviewer findings contract. */
function finding(overrides = {}) {
  return {
    severity: 'Suggestion',
    file: 'src/foo.js',
    line: 10,
    title: 'do the thing',
    detail: 'why it matters + suggested fix',
    lens: 'claude',
    ...overrides,
  }
}

/** Run the CLI as a subprocess; return trimmed stdout/stderr + exit status. */
function runCli(args = []) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8' })
  return { stdout: res.stdout.trim(), stderr: res.stderr.trim(), status: res.status }
}

// ---------------------------------------------------------------------------
// THE FALSIFICATION CASE — written and run first. An implementation that
// counts *occurrences* instead of *distinct reviewers* passes every other
// test in this file and fails only this one.
// ---------------------------------------------------------------------------

test('same lens reporting the same finding twice does NOT converge (distinct reviewers, not occurrences)', () => {
  const items = [finding(), finding()]
  const { mustFix, pool, invalid } = triageFindings(items)
  assert.deepEqual(invalid, [])
  assert.equal(mustFix.length, 0)
  assert.equal(pool.length, 1)
  assert.equal(pool[0].promotedBy, null)
  assert.equal(pool[0].reviewerCount, 1)
  assert.deepEqual(pool[0].lenses, ['claude'])
})

test('CLI categorize derives a bare Tier 2 lens reviewer identity from its dispatch file', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    writeFileSync(
      join(dir, 'findings-configured-reviewer.json'),
      JSON.stringify([finding({ lens: 'invented-first' }), finding({ lens: 'invented-second' })]),
    )

    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.mustFix.length, 0)
    assert.equal(parsed.pool.length, 1)
    assert.equal(parsed.pool[0].reviewerCount, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['configured-reviewer'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI categorize retains the source reviewer for malformed bare-array items', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const filename = 'findings-configured-reviewer.json'
    writeFileSync(join(dir, filename), JSON.stringify([null]))

    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.mustFix, [])
    assert.deepEqual(parsed.pool, [])
    assert.deepEqual(parsed.invalid, [
      {
        item: null,
        reason: 'item is not an object',
        source: { filename, reviewer: 'configured-reviewer' },
      },
    ])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// Convergence promotion across distinct reviewers.
// ---------------------------------------------------------------------------

test('two distinct lenses at the same key converge into one mustFix group', () => {
  const items = [finding({ lens: 'claude' }), finding({ lens: 'codex' })]
  const { mustFix, pool } = triageFindings(items)
  assert.equal(pool.length, 0)
  assert.equal(mustFix.length, 1)
  assert.equal(mustFix[0].promotedBy, 'convergence')
  assert.equal(mustFix[0].reviewerCount, 2)
  assert.deepEqual(mustFix[0].lenses, ['claude', 'codex'])
})

test('lenses are recorded in first-seen order', () => {
  const items = [
    finding({ lens: 'codex' }),
    finding({ lens: 'claude' }),
    finding({ lens: 'codex' }), // repeat: must not move or duplicate
  ]
  const { mustFix } = triageFindings(items)
  assert.deepEqual(mustFix[0].lenses, ['codex', 'claude'])
})

// ---------------------------------------------------------------------------
// Severity promotion.
// ---------------------------------------------------------------------------

test('a single Critical finding is promoted by severity', () => {
  const { mustFix } = triageFindings([finding({ severity: 'Critical', lens: 'claude' })])
  assert.equal(mustFix.length, 1)
  assert.equal(mustFix[0].promotedBy, 'severity')
  assert.equal(mustFix[0].reviewerCount, 1)
})

test('a single Warning finding is promoted by severity', () => {
  const { mustFix } = triageFindings([finding({ severity: 'Warning', lens: 'claude' })])
  assert.equal(mustFix.length, 1)
  assert.equal(mustFix[0].promotedBy, 'severity')
})

test('mixed severities at one key merge upward to the highest', () => {
  const items = [
    finding({ severity: 'Suggestion', lens: 'claude' }),
    finding({ severity: 'Critical', lens: 'codex' }),
  ]
  const { mustFix } = triageFindings(items)
  assert.equal(mustFix.length, 1)
  assert.equal(mustFix[0].severity, 'Critical')
  assert.equal(mustFix[0].promotedBy, 'severity')
})

test('Warning below Critical still merges to Critical', () => {
  const items = [
    finding({ severity: 'Warning', lens: 'claude' }),
    finding({ severity: 'Critical', lens: 'codex' }),
    finding({ severity: 'Suggestion', lens: 'gemini' }),
  ]
  const { mustFix } = triageFindings(items)
  assert.equal(mustFix[0].severity, 'Critical')
})

// ---------------------------------------------------------------------------
// Dedupe key: (file, line, title). null vs numbered lines never collapse.
// ---------------------------------------------------------------------------

test('line: null and line: 12 are distinct keys', () => {
  const items = [finding({ line: null, lens: 'claude' }), finding({ line: 12, lens: 'claude' })]
  const { pool, mustFix } = triageFindings(items)
  assert.equal(mustFix.length, 0)
  assert.equal(pool.length, 2)
})

test('different file or title never collapses into the same group', () => {
  const items = [
    finding({ file: 'a.js', lens: 'claude' }),
    finding({ file: 'b.js', lens: 'claude' }),
    finding({ title: 'other title', lens: 'claude' }),
  ]
  const { pool } = triageFindings(items)
  assert.equal(pool.length, 3)
})

// ---------------------------------------------------------------------------
// detail merge: first non-empty `detail` seen wins (documented choice).
// ---------------------------------------------------------------------------

test('detail merge takes the first non-empty detail seen', () => {
  const items = [
    finding({ lens: 'claude', detail: '' }),
    finding({ lens: 'codex', detail: 'the real explanation' }),
  ]
  const { mustFix } = triageFindings(items)
  assert.equal(mustFix[0].detail, 'the real explanation')
})

// A whitespace-only detail is truthy. Merging on emptiness rather than
// blankness would let it count as the chosen detail and block the real one
// behind it -- the group would then be rejected as unexplained while an
// occurrence that DID explain the defect sat unread.
test('detail merge steps over a whitespace-only detail to reach a real one', () => {
  const { mustFix, invalid } = triageFindings([
    finding({ lens: 'claude', detail: '   \n\t ' }),
    finding({ lens: 'codex', detail: 'the real explanation' }),
  ])
  assert.deepEqual(invalid, [])
  assert.equal(mustFix.length, 1)
  assert.equal(mustFix[0].detail, 'the real explanation')
})

// ---------------------------------------------------------------------------
// An all-blank merged detail is malformed: a finding is a location PLUS an
// explanation, and the Phase 6 fixer cannot adjudicate a premise from a
// location and an empty requested change. The verdict is per GROUP, after the
// merge, because a duplicate occurrence is allowed to supply the detail.
// ---------------------------------------------------------------------------

test('a group whose every occurrence has a blank detail is invalid, not promoted', () => {
  for (const blank of ['', '   ', '\n\t ']) {
    const { mustFix, pool, invalid } = triageFindings([
      finding({ severity: 'Critical', detail: blank }),
    ])
    assert.equal(mustFix.length, 0, `severity promotion escaped for ${JSON.stringify(blank)}`)
    assert.equal(pool.length, 0, `pool leak for ${JSON.stringify(blank)}`)
    assert.equal(invalid.length, 1)
    assert.equal(invalid[0].reason, 'blank detail on every occurrence of this finding')
    // The entry must still name the finding, or the ledger cannot route it back
    // to the reviewer that owes the explanation.
    assert.equal(invalid[0].item.file, 'src/foo.js')
    assert.equal(invalid[0].item.title, 'do the thing')
  }
})

// Convergence is the other promotion route, and it does not consult severity --
// so a Suggestion reported by two distinct reviewers would reach must-fix by a
// different branch. It must be rejected on the same grounds. The verdict is
// reached once per group but REPORTED once per occurrence, because each
// occurrence is a separate reviewer output that owes an explanation.
test('a converged group with blank details is invalid rather than must-fix', () => {
  const { mustFix, pool, invalid } = triageFindings([
    finding({ lens: 'claude', detail: '' }),
    finding({ lens: 'codex', detail: '  ' }),
  ])
  assert.equal(mustFix.length, 0)
  assert.equal(pool.length, 0)
  assert.equal(invalid.length, 2)
  assert.deepEqual(
    invalid.map((entry) => entry.item.lens),
    ['claude', 'codex'],
  )
})

// THE FALSIFICATION CASE for the rule above: it rejects UNEXPLAINED findings,
// not every finding. An implementation that rejected on any blank occurrence,
// or that simply stopped promoting, would pass the tests above and fail here.
test('a group with a real detail still promotes normally', () => {
  const { mustFix, pool, invalid } = triageFindings([
    finding({ severity: 'Critical', detail: 'why it matters' }),
    finding({ severity: 'Suggestion', detail: 'a lone suggestion', title: 'other thing' }),
  ])
  assert.deepEqual(invalid, [])
  assert.equal(mustFix.length, 1)
  assert.equal(mustFix[0].promotedBy, 'severity')
  assert.equal(pool.length, 1)
  assert.equal(pool[0].promotedBy, null)
})

// ---------------------------------------------------------------------------
// Malformed items — never silently dropped; recorded with a reason.
// ---------------------------------------------------------------------------

test('malformed items land in invalid with a reason, never in mustFix/pool', () => {
  const items = [
    finding({ file: '' }),
    finding({ file: undefined }),
    finding({ title: '' }),
    finding({ severity: 'Nope' }),
  ]
  const { mustFix, pool, invalid } = triageFindings(items)
  assert.equal(mustFix.length, 0)
  assert.equal(pool.length, 0)
  assert.equal(invalid.length, 4)
  for (const entry of invalid) {
    assert.ok('item' in entry)
    assert.equal(typeof entry.reason, 'string')
    assert.ok(entry.reason.length > 0)
  }
})

test('missing or non-string detail is invalid rather than producing an incomplete finding', () => {
  const { mustFix, pool, invalid } = triageFindings([
    finding({ detail: undefined }),
    finding({ detail: 42 }),
  ])
  assert.equal(mustFix.length, 0)
  assert.equal(pool.length, 0)
  assert.equal(invalid.length, 2)
  assert.deepEqual(
    invalid.map((entry) => entry.reason),
    ['missing or non-string detail', 'missing or non-string detail'],
  )
})

test('non-array items argument returns empty groups plus one invalid entry, and does not throw', () => {
  const result = triageFindings('not an array')
  assert.deepEqual(result.mustFix, [])
  assert.deepEqual(result.pool, [])
  assert.equal(result.invalid.length, 1)
  assert.equal(result.invalid[0].item, 'not an array')
  assert.equal(typeof result.invalid[0].reason, 'string')
  assert.ok(result.invalid[0].reason.length > 0)
})

test('a non-object item is invalid rather than throwing', () => {
  const result = triageFindings([null, 42, ['nope']])
  assert.equal(result.mustFix.length, 0)
  assert.equal(result.pool.length, 0)
  assert.equal(result.invalid.length, 3)
})

// ---------------------------------------------------------------------------
// convergenceThreshold: overridable per call; exported default is 2.
// ---------------------------------------------------------------------------

test('the exported default convergence threshold is 2', () => {
  assert.equal(CONVERGENCE_THRESHOLD, 2)
})

test('convergenceThreshold: 3 override requires a third distinct reviewer', () => {
  const two = [finding({ lens: 'claude' }), finding({ lens: 'codex' })]
  const resTwo = triageFindings(two, { convergenceThreshold: 3 })
  assert.equal(resTwo.mustFix.length, 0)
  assert.equal(resTwo.pool.length, 1)
  assert.equal(resTwo.pool[0].reviewerCount, 2)
  assert.equal(resTwo.pool[0].promotedBy, null)

  const three = [
    finding({ lens: 'claude' }),
    finding({ lens: 'codex' }),
    finding({ lens: 'gemini' }),
  ]
  const resThree = triageFindings(three, { convergenceThreshold: 3 })
  assert.equal(resThree.mustFix.length, 1)
  assert.equal(resThree.mustFix[0].promotedBy, 'convergence')
  assert.equal(resThree.mustFix[0].reviewerCount, 3)
})

// ---------------------------------------------------------------------------
// CLI — `categorize <dir>`.
// ---------------------------------------------------------------------------

test('CLI categorize reads every findings-*.json in a round-namespaced dir and prints parseable JSON', () => {
  const base = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const roundDir = join(base, 'round2')
    mkdirSync(roundDir)
    writeFileSync(
      join(roundDir, 'findings-claude.json'),
      JSON.stringify([finding({ lens: 'claude' })]),
    )
    writeFileSync(
      join(roundDir, 'findings-codex.json'),
      JSON.stringify([finding({ lens: 'codex' })]),
    )
    // A non-matching file in the same dir must be ignored.
    writeFileSync(join(roundDir, 'other.json'), JSON.stringify([finding({ lens: 'gemini' })]))

    const { stdout, stderr, status } = runCli(['categorize', roundDir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.mustFix.length, 1)
    assert.equal(parsed.mustFix[0].reviewerCount, 2)
    assert.deepEqual([...parsed.mustFix[0].lenses].sort(), ['claude', 'codex'])
    assert.equal(parsed.pool.length, 0)
    assert.equal(parsed.invalid.length, 0)
  } finally {
    rmSync(base, { recursive: true, force: true })
  }
})

// A findings file whose top level is not an array PARSES, so a reader that only
// spreads arrays reports it as "nothing found" — indistinguishable from a clean
// review. The object wrapper is the realistic shape (a reviewer returning
// `{"findings": [...]}` instead of a bare array), so it is asserted by name.
test('CLI categorize reports a non-array findings file as invalid instead of dropping it', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    writeFileSync(
      join(dir, 'findings-wrapped.json'),
      JSON.stringify({ findings: [finding({ severity: 'Critical', lens: 'claude' })] }),
    )
    writeFileSync(join(dir, 'findings-bare.json'), JSON.stringify(finding({ lens: 'codex' })))
    writeFileSync(join(dir, 'findings-gemini.json'), JSON.stringify([finding({ lens: 'gemini' })]))

    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.invalid.length, 2)
    const reasons = parsed.invalid.map((entry) => entry.reason).sort()
    assert.deepEqual(reasons, [
      'findings-bare.json: top level is not a list of findings',
      'findings-wrapped.json: top level is not a list of findings',
    ])
    // The well-formed sibling file is still triaged normally.
    assert.equal(parsed.pool.length, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['gemini'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// Truncated reviewer output is routine. Aborting the directory over one bad
// file would discard every OTHER reviewer's findings — the same loss, widened.
test('CLI categorize reports an unparseable findings file by name and still triages its siblings', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    writeFileSync(join(dir, 'findings-truncated.json'), '[{"severity":"Critical",')
    writeFileSync(join(dir, 'findings-codex.json'), JSON.stringify([finding({ lens: 'codex' })]))

    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.invalid.length, 1)
    // The reason must name the file: JSON.parse messages carry no filename, so
    // an unnamed parse error leaves the operator unable to find the bad file.
    assert.match(parsed.invalid[0].reason, /^findings-truncated\.json: /)
    // The sibling reviewer's findings survive the bad file.
    assert.equal(parsed.pool.length, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['codex'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// The helper documents a deliberate ordering: file-level rejections lead,
// because a whole reviewer going unread outranks one malformed item. Without
// this case, switching `unshift` to `push` would keep every other test green.
test('CLI categorize lists file-level rejections ahead of item-level ones', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    // Sorts LAST by filename, so only the deliberate ordering can put it first.
    writeFileSync(join(dir, 'findings-zz-wrapped.json'), JSON.stringify({ findings: [] }))
    writeFileSync(join(dir, 'findings-aa-items.json'), JSON.stringify([finding({ title: '' })]))

    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.invalid.length, 2)
    assert.equal(
      parsed.invalid[0].reason,
      'findings-zz-wrapped.json: top level is not a list of findings',
    )
    assert.equal(parsed.invalid[1].reason, 'missing or blank title')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// A reviewer extension writes a result envelope, not a bare array, and its
// findings live under `items`. Reading only bare arrays made every
// extension-produced file report as unread -- on a repo whose reviewers are all
// extensions, that is the entire round going missing.
test('CLI categorize validates an extension envelope and assigns its stable reviewer ID', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    writeFileSync(
      join(dir, 'findings-round-ext.json'),
      JSON.stringify({
        ok: true,
        // Deliberately NOT the filename slug: identity comes from the dispatch
        // file, not from what the envelope calls itself.
        extension: 'some-round',
        role: 'round',
        // The extension contract intentionally omits per-item `lens`.
        items: [finding({ severity: 'Warning', lens: undefined })],
        notes: '',
        error: null,
      }),
    )
    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    assert.equal(parsed.mustFix.length, 1)
    assert.equal(parsed.mustFix[0].promotedBy, 'severity')
    assert.deepEqual(parsed.mustFix[0].lenses, ['round-ext'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// `validateResult` only checks that `extension` is a non-empty string, so two
// round extensions can declare the same name -- one copying another's envelope
// example. Trusting that field would collapse them into a single reviewer, and
// their agreeing Suggestions would never reach the convergence threshold. The
// filenames differ by construction (one file per dispatch), so identity taken
// from the filename keeps them distinct.
test('CLI categorize keeps two round extensions distinct despite a duplicate declared name', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const envelope = (extension) =>
      JSON.stringify({
        ok: true,
        extension,
        role: 'round',
        items: [finding({ severity: 'Suggestion', lens: undefined })],
        notes: '',
        error: null,
      })
    // Both copied the same `extension` value out of the same example.
    writeFileSync(join(dir, 'findings-round-alpha.json'), envelope('copied-name'))
    writeFileSync(join(dir, 'findings-round-beta.json'), envelope('copied-name'))

    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    // Two distinct reviewers agreeing on one Suggestion => convergence.
    assert.equal(parsed.pool.length, 0)
    assert.equal(parsed.mustFix.length, 1)
    assert.equal(parsed.mustFix[0].promotedBy, 'convergence')
    assert.equal(parsed.mustFix[0].reviewerCount, 2)
    assert.deepEqual(parsed.mustFix[0].lenses, ['round-alpha', 'round-beta'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// THE FALSIFICATION CASE: identity is per-DISPATCH, not per-file-that-exists.
// Several Tier 1 descriptors can legitimately serve ONE configured lens, so an
// indexed lens file must still resolve to its configured skill -- deriving
// every envelope's identity from its own filename slug would falsely INFLATE
// reviewer count there, which is the opposite bug.
test('CLI categorize still maps indexed lens envelopes to one configured skill', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const lensEntries = join(dir, 'lens-entries.json')
    writeFileSync(
      lensEntries,
      JSON.stringify([{ skill: 'shared-reviewer' }, { skill: 'shared-reviewer' }]),
    )
    const envelope = (extension) =>
      JSON.stringify({
        ok: true,
        extension,
        role: 'lens',
        items: [finding({ severity: 'Suggestion', lens: undefined })],
        notes: '',
        error: null,
      })
    writeFileSync(join(dir, 'findings-lens-0-first.json'), envelope('first-descriptor'))
    writeFileSync(join(dir, 'findings-lens-1-second.json'), envelope('second-descriptor'))

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--lens-entries-file',
      lensEntries,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    // ONE configured reviewer reporting twice is not convergence.
    assert.equal(parsed.mustFix.length, 0)
    assert.equal(parsed.pool.length, 1)
    assert.equal(parsed.pool[0].reviewerCount, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['shared-reviewer'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI categorize preserves the configured skill for every indexed lens extension', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const envelope = (extension) => ({
      ok: true,
      extension,
      role: 'lens',
      items: [finding({ lens: undefined })],
      notes: '',
      error: null,
    })
    writeFileSync(join(dir, 'findings-lens-0-first.json'), JSON.stringify(envelope('first')))
    writeFileSync(join(dir, 'findings-lens-1-second.json'), JSON.stringify(envelope('second')))
    const lensEntries = join(dir, 'lens-entries.json')
    writeFileSync(
      lensEntries,
      JSON.stringify([{ skill: 'configured-reviewer' }, { skill: 'configured-reviewer' }]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--lens-entries-file',
      lensEntries,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    assert.equal(parsed.mustFix.length, 0)
    assert.equal(parsed.pool.length, 1)
    assert.equal(parsed.pool[0].reviewerCount, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['configured-reviewer'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI categorize reads every entry-indexed Tier 2 fallback sharing one skill', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const lensEntries = join(dir, 'lens-entries.json')
    const expectedOutputs = join(dir, 'expected-outputs.json')
    writeFileSync(
      lensEntries,
      JSON.stringify([{ skill: 'shared-reviewer' }, { skill: 'shared-reviewer' }]),
    )
    writeFileSync(
      expectedOutputs,
      JSON.stringify([
        'findings-lens-0-shared-reviewer.json',
        'findings-lens-1-shared-reviewer.json',
      ]),
    )
    writeFileSync(
      join(dir, 'findings-lens-0-shared-reviewer.json'),
      JSON.stringify([finding({ title: 'first entry', lens: 'untrusted' })]),
    )
    writeFileSync(
      join(dir, 'findings-lens-1-shared-reviewer.json'),
      JSON.stringify([finding({ title: 'second entry', lens: 'untrusted' })]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--lens-entries-file',
      lensEntries,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    assert.equal(parsed.pool.length, 2)
    assert.deepEqual(
      parsed.pool.map((group) => group.lenses),
      [['shared-reviewer'], ['shared-reviewer']],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// The scenario above is exactly what makes the configured lens identity
// insufficient for repair: two output files, one configured skill. A
// blank-detail rejection that reported only the merged group would name
// "shared-reviewer" and leave the invalid-only repair path unable to tell WHICH
// output to replace -- so it could re-run and cap without ever fixing it.
test('CLI categorize retains the output file behind each blank-detail rejection', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const lensEntries = join(dir, 'lens-entries.json')
    writeFileSync(
      lensEntries,
      JSON.stringify([{ skill: 'shared-reviewer' }, { skill: 'shared-reviewer' }]),
    )
    // Same (file, line, title) in both files, so they merge into ONE group --
    // and neither occurrence explains the defect.
    writeFileSync(
      join(dir, 'findings-lens-0-shared-reviewer.json'),
      JSON.stringify([finding({ detail: '' })]),
    )
    writeFileSync(
      join(dir, 'findings-lens-1-shared-reviewer.json'),
      JSON.stringify([finding({ detail: '   ' })]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--lens-entries-file',
      lensEntries,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.mustFix.length, 0)
    assert.equal(parsed.pool.length, 0)
    // One entry per contributing occurrence, each naming its own output file.
    assert.equal(parsed.invalid.length, 2)
    assert.deepEqual(parsed.invalid.map((entry) => entry.source?.filename).sort(), [
      'findings-lens-0-shared-reviewer.json',
      'findings-lens-1-shared-reviewer.json',
    ])
    // The reviewer identity is identical across both -- which is the point.
    assert.deepEqual(
      parsed.invalid.map((entry) => entry.source?.reviewer),
      ['shared-reviewer', 'shared-reviewer'],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI categorize marks a selected reviewer with no output as invalid', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const expectedOutputs = join(dir, 'expected-outputs.json')
    writeFileSync(expectedOutputs, '[]\n')
    const expected = 'findings-round-inline.json'
    const registration = runCli(['expect', expectedOutputs, expected])
    assert.equal(registration.status, 0, registration.stderr)

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.mustFix, [])
    assert.deepEqual(parsed.pool, [])
    assert.deepEqual(parsed.invalid, [
      { item: null, reason: `${expected}: expected reviewer output is missing` },
    ])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI categorize rejects failed or mis-roled extension envelopes before triage', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    writeFileSync(
      join(dir, 'findings-round-failed.json'),
      JSON.stringify({
        ok: false,
        extension: 'failed-round',
        role: 'round',
        items: [finding({ severity: 'Warning' })],
        notes: '',
        error: 'review helper timed out',
      }),
    )
    writeFileSync(
      join(dir, 'findings-round-wrong-role.json'),
      JSON.stringify({
        ok: true,
        extension: 'wrong-role',
        role: 'lens',
        items: [finding({ severity: 'Warning' })],
        notes: '',
        error: null,
      }),
    )
    writeFileSync(join(dir, 'findings-bare.json'), JSON.stringify([finding({ lens: 'bare' })]))

    const { stdout, stderr, status } = runCli(['categorize', dir])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.mustFix.length, 0)
    assert.equal(parsed.pool.length, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['bare'])
    assert.equal(parsed.invalid.length, 2)
    assert.match(parsed.invalid[0].reason, /extension reported failure \(ok:false\)/)
    assert.match(parsed.invalid[1].reason, /does not match expected "round"/)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI categorize ignores a skipped Tier 1 envelope once its selected fallback succeeds', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const expectedOutputs = join(dir, 'expected-outputs.json')
    writeFileSync(expectedOutputs, JSON.stringify(['findings-fallback-reviewer.json']))
    writeFileSync(
      join(dir, 'findings-lens-0-failed-extension.json'),
      JSON.stringify({
        ok: false,
        extension: 'failed-extension',
        role: 'lens',
        items: [],
        notes: '',
        error: 'timed out',
      }),
    )
    writeFileSync(
      join(dir, 'findings-fallback-reviewer.json'),
      JSON.stringify([finding({ lens: 'ignored-model-lens' })]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    assert.equal(parsed.pool.length, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['fallback-reviewer'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// THE FALSIFICATION CASE for the skip above, and the reason the contract-valid
// envelope path must NOT consult the roster the way the bare-array and
// rejection paths do. The roster holds only the Tier 2/3 fallback selected
// AFTER every Tier 1 descriptor settled (`expect` is called at exactly three
// sites in SKILL.md, all of them fallbacks), so a Tier 1 output filename is
// never registered — whether that dispatch failed OR succeeded. Absence from
// the roster therefore does not distinguish a superseded Tier 1 attempt from a
// successful one; only the FILE SHAPE does, which is why the skip lives on the
// paths a successful Tier 1 extension can never take. A contract-valid envelope
// IS that success, so extending the skip to it would silently discard every
// working repo-local round/lens extension's findings — including Criticals —
// the moment any unrelated lens rostered a fallback.
test('CLI categorize reads a successful Tier 1 envelope that the roster does not list', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const expectedOutputs = join(dir, 'expected-outputs.json')
    // A fallback rostered for some OTHER reviewer; Tier 1 successes are never
    // registered here, so this round extension's file is absent from the roster.
    writeFileSync(expectedOutputs, JSON.stringify(['findings-round-builtin.json']))
    writeFileSync(
      join(dir, 'findings-round-thermonuclear.json'),
      JSON.stringify({
        ok: true,
        extension: 'boss-review-thermonuclear',
        role: 'round',
        items: [finding({ severity: 'Critical', title: 'from the successful extension' })],
        notes: '',
        error: null,
      }),
    )
    writeFileSync(
      join(dir, 'findings-round-builtin.json'),
      JSON.stringify([finding({ severity: 'Warning', title: 'from the rostered fallback' })]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    assert.deepEqual(parsed.mustFix.map((group) => group.title).sort(), [
      'from the rostered fallback',
      'from the successful extension',
    ])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// The superseded Tier 1 attempt above failed envelope validation, which is only
// one of the ways a Tier 1 extension leaves a bad file behind. A truncated write
// never parses and a non-envelope object never reaches validation at all, so if
// the skip were keyed on the failure SHAPE rather than on the roster, a run
// holding valid fallback output would still be capped by its own dead predecessor.
test('CLI categorize ignores superseded Tier 1 files that fail before envelope validation', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const expectedOutputs = join(dir, 'expected-outputs.json')
    writeFileSync(expectedOutputs, JSON.stringify(['findings-fallback-reviewer.json']))
    // Truncated output: JSON.parse throws before any role/envelope check runs.
    writeFileSync(join(dir, 'findings-lens-0-truncated.json'), '[{"severity":"Warn')
    // Parses, but the top level is neither an array nor an envelope.
    writeFileSync(join(dir, 'findings-lens-1-wrapped.json'), JSON.stringify({ findings: [] }))
    // Parses to a bare array, but no lens entry backs its index.
    writeFileSync(
      join(dir, 'findings-lens-9-unbacked.json'),
      JSON.stringify([finding({ lens: 'untrusted' })]),
    )
    writeFileSync(
      join(dir, 'findings-fallback-reviewer.json'),
      JSON.stringify([finding({ lens: 'ignored-model-lens' })]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    assert.equal(parsed.pool.length, 1)
    assert.deepEqual(parsed.pool[0].lenses, ['fallback-reviewer'])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// THE FALSIFICATION CASE for the skip above: it is keyed on the roster, not on
// the failure being survivable. A file the round SELECTED is the round's own
// evidence, so the identical truncation must still block a clean verdict --
// otherwise the skip would have been widened into "swallow every bad file".
test('CLI categorize still rejects a rostered file that fails to parse', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const expectedOutputs = join(dir, 'expected-outputs.json')
    writeFileSync(expectedOutputs, JSON.stringify(['findings-round-selected.json']))
    writeFileSync(join(dir, 'findings-round-selected.json'), '[{"severity":"Warn')

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.invalid.length, 1)
    assert.match(parsed.invalid[0].reason, /^findings-round-selected\.json: /)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// A superseded file's FINDINGS are skipped too, not just its failures. A Tier 1
// extension must emit an envelope; a bare array under an indexed lens filename
// means it broke that contract, so Phase 1 records it skipped and runs a
// fallback. The filename still resolves to a configured lens, so without the
// roster check the rejected extension's Critical items would be consumed
// alongside the fallback's and drive fixes the ledger says were never collected.
test('CLI categorize drops findings from a non-rostered Tier 1 bare array', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const lensEntries = join(dir, 'lens-entries.json')
    const expectedOutputs = join(dir, 'expected-outputs.json')
    writeFileSync(lensEntries, JSON.stringify([{ skill: 'configured-lens' }]))
    writeFileSync(expectedOutputs, JSON.stringify(['findings-lens-0-fallback.json']))
    // The contract-breaking Tier 1 output: a bare array, not an envelope.
    writeFileSync(
      join(dir, 'findings-lens-0-failed-extension.json'),
      JSON.stringify([finding({ severity: 'Critical', title: 'from the skipped extension' })]),
    )
    // The selected fallback that actually ran.
    writeFileSync(
      join(dir, 'findings-lens-0-fallback.json'),
      JSON.stringify([finding({ severity: 'Critical', title: 'from the fallback' })]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--lens-entries-file',
      lensEntries,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    // Skipped silently: Phase 1 already recorded the extension as skipped, so
    // this is a ledger skip and not unread evidence.
    assert.deepEqual(parsed.invalid, [])
    assert.deepEqual(
      parsed.mustFix.map((group) => group.title),
      ['from the fallback'],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// THE FALSIFICATION CASE for the skip above: it is keyed on the roster, not on
// the shape. A ROSTERED bare array is a legitimate Tier 2/3 fallback output and
// must still be read, or the skip would silently discard real reviewer findings.
test('CLI categorize still reads a rostered bare array', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    const expectedOutputs = join(dir, 'expected-outputs.json')
    writeFileSync(expectedOutputs, JSON.stringify(['findings-rostered-fallback.json']))
    writeFileSync(
      join(dir, 'findings-rostered-fallback.json'),
      JSON.stringify([finding({ severity: 'Critical', title: 'real fallback finding' })]),
    )

    const { stdout, stderr, status } = runCli([
      'categorize',
      dir,
      '--expected-outputs-file',
      expectedOutputs,
    ])
    assert.equal(status, 0, stderr)
    const parsed = JSON.parse(stdout)
    assert.deepEqual(parsed.invalid, [])
    assert.deepEqual(
      parsed.mustFix.map((group) => group.title),
      ['real fallback finding'],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// An arbitrary wrapper is NOT the envelope contract and must stay invalid --
// otherwise the round-1 silent-drop guard would be widened into a guess.
test('CLI categorize still rejects a wrapper object that is not the envelope shape', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-triage-'))
  try {
    writeFileSync(join(dir, 'findings-guess.json'), JSON.stringify({ findings: [finding()] }))
    const { stdout } = runCli(['categorize', dir])
    const parsed = JSON.parse(stdout)
    assert.equal(parsed.invalid.length, 1)
    assert.match(parsed.invalid[0].reason, /not a list of findings/)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI categorize prints usage and exits non-zero with no directory argument', () => {
  const r = runCli(['categorize'])
  assert.notEqual(r.status, 0)
  assert.match(r.stderr, /usage/)
  assert.equal(r.stdout, '')
})

test('CLI exits non-zero on an unknown subcommand', () => {
  const r = runCli(['bogus'])
  assert.notEqual(r.status, 0)
  assert.match(r.stderr, /usage/)
})
