// The specification for bs-mutation-obligations.mjs.
//
// The module's whole claim is that a one-mutant proof CANNOT read as discharged for
// a multi-mutant shape, so these cases are written as the falsifications of that
// claim: one case per shape asserting the exact obliged set and each mutant's
// required verdict, then the fail-closed cases — a missing mutant, a contradicted
// verdict, a relabelled obligation, a green-expected mutant that reds alone will not
// satisfy, a skip that names the source language instead of the consuming layer, and
// an operator error that must not surface a verdict word.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  ADJUDICATIONS,
  CLASSIFIED_SHAPE_NAMES,
  MIN_SHAPE_JUSTIFICATION_CHARS,
  DISCHARGE_MECHANISMS,
  MUTANT_VERDICTS,
  MUTATION_SHAPES,
  SHAPE_NAMES,
  SOURCE_LANGUAGE_TOKENS,
  adjudicateProofRecord,
  describeShapes,
  obligedMutants,
} from './bs-mutation-obligations.mjs'

const scriptPath = fileURLToPath(new URL('./bs-mutation-obligations.mjs', import.meta.url))

const run = (args) => spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8' })

const recordFile = (contents) => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-mutation-obligations-'))
  const file = join(dir, 'record.json')
  writeFileSync(file, typeof contents === 'string' ? contents : JSON.stringify(contents))
  return file
}

/** Build a record that satisfies `shape` outright, so a case can spoil exactly one field. */
function satisfyingRecord(shape, overrides = {}) {
  const mutants = obligedMutants(shape).map((mutant) => ({
    id: mutant.id,
    expected: mutant.expected,
    observed: mutant.expected,
    ...(mutant.justification === true
      ? { justification: 'recorded argument for this mutant' }
      : {}),
  }))
  return { shape, mutants, ...overrides }
}

const kinds = (result) => result.failures.map((entry) => entry.kind)

/** An `other` justification that clears the substance floor: long enough, and naming a shape it is not. */
const SUBSTANTIVE_OTHER =
  'a generated-file regeneration with no guard to mutate: not a replacement because no condition or literal was swapped, and not a new-gate because no gate was added'

// ---------------------------------------------------------------------------
// The enumeration — one case per shape, asserting the exact obliged mutant set.
// ---------------------------------------------------------------------------

test('the enumeration covers exactly the classified shapes plus the escape hatch', () => {
  assert.deepEqual(SHAPE_NAMES, [
    'replacement',
    'widening',
    'tightening',
    'compound-guard',
    'new-gate',
    'text-assertion',
    'skip-rule',
    'design-departure',
    'other',
  ])
  assert.deepEqual(
    CLASSIFIED_SHAPE_NAMES,
    SHAPE_NAMES.filter((name) => name !== 'other'),
  )
  // 'unclassified' is a THIRD verdict, and deliberately not a prefix of 'satisfied':
  // a caller grepping `^satisfied` must not read the escape hatch as a proof.
  assert.deepEqual(ADJUDICATIONS, ['satisfied', 'unclassified', 'insufficient'])
  assert.ok(!'unclassified'.startsWith('satisfied'))
  assert.deepEqual(MUTANT_VERDICTS, ['red', 'green'])
  // test-first-red is an EQUAL discharge to a sandboxed probe, not a lesser one.
  assert.deepEqual(DISCHARGE_MECHANISMS, ['probe', 'test-first-red'])
})

const OBLIGATIONS = {
  replacement: [['revert-pre-fix-text', 'red']],
  widening: [
    ['revert-added-clause', 'red'],
    ['sibling-under-same-mutation', 'green'],
  ],
  tightening: [
    ['revert-narrowing', 'red'],
    ['newly-rejected-input', 'red'],
  ],
  'compound-guard': [
    ['remove-whole-guard', 'red'],
    ['drop-clause-a', 'red'],
    ['drop-clause-b', 'red'],
  ],
  'new-gate': [
    ['new-gate-on-violation', 'red'],
    ['pre-fix-gate-on-violation', 'green'],
  ],
  'text-assertion': [
    ['revert-pre-fix-text', 'red'],
    ['perturb-new-pin', 'red'],
  ],
  'skip-rule': [
    ['revert-widened-rule', 'red'],
    ['legitimately-skipped-input', 'green'],
  ],
  'design-departure': [
    ['revert-chosen-implementation', 'red'],
    ['suggested-alternative', 'red'],
  ],
  other: [],
}

for (const [shape, expected] of Object.entries(OBLIGATIONS)) {
  test(`shape '${shape}' owes exactly its enumerated mutants, each with its required verdict`, () => {
    assert.deepEqual(
      obligedMutants(shape).map((mutant) => [mutant.id, mutant.expected]),
      expected,
    )
    // A satisfying record for the shape must actually satisfy it, or every
    // fail-closed case below would be passing for the wrong reason.
    const result = adjudicateProofRecord(
      satisfyingRecord(shape, shape === 'other' ? { shapeJustification: SUBSTANTIVE_OTHER } : {}),
    )
    // Only the escape hatch is exempt: it runs no mutant, so its clean outcome is the
    // recorded admission, never the proof verdict.
    assert.equal(
      result.verdict,
      shape === 'other' ? 'unclassified' : 'satisfied',
      JSON.stringify(result.failures),
    )
  })
}

test('every shape a consumer can classify into obliges at least one mutant, except the escape hatch', () => {
  for (const name of SHAPE_NAMES) {
    if (name === 'other') {
      assert.equal(obligedMutants(name).length, 0)
      assert.equal(MUTATION_SHAPES[name].requiresShapeJustification, true)
      continue
    }
    assert.ok(obligedMutants(name).length >= 1, name)
  }
})

test('describeShapes is the serialisable form of the same enumeration', () => {
  const described = describeShapes()
  assert.deepEqual(
    described.map((entry) => entry.shape),
    SHAPE_NAMES,
  )
  for (const entry of described) {
    assert.ok(entry.trigger.length > 0, entry.shape)
    assert.ok(entry.why.length > 0, entry.shape)
    for (const mutant of entry.mutants) {
      assert.ok(MUTANT_VERDICTS.includes(mutant.expected), `${entry.shape}/${mutant.id}`)
      assert.equal(typeof mutant.justification, 'boolean')
      assert.ok(mutant.description.length > 0)
    }
  }
})

test('an unknown shape is not adjudicable and never satisfied', () => {
  const result = adjudicateProofRecord({ shape: 'refactor', mutants: [] })
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(kinds(result), ['unknown-shape'])
})

// ---------------------------------------------------------------------------
// Fail-closed: the one-mutant record for a multi-mutant shape.
// ---------------------------------------------------------------------------

test('a widening record carrying ONLY the revert mutant is insufficient and names the missing green sibling', () => {
  const result = adjudicateProofRecord({
    shape: 'widening',
    mutants: [{ id: 'revert-added-clause', expected: 'red', observed: 'red' }],
  })
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(result.missing, ['sibling-under-same-mutation'])
  assert.deepEqual(kinds(result), ['mutant-missing'])
  assert.match(result.failures[0].detail, /sibling-under-same-mutation/)
  assert.match(result.failures[0].detail, /green/)
})

test('a compound-guard record carrying fewer than three mutants is insufficient', () => {
  const result = adjudicateProofRecord({
    shape: 'compound-guard',
    mutants: [
      { id: 'remove-whole-guard', expected: 'red', observed: 'red' },
      { id: 'drop-clause-a', expected: 'red', observed: 'red' },
    ],
  })
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(result.missing, ['drop-clause-b'])
})

test('a mutant whose observed verdict contradicts its required verdict is insufficient and named', () => {
  const record = satisfyingRecord('new-gate')
  record.mutants[1].observed = 'red' // the PRE-fix gate must stay GREEN
  const result = adjudicateProofRecord(record)
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(kinds(result), ['mutant-observed-mismatch'])
  assert.equal(result.failures[0].mutantId, 'pre-fix-gate-on-violation')
})

test('a green-expected obligation is not satisfiable by reds alone', () => {
  // Every mutant red — the shape an author produces unprompted.
  const result = adjudicateProofRecord({
    shape: 'skip-rule',
    mutants: [
      { id: 'revert-widened-rule', expected: 'red', observed: 'red' },
      {
        id: 'legitimately-skipped-input',
        expected: 'red',
        observed: 'red',
        justification: 'the dropped set is X and Y',
      },
    ],
  })
  assert.equal(result.verdict, 'insufficient')
  // Both halves fire: the record relabelled the obligation AND observed the wrong verdict.
  assert.deepEqual(kinds(result), ['mutant-expected-mismatch', 'mutant-observed-mismatch'])
})

test('a record may not relabel a required verdict to the one it happens to have observed', () => {
  const result = adjudicateProofRecord({
    shape: 'widening',
    mutants: [
      { id: 'revert-added-clause', expected: 'red', observed: 'red' },
      { id: 'sibling-under-same-mutation', expected: 'red', observed: 'red' },
    ],
  })
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(kinds(result), ['mutant-expected-mismatch', 'mutant-observed-mismatch'])
})

test('a mutant that omits its required verdict, or records an unreadable observation, is insufficient', () => {
  const missingExpected = adjudicateProofRecord({
    shape: 'replacement',
    mutants: [{ id: 'revert-pre-fix-text', observed: 'red' }],
  })
  assert.equal(missingExpected.verdict, 'insufficient')
  assert.deepEqual(kinds(missingExpected), ['mutant-expected-missing'])

  const unreadable = adjudicateProofRecord({
    shape: 'replacement',
    mutants: [{ id: 'revert-pre-fix-text', expected: 'red', observed: 'probably red' }],
  })
  assert.equal(unreadable.verdict, 'insufficient')
  assert.deepEqual(kinds(unreadable), ['mutant-observed-invalid'])
})

test('a mutant obliged to carry an argument is not discharged by its verdict alone', () => {
  const record = satisfyingRecord('tightening')
  delete record.mutants[1].justification
  const result = adjudicateProofRecord(record)
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(kinds(result), ['mutant-justification-missing'])
  assert.equal(result.failures[0].mutantId, 'newly-rejected-input')
})

test('test-first-red discharges a mutant exactly as a probe does, and an invented mechanism does not', () => {
  const testFirst = satisfyingRecord('replacement')
  testFirst.mutants[0].mechanism = 'test-first-red'
  assert.equal(adjudicateProofRecord(testFirst).verdict, 'satisfied')

  const invented = satisfyingRecord('replacement')
  invented.mutants[0].mechanism = 'i-read-the-code'
  const result = adjudicateProofRecord(invented)
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(kinds(result), ['mutant-mechanism-unknown'])
})

// ---------------------------------------------------------------------------
// Fail-closed: skips are judged at the PARSING layer.
// ---------------------------------------------------------------------------

test('a skip whose justification names the source language rather than the consuming layer is insufficient', () => {
  const result = adjudicateProofRecord({
    shape: 'widening',
    mutants: [{ id: 'revert-added-clause', expected: 'red', observed: 'red' }],
    skips: [
      {
        mutantId: 'sibling-under-same-mutation',
        reason: 'the mutated input could never occur',
        consumingLayer: 'Go',
      },
    ],
  })
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(kinds(result), ['skip-layer-is-source-language', 'mutant-missing'])
})

test('a skip that names the layer which actually parses the input is accepted', () => {
  const result = adjudicateProofRecord({
    shape: 'widening',
    mutants: [{ id: 'revert-added-clause', expected: 'red', observed: 'red' }],
    skips: [
      {
        mutantId: 'sibling-under-same-mutation',
        reason: 'no sibling test reaches this clause',
        consumingLayer:
          'a text scanner that reads the file with readFileSync and splits on newlines; it never compiles the source language',
      },
    ],
  })
  assert.equal(result.verdict, 'satisfied', JSON.stringify(result.failures))
})

test('a skip missing its reason or its consuming layer is insufficient', () => {
  const noLayer = adjudicateProofRecord({
    shape: 'widening',
    mutants: [{ id: 'revert-added-clause', expected: 'red', observed: 'red' }],
    skips: [{ mutantId: 'sibling-under-same-mutation', reason: 'unreachable' }],
  })
  assert.equal(noLayer.verdict, 'insufficient')
  assert.deepEqual(kinds(noLayer), ['skip-layer-missing', 'mutant-missing'])

  const noReason = adjudicateProofRecord({
    shape: 'widening',
    mutants: [{ id: 'revert-added-clause', expected: 'red', observed: 'red' }],
    skips: [{ mutantId: 'sibling-under-same-mutation', consumingLayer: 'the TAP reporter' }],
  })
  assert.equal(noReason.verdict, 'insufficient')
  assert.deepEqual(kinds(noReason), ['skip-reason-missing', 'mutant-missing'])
})

test('a skip naming a mutant the shape does not oblige is insufficient', () => {
  const record = satisfyingRecord('replacement')
  record.skips = [{ mutantId: 'drop-clause-b', reason: 'n/a', consumingLayer: 'the TAP reporter' }]
  const result = adjudicateProofRecord(record)
  assert.equal(result.verdict, 'insufficient')
  assert.deepEqual(kinds(result), ['skip-target-unknown'])
})

// NON-TAUTOLOGICAL counterpart to the bare-token sweep below. That sweep iterates the
// very list the matcher is built from, so it cannot see HOW the matcher matches: it
// passed unchanged while the matcher was an exact whole-string compare, and every one
// of these phrases — a source-language token plus one adjacent word — sailed through
// the only mechanical check the module applies to a skip. Each fixture here is a
// literal an author actually writes, not a member of SOURCE_LANGUAGE_TOKENS.
test('a consuming layer that wraps a source-language token in scaffolding is still rejected', () => {
  const rejected = [
    'Go source',
    'the Go compiler',
    'Go parser',
    'golang toolchain',
    'the TypeScript compiler',
    'the Go language spec',
    'Python syntax',
    'the markdown parser',
  ]
  for (const layer of rejected) {
    const result = adjudicateProofRecord({
      shape: 'replacement',
      skips: [
        { mutantId: 'revert-pre-fix-text', reason: 'impossible input', consumingLayer: layer },
      ],
    })
    assert.ok(kinds(result).includes('skip-layer-is-source-language'), layer)
  }

  // ...and a layer that genuinely names what parses the input is still accepted, so the
  // widening is not simply "reject everything".
  const accepted = [
    'a text scanner that reads the file with readFileSync and splits on newlines',
    'the TAP reporter',
    'the commit-msg hook regex',
    'gofmt, which reformats before the assertion reads the bytes',
  ]
  for (const layer of accepted) {
    const skipped = adjudicateProofRecord({
      shape: 'widening',
      mutants: [{ id: 'revert-added-clause', expected: 'red', observed: 'red' }],
      skips: [
        {
          mutantId: 'sibling-under-same-mutation',
          reason: 'no sibling reaches it',
          consumingLayer: layer,
        },
      ],
    })
    assert.equal(skipped.verdict, 'satisfied', layer)
  }
})

test('every bare source-language token is rejected as a consuming layer', () => {
  for (const token of SOURCE_LANGUAGE_TOKENS) {
    const result = adjudicateProofRecord({
      shape: 'replacement',
      skips: [
        { mutantId: 'revert-pre-fix-text', reason: 'impossible input', consumingLayer: token },
      ],
    })
    assert.ok(kinds(result).includes('skip-layer-is-source-language'), token)
  }
})

// ---------------------------------------------------------------------------
// Fail-closed: the escape hatch cannot pass silently.
// ---------------------------------------------------------------------------

test("shape 'other' obliges no mutants but never discharges without a written justification", () => {
  const bare = adjudicateProofRecord({ shape: 'other', mutants: [] })
  assert.equal(bare.verdict, 'insufficient')
  assert.deepEqual(kinds(bare), ['shape-justification-missing'])

  const justified = adjudicateProofRecord({
    shape: 'other',
    mutants: [],
    shapeJustification: SUBSTANTIVE_OTHER,
  })
  // NOT 'satisfied'. Zero mutants ran, so nothing was proven; the best this record can
  // reach is the recorded admission.
  assert.equal(justified.verdict, 'unclassified')
  // The helper ECHOES the justification back, so it lands in the caller's artefact
  // rather than being swallowed by an exit code.
  assert.equal(justified.echoedJustification, SUBSTANTIVE_OTHER)
})

// The fail-open this closes, stated as its own falsification: before it, `"x"` bought
// `satisfied` and exit 0 — the same token and the same status a three-mutant proof
// earns — so no caller reading either could tell a discharged proof from a keystroke.
test("shape 'other' refuses a token justification and never reaches 'satisfied' or exit 0", () => {
  for (const token of ['x', 'n/a', 'unclassifiable', 'does not fit']) {
    const result = adjudicateProofRecord({ shape: 'other', shapeJustification: token })
    assert.equal(result.verdict, 'insufficient', token)
    assert.deepEqual(kinds(result), ['shape-justification-insufficient'], token)
    assert.equal(result.echoedJustification, null, token)
  }

  // Long enough, but naming no classified shape: the author never engaged the enumeration.
  const unengaged = adjudicateProofRecord({
    shape: 'other',
    shapeJustification: 'z'.repeat(MIN_SHAPE_JUSTIFICATION_CHARS + 40),
  })
  assert.equal(unengaged.verdict, 'insufficient')
  assert.deepEqual(kinds(unengaged), ['shape-justification-insufficient'])

  // And the CLI seam: a clean escape-hatch record still exits non-zero and does not
  // print a line a `^satisfied` grep would credit.
  const cli = run([
    'adjudicate',
    '--record',
    recordFile({ shape: 'other', shapeJustification: SUBSTANTIVE_OTHER }),
  ])
  assert.equal(cli.status, 1)
  assert.match(cli.stdout, /^unclassified/)
  assert.doesNotMatch(cli.stdout, /^satisfied/)
})

test('a record that is not an object is insufficient rather than a crash', () => {
  for (const value of [null, 'widening', 42, ['widening']]) {
    const result = adjudicateProofRecord(value)
    assert.equal(result.verdict, 'insufficient')
    assert.deepEqual(kinds(result), ['record-not-an-object'])
  }
})

test('a mutant not obliged by the shape is reported as extra rather than credited', () => {
  const record = satisfyingRecord('replacement')
  record.mutants.push({ id: 'drop-clause-a', expected: 'red', observed: 'red' })
  const result = adjudicateProofRecord(record)
  assert.equal(result.verdict, 'satisfied')
  assert.deepEqual(result.extra, ['drop-clause-a'])
})

// ---------------------------------------------------------------------------
// The CLI: exit codes, and the rule that an operator error is never a verdict.
// ---------------------------------------------------------------------------

test('shapes prints the enumeration, and --json round-trips describeShapes', () => {
  const human = run(['shapes'])
  assert.equal(human.status, 0)
  for (const name of SHAPE_NAMES) assert.match(human.stdout, new RegExp(name))

  const json = run(['shapes', '--json'])
  assert.equal(json.status, 0)
  assert.deepEqual(JSON.parse(json.stdout), describeShapes())
})

test('adjudicate exits 0 on satisfied and 1 on insufficient', () => {
  const satisfied = run(['adjudicate', '--record', recordFile(satisfyingRecord('replacement'))])
  assert.equal(satisfied.status, 0)
  assert.match(satisfied.stdout, /^satisfied/)

  const insufficient = run([
    'adjudicate',
    '--record',
    recordFile({
      shape: 'widening',
      mutants: [{ id: 'revert-added-clause', expected: 'red', observed: 'red' }],
    }),
  ])
  assert.equal(insufficient.status, 1)
  assert.match(insufficient.stdout, /^insufficient/)
  assert.match(insufficient.stdout, /sibling-under-same-mutation/)
})

test('an absent or unreadable record exits 2 and prints no verdict word', () => {
  const absent = run(['adjudicate', '--record', join(tmpdir(), 'no-such-proof-record.json')])
  assert.equal(absent.status, 2)
  for (const stream of [absent.stdout, absent.stderr]) {
    assert.doesNotMatch(stream, /satisfied|insufficient/)
  }

  const unparseable = run(['adjudicate', '--record', recordFile('{ not json')])
  assert.equal(unparseable.status, 2)
  for (const stream of [unparseable.stdout, unparseable.stderr]) {
    assert.doesNotMatch(stream, /satisfied|insufficient/)
  }
})

test('a usage error exits 2 and prints no verdict word', () => {
  for (const args of [[], ['adjudicate'], ['shapes', '--pretty'], ['explain']]) {
    const result = run(args)
    assert.equal(result.status, 2, JSON.stringify(args))
    for (const stream of [result.stdout, result.stderr]) {
      assert.doesNotMatch(stream, /satisfied|insufficient/, JSON.stringify(args))
    }
  }
})
