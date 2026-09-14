import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  DEFAULT_REVIEW_MAX_ROUNDS,
  DEFAULT_REVIEW_MAX_DISPATCHED_ROUNDS,
  resolveMaxRounds,
  resolveMaxDispatchedRounds,
  reviewMaxRounds,
  reviewMaxDispatchedRounds,
  CLEAN_PREFIX,
  CAPPED_PREFIX,
  cleanSentinel,
  cappedSentinel,
  coverageCappedSentinel,
  matchSentinel,
  reviewVerdict,
  reviewAgreement,
  reviewConfidence,
  vanishedFindings,
  classifyOscillation,
  classifySentinels,
  DEFAULT_FIX_ROUND_SECONDS,
  FUNDING_STARVED,
  FUNDING_UNPRICED,
  FUNDING_REASONS,
  stepAllowanceSeconds,
  fundedFixRounds,
  priceAllowance,
  fundingDisclosure,
  sentinelPayload,
  isFundingReason,
  MUSTFIX_OVERRUN_ROUNDS,
  MUSTFIX_OVERRUN_SECONDS,
  ADMIT_FIX_ROUND_REASONS,
  admitFixRound,
  ADMIT_DISPATCHED_ROUND_REASONS,
  admitDispatchedRound,
  ADMIT_CONFIRMING_ROUND_REASONS,
  admitConfirmingRound,
} from './bs-review-caps.mjs'
import { isProvisional } from './bs-run-sentinel.mjs'

// Pin this suite's gate-outcome destination. Why, and the test enforcing it: gate-outcome.test.mjs.
process.env.BOSS_GATE_OUTCOME_FILE = join(
  mkdtempSync(join(tmpdir(), 'gate-outcome-suite-')),
  'outcomes.tsv',
)
const scriptPath = fileURLToPath(new URL('./bs-review-caps.mjs', import.meta.url))

/** Run the CLI block as a subprocess with an overlaid env; return trimmed stdout + status. */
function runCli(args = [], env = {}) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], {
    encoding: 'utf8',
    env: { ...process.env, ...env },
  })
  return { stdout: res.stdout.trim(), stderr: res.stderr.trim(), status: res.status }
}

const cleanLedger = {
  discovered: 2,
  completed: 2,
  skipped: 0,
  timedOut: 0,
  notReached: 0,
}

// ---------------------------------------------------------------------------
// Clamp: env-configurable but LOWER-ONLY (never raises the cap).
// ---------------------------------------------------------------------------

test('the hard default cap is 3', () => {
  assert.equal(DEFAULT_REVIEW_MAX_ROUNDS, 3)
})

test('resolveMaxRounds honors valid in-range values (lowers the cap)', () => {
  assert.equal(resolveMaxRounds('1'), 1)
  assert.equal(resolveMaxRounds('2'), 2)
  assert.equal(resolveMaxRounds('3'), 3)
})

test('resolveMaxRounds never raises: too-high values fall back to the default', () => {
  assert.equal(resolveMaxRounds('4'), 3)
  assert.equal(resolveMaxRounds('5'), 3)
  assert.equal(resolveMaxRounds('300'), 3)
})

test('resolveMaxRounds falls back to the default for values < 1', () => {
  assert.equal(resolveMaxRounds('0'), 3)
  assert.equal(resolveMaxRounds('-1'), 3)
  assert.equal(resolveMaxRounds('-5'), 3)
})

test('resolveMaxRounds falls back to the default for non-integers', () => {
  assert.equal(resolveMaxRounds('2.5'), 3)
  assert.equal(resolveMaxRounds('2abc'), 3)
  assert.equal(resolveMaxRounds('abc'), 3)
  assert.equal(resolveMaxRounds('0x2'), 3)
  assert.equal(resolveMaxRounds('1e0'), 3)
  assert.equal(resolveMaxRounds('+2'), 3)
})

test('resolveMaxRounds falls back to the default for absent/empty input', () => {
  assert.equal(resolveMaxRounds(undefined), 3)
  assert.equal(resolveMaxRounds(null), 3)
  assert.equal(resolveMaxRounds(''), 3)
  assert.equal(resolveMaxRounds('   '), 3)
})

test('resolveMaxRounds trims surrounding whitespace on an otherwise-valid int', () => {
  assert.equal(resolveMaxRounds(' 2 '), 2)
})

test('resolveMaxRounds clamps lower-only against a custom default', () => {
  assert.equal(resolveMaxRounds('3', 5), 3)
  assert.equal(resolveMaxRounds('5', 5), 5)
  assert.equal(resolveMaxRounds('6', 5), 5)
  assert.equal(resolveMaxRounds('0', 5), 5)
  assert.equal(resolveMaxRounds(undefined, 5), 5)
})

test('reviewMaxRounds reads BS_REVIEW_MAX_ROUNDS from the env (lower-only)', () => {
  assert.equal(reviewMaxRounds({ BS_REVIEW_MAX_ROUNDS: '2' }), 2)
  assert.equal(reviewMaxRounds({ BS_REVIEW_MAX_ROUNDS: '9' }), 3)
  assert.equal(reviewMaxRounds({ BS_REVIEW_MAX_ROUNDS: 'nope' }), 3)
  assert.equal(reviewMaxRounds({}), 3)
})

test('resolveMaxDispatchedRounds honors lower values and clamps invalid or higher values', () => {
  assert.equal(DEFAULT_REVIEW_MAX_DISPATCHED_ROUNDS, 6)
  assert.equal(resolveMaxDispatchedRounds('1'), 1)
  assert.equal(resolveMaxDispatchedRounds('4'), 4)
  assert.equal(resolveMaxDispatchedRounds('6'), 6)
  assert.equal(resolveMaxDispatchedRounds('7'), 6)
  assert.equal(resolveMaxDispatchedRounds(undefined), 6)
  assert.equal(resolveMaxDispatchedRounds(null), 6)
  assert.equal(resolveMaxDispatchedRounds('-1'), 6)
  assert.equal(resolveMaxDispatchedRounds('3.5'), 6)
  assert.equal(resolveMaxDispatchedRounds('3'), 3)
  assert.equal(resolveMaxDispatchedRounds(3), 3)
})

test('reviewMaxDispatchedRounds reads BS_REVIEW_MAX_DISPATCHED_ROUNDS from the env', () => {
  assert.equal(reviewMaxDispatchedRounds({ BS_REVIEW_MAX_DISPATCHED_ROUNDS: '3' }), 3)
  assert.equal(reviewMaxDispatchedRounds({ BS_REVIEW_MAX_DISPATCHED_ROUNDS: '9' }), 6)
  assert.equal(reviewMaxDispatchedRounds({ BS_REVIEW_MAX_DISPATCHED_ROUNDS: 'nope' }), 6)
  assert.equal(reviewMaxDispatchedRounds({}), 6)
})

// ---------------------------------------------------------------------------
// Agreement and confidence — derived from collected panel evidence.
// ---------------------------------------------------------------------------

test('reviewAgreement reports panel size, shrink, uncorroborated must-fix, and vanished findings', () => {
  const evidence = {
    panel: {
      initial: ['claude', 'codex', 'golang'],
      reviewers: ['claude', 'codex'],
    },
    mustfix: {
      unresolved: 0,
      items: [
        { title: 'one voice', reviewerCount: 1 },
        { title: 'two voices', reviewerCount: 2 },
      ],
    },
    history: {
      rounds: [
        { round: 1, mustFix: [{ file: 'a.go', line: 1, title: 'vanishes' }] },
        { round: 2, mustFix: [] },
      ],
      fixed: [],
      leaveAsIs: [],
    },
  }
  assert.deepEqual(reviewAgreement(evidence), {
    ok: true,
    panelSize: 2,
    initialPanelSize: 3,
    terminalPanel: ['claude', 'codex'],
    initialPanel: ['claude', 'codex', 'golang'],
    panelShrank: true,
    uncorroboratedMustFixCount: 1,
    vanishedFindings: [{ file: 'a.go', line: 1, title: 'vanishes' }],
  })
})

test('reviewConfidence returns Low with distinct reasons for weak or unreadable evidence', () => {
  const base = {
    panel: { initial: ['a', 'b'], reviewers: ['a', 'b'] },
    mustfix: { unresolved: 0, items: [] },
    invalid: [],
    ledger: cleanLedger,
    capped: false,
    history: { rounds: [], fixed: [], leaveAsIs: [] },
  }
  assert.deepEqual(reviewConfidence({ ...base, panel: { reviewers: ['a'] } }), {
    grade: 'Low',
    reasons: ['single-sample-panel'],
  })
  assert.deepEqual(reviewConfidence({ ...base, capped: true }), {
    grade: 'Low',
    reasons: ['round-cap-hit'],
  })
  assert.deepEqual(reviewConfidence({ ...base, mustfix: { unresolved: 1, items: [] } }), {
    grade: 'Low',
    reasons: ['unresolved-mustfix'],
  })
  assert.deepEqual(
    reviewConfidence({
      ...base,
      history: {
        rounds: [
          { round: 1, mustFix: [{ file: 'a.go', line: 1, title: 'vanishes' }] },
          { round: 2, mustFix: [] },
        ],
        fixed: [],
        leaveAsIs: [],
      },
    }),
    { grade: 'Low', reasons: ['vanished-finding'] },
  )
  assert.deepEqual(reviewConfidence({ ...base, panel: null }), {
    grade: 'Low',
    reasons: ['unreadable-panel-evidence'],
  })
})

test('reviewConfidence honors supplied agreement evidence', () => {
  assert.deepEqual(
    reviewConfidence({
      panel: { initial: ['a', 'b'], reviewers: ['a', 'b'] },
      agreement: {
        panelSize: 2,
        initialPanelSize: 2,
        terminalPanel: ['a', 'b'],
        initialPanel: ['a', 'b'],
        panelShrank: false,
        uncorroboratedMustFixCount: 0,
        vanishedFindings: [{ file: 'a.go', line: 1, title: 'vanished' }],
      },
      mustfix: { unresolved: 0, items: [] },
      invalid: [],
      ledger: cleanLedger,
    }),
    { grade: 'Low', reasons: ['vanished-finding'] },
  )
})

test('reviewConfidence returns High only when every confidence input is strong', () => {
  const strong = {
    panel: { initial: ['a', 'b'], reviewers: ['a', 'b'] },
    mustfix: { unresolved: 0, items: [] },
    invalid: [],
    ledger: cleanLedger,
    capped: false,
    history: { rounds: [], fixed: [], leaveAsIs: [] },
  }
  assert.deepEqual(reviewConfidence(strong), { grade: 'High', reasons: [] })
  assert.equal(
    reviewConfidence({ ...strong, panel: { initial: ['a', 'b', 'c'], reviewers: ['a', 'b'] } })
      .grade,
    'Medium',
  )
  assert.equal(reviewConfidence({ ...strong, capped: true }).grade, 'Low')
  assert.equal(reviewConfidence({ ...strong, mustfix: { unresolved: 1, items: [] } }).grade, 'Low')
  assert.equal(reviewConfidence({ ...strong, panel: { reviewers: ['a'] } }).grade, 'Low')
})

test('reviewConfidence grades not-reached and timed-out reviewers Low without treating skipped as a shortfall', () => {
  const strong = {
    panel: { initial: ['a', 'b'], reviewers: ['a', 'b'] },
    mustfix: { unresolved: 0, items: [] },
    invalid: [],
    capped: false,
    history: { rounds: [], fixed: [], leaveAsIs: [] },
  }
  assert.deepEqual(
    reviewConfidence({
      ...strong,
      ledger: { discovered: 3, completed: 2, skipped: 1, timedOut: 0, notReached: 0 },
    }),
    { grade: 'High', reasons: [] },
  )
  assert.deepEqual(
    reviewConfidence({
      ...strong,
      ledger: { discovered: 3, completed: 2, skipped: 0, timedOut: 0, notReached: 1 },
    }),
    { grade: 'Low', reasons: ['not-reached-reviewer'] },
  )
  assert.deepEqual(
    reviewConfidence({
      ...strong,
      ledger: { discovered: 3, completed: 2, skipped: 0, timedOut: 1, notReached: 0 },
    }),
    { grade: 'Low', reasons: ['timed-out-reviewer'] },
  )
})

test('vanishedFindings distinguishes disappeared, fixed, left-as-is, and repeated findings', () => {
  const disappeared = { file: 'a.go', line: 1, title: 'gone' }
  const fixed = { file: 'b.go', line: 2, title: 'fixed' }
  const left = { file: 'c.go', line: 3, title: 'left' }
  const repeated = { file: 'd.go', line: 4, title: 'still there' }
  assert.deepEqual(
    vanishedFindings({
      rounds: [
        { round: 1, mustFix: [disappeared, fixed, left, repeated] },
        { round: 2, mustFix: [repeated] },
      ],
      fixed: [fixed],
      leaveAsIs: [left],
    }),
    { ok: true, findings: [disappeared] },
  )
})

test('malformed vanished-finding history makes confidence Low instead of silently High', () => {
  assert.deepEqual(vanishedFindings({ rounds: 'not an array' }), {
    ok: false,
    reason: 'history rounds must be an array',
    findings: [],
  })
  assert.deepEqual(
    reviewConfidence({
      panel: { reviewers: ['a', 'b'] },
      mustfix: { unresolved: 0, items: [] },
      invalid: [],
      ledger: cleanLedger,
      history: { rounds: 'not an array' },
    }),
    { grade: 'Low', reasons: ['unreadable-vanished-history'] },
  )
})

// ---------------------------------------------------------------------------
// Oscillation — deterministic guard over consecutive must-fix batches.
// ---------------------------------------------------------------------------

const finding = (file, line, title) => ({ severity: 'Warning', file, line, title })

test('classifyOscillation names a repeated must-fix with no fixed or verified disposition', () => {
  const repeated = finding('a.go', 12, 'check the branch')
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [repeated, finding('b.go', 3, 'gone')] },
      currentRound: { mustFix: [repeated, finding('c.go', 4, 'new')] },
      dispositions: {},
    }),
    { oscillating: ['["a.go",12,"check the branch"]'], reasons: [] },
  )
})

test('classifyOscillation reports fixed or verified findings that repeat as oscillating', () => {
  const fixed = finding('a.go', 1, 'fixed finding')
  const verified = finding('b.go', 2, 'verified finding')
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [fixed, verified] },
      currentRound: { mustFix: [fixed, verified] },
      dispositions: { fixed: [fixed], verified: [verified] },
    }),
    {
      oscillating: ['["a.go",1,"fixed finding"]', '["b.go",2,"verified finding"]'],
      reasons: [],
    },
  )
})

test('classifyOscillation detects one flip-flopping pair inside a batch of at least ten must-fix items', () => {
  const flip = finding('review.md', 42, 'pooled pass lost')
  const previous = [
    flip,
    ...Array.from({ length: 9 }, (_, index) => finding(`old-${index}.go`, index + 1, 'fixed once')),
  ]
  const current = [
    flip,
    ...Array.from({ length: 9 }, (_, index) => finding(`new-${index}.go`, index + 1, 'fresh item')),
  ]
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: previous },
      currentRound: { mustFix: current },
      dispositions: { fixed: previous.slice(1) },
    }),
    { oscillating: ['["review.md",42,"pooled pass lost"]'], reasons: [] },
  )
})

test('classifyOscillation returns the same pair verdict when twenty unrelated members are added', () => {
  const flip = finding('review.md', 42, 'pooled pass lost')
  const unrelated = Array.from({ length: 20 }, (_, index) =>
    finding(`unrelated-${index}.go`, index + 1, 'noise'),
  )
  const baseInput = {
    previousRound: { mustFix: [flip] },
    currentRound: { mustFix: [flip] },
    dispositions: {},
  }
  const expandedInput = {
    previousRound: { mustFix: [flip, ...unrelated] },
    currentRound: {
      mustFix: [flip, ...unrelated.map((item) => ({ ...item, title: 'new noise' }))],
    },
    dispositions: {},
  }
  assert.deepEqual(classifyOscillation(expandedInput), classifyOscillation(baseInput))
})

test('classifyOscillation preserves exact file and title identity', () => {
  const previous = finding('a.go', 1, 'same title')
  const whitespaceChanged = finding(' a.go', 1, 'same title')
  const caseChanged = finding('a.go', 1, 'Same title')

  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [previous] },
      currentRound: { mustFix: [whitespaceChanged, caseChanged] },
      dispositions: {},
    }),
    { oscillating: [], reasons: [] },
  )
})

test('classifyOscillation compares and reports encoded tuple identity without delimiter collisions', () => {
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [finding('a', 1, 'b:2 - c'), finding('a:1 - b', 2, 'c')] },
      currentRound: { mustFix: [finding('a', 1, 'b:2 - c'), finding('a:1 - b', 2, 'c')] },
      dispositions: {},
    }),
    {
      oscillating: ['["a",1,"b:2 - c"]', '["a:1 - b",2,"c"]'],
      reasons: [],
    },
  )
})

test('classifyOscillation returns a well-formed result for degenerate inputs', () => {
  assert.deepEqual(classifyOscillation(), { oscillating: [], reasons: ['rounds must be objects'] })
  assert.deepEqual(classifyOscillation({ previousRound: {}, currentRound: { mustFix: [] } }), {
    oscillating: [],
    reasons: ['rounds must be objects'],
  })
  assert.deepEqual(
    classifyOscillation({ previousRound: { mustFix: [] }, currentRound: { mustFix: [] } }),
    { oscillating: [], reasons: [] },
  )
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [{ title: 'missing file' }] },
      currentRound: { mustFix: [{ title: 'missing file' }] },
      dispositions: { fixed: [finding('unknown.go', 1, 'unknown')] },
    }),
    { oscillating: [], reasons: ['malformed finding'] },
  )
})

test('classifyOscillation reports malformed disposition evidence', () => {
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      currentRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      dispositions: { fixed: { file: 'a.go', line: 1, title: 'stuck' } },
    }),
    {
      oscillating: ['["a.go",1,"stuck"]'],
      reasons: ['fixed dispositions must be an array'],
    },
  )
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      currentRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      dispositions: { verified: [{ title: 'missing file' }] },
    }),
    {
      oscillating: ['["a.go",1,"stuck"]'],
      reasons: ['malformed verified disposition'],
    },
  )
})

test('classifyOscillation reports invalid top-level disposition evidence', () => {
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      currentRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      dispositions: 'not-an-object',
    }),
    {
      oscillating: ['["a.go",1,"stuck"]'],
      reasons: ['dispositions must be an object or array'],
    },
  )
})

test('classifyOscillation validates every array disposition row before filtering', () => {
  assert.deepEqual(
    classifyOscillation({
      previousRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      currentRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      dispositions: [{ status: 7 }, { status: 'fixed', ...finding('a.go', 1, 'stuck') }],
    }),
    {
      oscillating: ['["a.go",1,"stuck"]'],
      reasons: ['malformed disposition'],
    },
  )
})

// ---------------------------------------------------------------------------
// Sentinel prefixes — byte-identical pins (the downstream-matcher contract).
// ---------------------------------------------------------------------------

test('the sentinel prefixes are byte-identical to the published contract', () => {
  assert.equal(CLEAN_PREFIX, 'bs-review clean:')
  assert.equal(CAPPED_PREFIX, 'bs-review capped:')
})

test('cleanSentinel is byte-identical', () => {
  assert.equal(cleanSentinel(), 'bs-review clean: no open must-fix findings.')
  assert.ok(cleanSentinel().startsWith(CLEAN_PREFIX))
})

test('cappedSentinel keeps a fixed prefix with only the round-count tail dynamic', () => {
  assert.equal(
    cappedSentinel(3),
    'bs-review capped: unresolved must-fix findings or invalid evidence remain after 3 rounds.',
  )
  assert.equal(
    cappedSentinel(2),
    'bs-review capped: unresolved must-fix findings or invalid evidence remain after 2 rounds.',
  )
  assert.ok(cappedSentinel(2).startsWith(CAPPED_PREFIX))
  // Two capped sentinels differ ONLY in the round-count tail.
  assert.equal(
    cappedSentinel(2).replace('2 rounds', 'N rounds'),
    cappedSentinel(3).replace('3 rounds', 'N rounds'),
  )
})

test('funding helpers price initial legs and funded fix rounds at the boundary', () => {
  assert.equal(FUNDING_STARVED, 'funding-starved')
  assert.equal(stepAllowanceSeconds({ legSeconds: 300, legs: 3 }), 900)
  assert.equal(
    fundedFixRounds({
      allowanceSeconds: 900,
      legSeconds: 300,
      initialLegs: 3,
      fixRoundSeconds: 1200,
    }),
    0,
  )
  assert.equal(
    fundedFixRounds({
      allowanceSeconds: 2099,
      legSeconds: 300,
      initialLegs: 3,
      fixRoundSeconds: 1200,
    }),
    0,
  )
  assert.equal(
    fundedFixRounds({
      allowanceSeconds: 2100,
      legSeconds: 300,
      initialLegs: 3,
      fixRoundSeconds: 1200,
    }),
    1,
  )
})

test('priceAllowance refuses to price a malformed allowance instead of fabricating one', () => {
  // Each shape below used to yield a confident priced verdict that a caller could not
  // tell from a real one: `{}` reported a starved step priced from nothing, a zero
  // `legSeconds` funded a round the caller could not afford, and a negative allowance
  // was floored to 0 and reported as starved.
  for (const bad of [
    {},
    { legSeconds: 300, initialLegs: 3 },
    { allowanceSeconds: 900, initialLegs: 3 },
    { allowanceSeconds: 900, legSeconds: 300 },
    { allowanceSeconds: 2100, legSeconds: 0, initialLegs: 3 },
    { allowanceSeconds: -900, legSeconds: 300, initialLegs: 3 },
    { allowanceSeconds: 900, legSeconds: 300, initialLegs: 0 },
    { allowanceSeconds: 900, legSeconds: 300, initialLegs: 1.5 },
    { allowanceSeconds: '900', legSeconds: 300, initialLegs: 3 },
    { allowanceSeconds: Number.NaN, legSeconds: 300, initialLegs: 3 },
    { allowanceSeconds: Number.POSITIVE_INFINITY, legSeconds: 300, initialLegs: 3 },
    { allowanceSeconds: 900, legSeconds: 300, initialLegs: 3, fixRoundSeconds: 0 },
    { allowanceSeconds: 900, legSeconds: 300, initialLegs: 3, fixRoundSeconds: -1 },
  ]) {
    assert.throws(() => priceAllowance(bad), TypeError, `should refuse ${JSON.stringify(bad)}`)
    assert.throws(() => fundingDisclosure(bad), TypeError)
  }
  // `fixRoundSeconds` alone stays optional — omitting it is how both shipped tiers call
  // the verb, and the value actually used is echoed back.
  assert.deepEqual(priceAllowance({ allowanceSeconds: 900, legSeconds: 300, initialLegs: 3 }), {
    fundedFixRounds: 0,
    allowanceSeconds: 900,
    fixRoundSeconds: DEFAULT_FIX_ROUND_SECONDS,
  })
})

test('fundingDisclosure narrates the same arithmetic it prices — one normalisation, not two', () => {
  // The count and the two echoed numbers must come from ONE pricing call. Fractional
  // terms are where two independent normalisations diverge: the allowance floors, the
  // leg and the price ceil, and a disclosure that re-derived them separately could
  // report an allowance that does not produce its own count.
  const input = {
    allowanceSeconds: 2100.9,
    legSeconds: 299.1,
    initialLegs: 3,
    fixRoundSeconds: 1199.2,
  }
  const priced = priceAllowance(input)
  const disclosure = fundingDisclosure(input)
  assert.equal(disclosure.fundedFixRounds, priced.fundedFixRounds)
  assert.equal(disclosure.allowanceSeconds, priced.allowanceSeconds)
  assert.equal(disclosure.fixRoundSeconds, priced.fixRoundSeconds)
  // The echoed numbers reproduce the count they explain, which is the whole promise.
  assert.equal(
    Math.max(
      0,
      Math.floor(
        (disclosure.allowanceSeconds - Math.ceil(input.legSeconds) * input.initialLegs) /
          disclosure.fixRoundSeconds,
      ),
    ),
    disclosure.fundedFixRounds,
  )
})

test('sentinelPayload builds the run-file payload over a CLOSED reason set', () => {
  assert.deepEqual(FUNDING_REASONS, [FUNDING_STARVED, FUNDING_UNPRICED])
  assert.equal(FUNDING_UNPRICED, 'funding-unpriced')
  // A funded step names no reason at all — the key is ABSENT, never empty-valued,
  // because `boss-review` reads absence as the assertion "this step was not starved".
  for (const none of [undefined, '', null]) {
    assert.deepEqual(sentinelPayload(none), { provisional: false })
    assert.equal(JSON.stringify(sentinelPayload(none)), '{"provisional":false}')
  }
  assert.equal(
    JSON.stringify(sentinelPayload(FUNDING_STARVED)),
    '{"provisional":false,"funding":{"reason":"funding-starved"}}',
  )
  // A pricing call that FAILED is distinguishable from both — never silently equal to
  // "not starved", which is what an unchecked exit status used to produce.
  assert.equal(
    JSON.stringify(sentinelPayload(FUNDING_UNPRICED)),
    '{"provisional":false,"funding":{"reason":"funding-unpriced"}}',
  )
  // TOTAL, not throwing. The set stays closed for CONSUMERS — an unrecognised value is
  // disclosed as unpriced — but the builder cannot refuse, because at every call site it
  // is an unchecked command substitution whose failure empties the payload argument and
  // costs the write its `provisional:false` marker as well as its reason.
  for (const bad of ['starved', 'funding-starved ', 'unknown', 0, {}]) {
    assert.equal(
      JSON.stringify(sentinelPayload(bad)),
      '{"provisional":false,"funding":{"reason":"funding-unpriced"}}',
      `should disclose ${JSON.stringify(bad)} as unpriced, not throw`,
    )
    assert.equal(isFundingReason(bad), false)
  }
})

// ---------------------------------------------------------------------------
// Matcher — recognizes the prefixes callers route on.
// ---------------------------------------------------------------------------

test('matchSentinel recognizes the clean sentinel', () => {
  assert.deepEqual(matchSentinel('bs-review clean: no open must-fix findings.'), {
    status: 'clean',
  })
})

test('matchSentinel recognizes the empty-diff clean variant', () => {
  assert.deepEqual(matchSentinel('bs-review clean: no changes to review.'), { status: 'clean' })
})

test('matchSentinel recognizes the capped sentinel and extracts the round count', () => {
  assert.deepEqual(
    matchSentinel(
      'bs-review capped: unresolved must-fix findings or invalid evidence remain after 3 rounds.',
    ),
    {
      status: 'capped',
      rounds: 3,
    },
  )
})

test('matchSentinel rejects malformed capped sentinels', () => {
  assert.equal(
    matchSentinel(
      'bs-review capped: unresolved must-fix findings or invalid evidence remain after 0 rounds.',
    ),
    null,
  )
  assert.equal(
    matchSentinel(
      'bs-review capped: unresolved must-fix findings or invalid evidence remain after abc rounds.',
    ),
    null,
  )
  assert.equal(matchSentinel('bs-review capped: open must-fix findings remain.'), null)
})

test('matchSentinel tolerates surrounding whitespace', () => {
  assert.deepEqual(matchSentinel('  bs-review clean: no open must-fix findings.  '), {
    status: 'clean',
  })
})

test('matchSentinel returns null for a non-sentinel line', () => {
  assert.equal(matchSentinel('some other output'), null)
  assert.equal(matchSentinel(''), null)
  assert.equal(matchSentinel(undefined), null)
})

test('matchSentinel round-trips the builders', () => {
  assert.deepEqual(matchSentinel(cleanSentinel()), { status: 'clean' })
  assert.deepEqual(matchSentinel(cappedSentinel(2)), { status: 'capped', rounds: 2 })
  assert.deepEqual(matchSentinel(coverageCappedSentinel(2)), { status: 'capped', rounds: 2 })
})

// ---------------------------------------------------------------------------
// Derived verdict — clean only when evidence proves no unresolved blockers.
// ---------------------------------------------------------------------------

test('reviewVerdict returns clean only when unresolved must-fix, invalid and ledger evidence are clean', () => {
  assert.deepEqual(
    reviewVerdict({ mustfix: { unresolved: 0 }, invalid: [], ledger: cleanLedger }),
    {
      status: 'clean',
      reasons: [],
    },
  )
  assert.deepEqual(
    reviewVerdict({ mustfix: { unresolved: 1 }, invalid: [], ledger: cleanLedger }),
    {
      status: 'capped',
      reasons: ['unresolved-mustfix'],
    },
  )
  assert.deepEqual(
    reviewVerdict({
      mustfix: { unresolved: 0 },
      invalid: [{ reason: 'bad' }],
      ledger: cleanLedger,
    }),
    {
      status: 'capped',
      reasons: ['invalid-evidence'],
    },
  )
})

test('reviewVerdict reports every blocker reason that prevents a clean verdict', () => {
  assert.deepEqual(
    reviewVerdict({
      mustfix: { unresolved: 2 },
      invalid: [{ reason: 'bad' }],
      ledger: cleanLedger,
    }),
    {
      status: 'capped',
      reasons: ['unresolved-mustfix', 'invalid-evidence'],
    },
  )
})

test('reviewVerdict caps for unreadable or zero-completed ledger coverage', () => {
  assert.deepEqual(reviewVerdict({ mustfix: { unresolved: 0 }, invalid: [] }), {
    status: 'capped',
    reasons: ['unreadable-ledger'],
  })
  assert.deepEqual(
    reviewVerdict({
      mustfix: { unresolved: 0 },
      invalid: [],
      ledger: { discovered: 2, completed: 0, skipped: 0, timedOut: 0, notReached: 2 },
    }),
    { status: 'capped', reasons: ['no-coverage'] },
  )
  assert.deepEqual(
    reviewVerdict({
      mustfix: { unresolved: 0 },
      invalid: [],
      ledger: { discovered: 2, completed: 2, skipped: 1, timedOut: 0, notReached: 0 },
    }),
    { status: 'capped', reasons: ['unreadable-ledger'] },
  )
})

test('reviewVerdict fails closed on unreadable evidence', () => {
  for (const evidence of [
    undefined,
    null,
    [],
    {},
    { mustfix: { unresolved: '0' }, invalid: [], ledger: cleanLedger },
    { mustfix: { unresolved: 0 }, invalid: {}, ledger: cleanLedger },
  ]) {
    assert.deepEqual(
      reviewVerdict(evidence),
      { status: 'capped', reasons: ['unreadable-evidence'] },
      `evidence ${JSON.stringify(evidence)} must not be clean`,
    )
  }
})

// ---------------------------------------------------------------------------
// Whole-text sentinel classification — absence and ambiguity are non-clean.
// ---------------------------------------------------------------------------

test('classifySentinels returns missing for empty text or text with no sentinel', () => {
  assert.deepEqual(classifySentinels(''), { status: 'missing' })
  assert.deepEqual(classifySentinels('report body\nno terminal verdict\n'), { status: 'missing' })
})

test('classifySentinels round-trips exactly one clean or capped sentinel', () => {
  assert.deepEqual(classifySentinels(`body\n${cleanSentinel()}\n`), { status: 'clean' })
  assert.deepEqual(classifySentinels(`body\n${cappedSentinel(2)}\n`), {
    status: 'capped',
    rounds: 2,
  })
})

test('classifySentinels returns ambiguous for disagreeing or combined sentinel lines', () => {
  assert.deepEqual(classifySentinels(`${cleanSentinel()}\n${cappedSentinel(3)}\n`), {
    status: 'ambiguous',
  })
  assert.deepEqual(classifySentinels(`${CLEAN_PREFIX} ${CAPPED_PREFIX} both on one line\n`), {
    status: 'ambiguous',
  })
})

// ---------------------------------------------------------------------------
// CLI — the surface the skill prose invokes.
// ---------------------------------------------------------------------------

test('CLI `rounds` reads the env and clamps lower-only', () => {
  assert.equal(runCli(['rounds'], { BS_REVIEW_MAX_ROUNDS: '2' }).stdout, '2')
  assert.equal(runCli(['rounds'], { BS_REVIEW_MAX_ROUNDS: '9' }).stdout, '3')
  assert.equal(runCli(['rounds'], { BS_REVIEW_MAX_ROUNDS: '' }).stdout, '3')
})

test('CLI `dispatched-rounds` reads the env and clamps lower-only', () => {
  assert.equal(runCli(['dispatched-rounds'], { BS_REVIEW_MAX_DISPATCHED_ROUNDS: '4' }).stdout, '4')
  assert.equal(runCli(['dispatched-rounds'], { BS_REVIEW_MAX_DISPATCHED_ROUNDS: '9' }).stdout, '6')
  assert.equal(runCli(['dispatched-rounds'], { BS_REVIEW_MAX_DISPATCHED_ROUNDS: '' }).stdout, '6')
})

/** Write `report` to a scratch file and return its path. */
function reportFile(report) {
  const path = join(mkdtempSync(join(tmpdir(), 'bs-review-caps-report-')), 'report.json')
  writeFileSync(path, JSON.stringify(report))
  return path
}

test('CLI `sentinel clean --in|capped` prints byte-identical lines', () => {
  const clean = reportFile({ mustfix: { unresolved: 0 }, invalid: [], ledger: cleanLedger })
  assert.equal(
    runCli(['sentinel', 'clean', '--in', clean]).stdout,
    'bs-review clean: no open must-fix findings.',
  )
  assert.equal(
    runCli(['sentinel', 'capped', '3']).stdout,
    'bs-review capped: unresolved must-fix findings or invalid evidence remain after 3 rounds.',
  )
})

// THE FALSIFICATION. A report with zero open must-fix and a non-empty `invalid` list is the exact
// shape that published a false green: `reviewVerdict` has always refused it, but the clean CLI verb
// printed its line without ever consulting the verdict owner. Both halves are asserted — the gate
// fires on the unrepaired report, and it is demonstrably ABLE to pass on an otherwise identical
// report with `invalid` emptied — so a gate that refused everything could not pass this test.
test('a clean sentinel is unobtainable from a report carrying unrepaired invalid evidence', () => {
  const unrepaired = {
    mustfix: { unresolved: 0 },
    invalid: [{ reason: 'malformed' }],
    ledger: cleanLedger,
  }
  const refused = runCli(['sentinel', 'clean', '--in', reportFile(unrepaired)])
  assert.equal(refused.status, 3, 'the refusal must be a distinct non-zero exit')
  assert.equal(refused.stdout, '', 'no clean line may reach stdout')
  assert.match(refused.stderr, /refusing a clean sentinel/)
  assert.match(refused.stderr, /invalid-evidence/, 'the refusal must NAME the blocking reason')

  // The other documented route over the same evidence: `verdict --in` emits capped, never clean.
  const derived = runCli(['verdict', '--in', reportFile(unrepaired)])
  assert.equal(derived.status, 0)
  assert.deepEqual(matchSentinel(derived.stdout), { status: 'capped', rounds: 1 })

  // Able to fire: the same report with `invalid` emptied passes by BOTH routes.
  const repaired = { ...unrepaired, invalid: [] }
  assert.equal(
    runCli(['sentinel', 'clean', '--in', reportFile(repaired)]).stdout,
    cleanSentinel(),
    'the gate must be demonstrably able to pass, or the refusal proves nothing',
  )
  assert.equal(runCli(['verdict', '--in', reportFile(repaired)]).stdout, cleanSentinel())
})

test('CLI `sentinel clean` refuses outright when no report evidence is supplied', () => {
  for (const args of [
    ['sentinel', 'clean'],
    ['sentinel', 'clean', '--in'],
    ['sentinel', 'clean', 'report.json'],
  ]) {
    const res = runCli(args)
    assert.equal(res.status, 2, `${args.join(' ')} must fail closed`)
    assert.equal(res.stdout, '', `${args.join(' ')} must print no sentinel line`)
    assert.match(res.stderr, /sentinel clean requires report evidence/)
  }
  const missing = runCli([
    'sentinel',
    'clean',
    '--in',
    join(tmpdir(), 'bs-caps-absent-report.json'),
  ])
  assert.equal(missing.status, 2)
  assert.equal(missing.stdout, '')
  assert.match(missing.stderr, /unable to read report JSON/)
})

// The evidence-free CAPPED form is deliberately preserved: the pre-dispatch seed and the decline
// routes hold no report, and a capped line can only under-claim, so no false green is reachable.
test('CLI `sentinel capped <N>` still needs no report evidence', () => {
  const res = runCli(['sentinel', 'capped', '2'])
  assert.equal(res.status, 0)
  assert.deepEqual(matchSentinel(res.stdout), { status: 'capped', rounds: 2 })
})

test('CLI `sentinel capped` rejects missing / 0 / non-integer counts', () => {
  for (const args of [
    ['sentinel', 'capped'],
    ['sentinel', 'capped', '0'],
    ['sentinel', 'capped', 'abc'],
    ['sentinel', 'capped', '2.5'],
  ]) {
    const r = runCli(args, { BS_REVIEW_MAX_ROUNDS: '2' })
    assert.notEqual(r.status, 0)
    assert.match(r.stderr, /positive integer round count/)
    assert.equal(r.stdout, '')
  }
})

test('CLI `match` classifies a sentinel line as JSON', () => {
  const r = runCli([
    'match',
    'bs-review capped: unresolved must-fix findings or invalid evidence remain after 2 rounds.',
  ])
  assert.deepEqual(JSON.parse(r.stdout), { status: 'capped', rounds: 2 })
})

test('CLI `funding` prices a per-step allowance against one fix round', () => {
  // The verb exists so a skill body can *compute* the funded-round count it used to
  // narrate. Step 6c's shipped default is three 300s legs under a 900s allowance with
  // each ordinary fix round costing 1200s: nothing is left over, so the step is starved.
  const starved = runCli([
    'funding',
    '{"allowanceSeconds":900,"legSeconds":300,"initialLegs":3,"fixRoundSeconds":1200}',
  ])
  assert.equal(starved.status, 0)
  assert.deepEqual(JSON.parse(starved.stdout), {
    fundedFixRounds: 0,
    reason: FUNDING_STARVED,
    allowanceSeconds: 900,
    fixRoundSeconds: 1200,
  })

  // One fix round becomes affordable exactly at initialLegs * legSeconds + fixRoundSeconds,
  // and a funded step names no reason at all rather than an empty-string one.
  const funded = runCli([
    'funding',
    '{"allowanceSeconds":2100,"legSeconds":300,"initialLegs":3,"fixRoundSeconds":1200}',
  ])
  assert.equal(funded.status, 0)
  assert.deepEqual(JSON.parse(funded.stdout), {
    fundedFixRounds: 1,
    reason: null,
    allowanceSeconds: 2100,
    fixRoundSeconds: 1200,
  })

  // One second short of that boundary is still starved.
  assert.deepEqual(
    JSON.parse(
      runCli([
        'funding',
        '{"allowanceSeconds":2099,"legSeconds":300,"initialLegs":3,"fixRoundSeconds":1200}',
      ]).stdout,
    ),
    { fundedFixRounds: 0, reason: FUNDING_STARVED, allowanceSeconds: 2099, fixRoundSeconds: 1200 },
  )

  // The quick tier's two legs are strictly worse, and it leaves the price defaulted —
  // the echoed `fixRoundSeconds` is the price actually used, not the argument passed.
  assert.deepEqual(
    JSON.parse(
      runCli(['funding', '{"allowanceSeconds":600,"legSeconds":300,"initialLegs":2}']).stdout,
    ),
    {
      fundedFixRounds: 0,
      reason: FUNDING_STARVED,
      allowanceSeconds: 600,
      fixRoundSeconds: DEFAULT_FIX_ROUND_SECONDS,
    },
  )
})

test('CLI `funding` reads no clock and no env', () => {
  // A per-step allowance is passed in, never sampled. Two runs a moment apart under a
  // hostile env must agree byte for byte, or the verb could select an outcome by timing.
  const args = ['funding', '{"allowanceSeconds":900,"legSeconds":300,"initialLegs":3}']
  const a = runCli(args, { BS_REVIEW_MAX_ROUNDS: '9', BS_REVIEW_FIX_ROUND_SECONDS: '1' })
  const b = runCli(args)
  assert.equal(a.status, 0)
  assert.equal(a.stdout, b.stdout)
  assert.equal(JSON.parse(a.stdout).reason, FUNDING_STARVED)
})

test('CLI `funding` rejects malformed input instead of pricing a guess', () => {
  for (const args of [
    ['funding'],
    ['funding', 'not json'],
    ['funding', 'null'],
    ['funding', '[]'],
    ['funding', '3'],
    ['funding', '"900"'],
  ]) {
    const r = runCli(args)
    assert.notEqual(r.status, 0)
    assert.match(r.stderr, /funding requires one JSON object argument/)
    assert.equal(r.stdout, '')
  }

  // A malformed TERM is the dangerous case, because the argument parses: the verb used
  // to print a priced verdict and exit 0, so a caller's `if` read the failure as a
  // funded step. It must exit non-zero and print nothing on stdout.
  for (const json of [
    '{}',
    '{"legSeconds":0,"allowanceSeconds":2100,"initialLegs":3}',
    '{"allowanceSeconds":-900,"legSeconds":300,"initialLegs":3}',
    '{"allowanceSeconds":900,"legSeconds":300,"initialLegs":0}',
    '{"allowanceSeconds":"900","legSeconds":300,"initialLegs":3}',
    '{"allowanceSeconds":900,"legSeconds":300,"initialLegs":3,"fixRoundSeconds":0}',
  ]) {
    const r = runCli(['funding', json])
    assert.notEqual(r.status, 0, `${json} must not price`)
    assert.match(r.stderr, /funding cannot price this allowance:/)
    assert.equal(r.stdout, '')
  }
})

test('CLI `sentinel-payload` builds the payload every terminal write carries', () => {
  // The verb exists so no skill body hand-escapes JSON inside a double-quoted shell
  // string, and so the reason cannot leak into the byte-stable sentinel LINE.
  const none = runCli(['sentinel-payload'])
  assert.equal(none.status, 0)
  assert.equal(none.stdout, '{"provisional":false}')
  // An UNSET caller variable expands to the empty string in the shell, which is the
  // funded case — the same bytes, not an empty-valued reason key.
  assert.equal(runCli(['sentinel-payload', '']).stdout, '{"provisional":false}')
  assert.equal(
    runCli(['sentinel-payload', FUNDING_STARVED]).stdout,
    '{"provisional":false,"funding":{"reason":"funding-starved"}}',
  )
  assert.equal(
    runCli(['sentinel-payload', FUNDING_UNPRICED]).stdout,
    '{"provisional":false,"funding":{"reason":"funding-unpriced"}}',
  )
  // TOTALITY. Every call site is an UNCHECKED command substitution inside a
  // `bs-run-sentinel.mjs write` argument list, so a non-zero exit here does not stop the
  // write — it empties that argument. Measured at the real seam before this was total:
  // the persisted payload was `{}`, losing `provisional:false` along with the reason, so
  // the verdict read back as the caller's own pessimistic seed. An unrecognised reason
  // must therefore still print a VALID payload, disclosed as unpriced (never as funded)
  // and named on stderr.
  for (const bad of ['starved', 'true', 'funding starved']) {
    const r = runCli(['sentinel-payload', bad])
    assert.equal(
      r.status,
      0,
      `${bad} must not exit non-zero: an empty substitution drops the payload`,
    )
    assert.equal(r.stdout, '{"provisional":false,"funding":{"reason":"funding-unpriced"}}')
    assert.match(r.stderr, /unrecognised funding reason/)
  }
  assert.equal(isFundingReason(''), true)
  assert.equal(isFundingReason(FUNDING_STARVED), true)
  assert.equal(isFundingReason('starved'), false)
})

test('`bs-run-sentinel write` lands the verdict when the payload argument collapses', () => {
  // The other half of the same seam, and the direction that matters: the KIND is the
  // verdict, the payload only an optional disclosure. Because the caller SEEDS a
  // pessimistic `capped` + `provisional:true` line before dispatch, a refused terminal
  // write leaves that seed standing verbatim — so refusing the write over a collapsed
  // substitution DEMOTES a clean run to `review coverage unknown`. Measured at the real
  // seam. A degraded disclosure must cost the disclosure, never the verdict.
  const scratch = mkdtempSync(join(tmpdir(), 'bs-run-sentinel-payload-'))
  const sentinelCli = fileURLToPath(new URL('./bs-run-sentinel.mjs', import.meta.url))
  const sentinelPath = join(scratch, 'review.json')
  const write = (kind, ...args) =>
    spawnSync(process.execPath, [sentinelCli, 'write', scratch, 'r1', 'review', kind, ...args], {
      encoding: 'utf8',
    })
  const stored = () => JSON.parse(readFileSync(sentinelPath, 'utf8'))

  // Seed the pessimistic provisional line the orchestrator writes before it dispatches.
  assert.equal(write('seed capped', '{"provisional":true}').status, 0)
  assert.equal(isProvisional({ status: 'ok', ...stored() }), true)

  // The terminal clean write whose payload argument collapsed to empty: it LANDS, the
  // seed is gone, and the run no longer reads as provisional.
  const empty = write('earned clean', '')
  assert.equal(empty.status, 0, empty.stderr)
  assert.match(empty.stderr, /empty payload argument/)
  assert.equal(stored().kind, 'earned clean')
  assert.deepEqual(stored().payload, {})
  assert.equal(isProvisional({ status: 'ok', ...stored() }), false)

  // A PRESENT-but-MALFORMED payload is an unreadable disclosure, not an absent one, and is
  // still refused — garbage must never be stored or reinterpreted as the funded case.
  for (const bad of ['[1]', 'nope', 'null', '"x"']) {
    const r = write('mangled', bad)
    assert.notEqual(r.status, 0, `${bad} must be refused`)
    assert.equal(stored().kind, 'earned clean', `${bad} must not overwrite the stored verdict`)
  }

  assert.equal(write('k', '{"provisional":false}').status, 0)
  assert.deepEqual(stored().payload, { provisional: false })
  // An OMITTED payload remains the no-payload case.
  assert.equal(write('k').status, 0)
  assert.deepEqual(stored().payload, {})
  rmSync(scratch, { recursive: true, force: true })
})

test('the starved reason survives the whole funding → payload → run-file route', () => {
  // The behavioural end-to-end this ticket exists for: a starved step's reason must
  // ARRIVE in the payload a terminal sentinel write persists, and a funded step's
  // payload must carry NO funding key — because the consumer reads absence as the
  // assertion "this step was not starved".
  const scratch = mkdtempSync(join(tmpdir(), 'bs-review-caps-funding-'))
  const sentinelCli = fileURLToPath(new URL('./bs-run-sentinel.mjs', import.meta.url))
  const runSentinel = (args) =>
    spawnSync(process.execPath, [sentinelCli, ...args], { encoding: 'utf8' })
  const route = (allowanceSeconds, initialLegs, runId) => {
    const dir = join(scratch, runId)
    mkdirSync(dir, { recursive: true })
    // 1. price the step, exactly as the skill body does
    const funding = runCli([
      'funding',
      JSON.stringify({ allowanceSeconds, legSeconds: 300, initialLegs }),
    ])
    assert.equal(funding.status, 0)
    const reason = JSON.parse(funding.stdout).reason ?? ''
    // 2. build the payload from the reason it printed
    const payload = runCli(['sentinel-payload', reason])
    assert.equal(payload.status, 0)
    // 3. generate the byte-stable line, and 4. persist both into the run file
    const line = runCli(['sentinel', 'capped', '2'])
    assert.equal(line.status, 0)
    const write = runSentinel(['write', dir, runId, 'review', line.stdout, payload.stdout])
    assert.equal(write.status, 0, write.stderr)
    // 5. read it back the way the orchestrator does
    const read = runSentinel(['read', dir, runId, 'review'])
    assert.equal(read.status, 0, read.stderr)
    return { reason, line: line.stdout, file: JSON.parse(read.stdout) }
  }

  // Step 6's shipped terms: three 300s legs under a 900s allowance — starved.
  const starved = route(900, 3, 'starved-run')
  assert.equal(starved.reason, FUNDING_STARVED)
  assert.equal(starved.file.status, 'ok')
  assert.equal(starved.file.payload.funding.reason, FUNDING_STARVED)
  assert.equal(starved.file.payload.provisional, false)
  // The reason travelled in the PAYLOAD only: the line is the helper's bytes, and
  // `matchSentinel` still classifies it.
  assert.equal(starved.file.kind, starved.line)
  assert.doesNotMatch(starved.file.kind, /funding/)
  assert.deepEqual(matchSentinel(starved.file.kind), { status: 'capped', rounds: 2 })

  // A funded step: no `funding` key at all, so absence keeps meaning "not starved".
  const funded = route(2100, 3, 'funded-run')
  assert.equal(funded.reason, '')
  assert.equal(funded.file.status, 'ok')
  assert.ok(!Object.hasOwn(funded.file.payload, 'funding'))
  assert.equal(funded.file.payload.provisional, false)

  rmSync(scratch, { recursive: true, force: true })
})

test('CLI `verdict --in` prints the sentinel implied by report evidence', () => {
  const scratch = mkdtempSync(join(tmpdir(), 'bs-review-caps-'))
  try {
    const cleanPath = join(scratch, 'clean.json')
    const contradictedPath = join(scratch, 'contradicted.json')
    writeFileSync(
      cleanPath,
      JSON.stringify({ rounds: 2, mustfix: { unresolved: 0 }, invalid: [], ledger: cleanLedger }),
    )
    writeFileSync(
      contradictedPath,
      JSON.stringify({
        status: 'clean',
        rounds: 3,
        mustfix: { unresolved: 0 },
        invalid: [{}],
        ledger: cleanLedger,
      }),
    )
    assert.equal(runCli(['verdict', '--in', cleanPath]).stdout, cleanSentinel())
    assert.equal(runCli(['verdict', '--in', contradictedPath]).stdout, cappedSentinel(3))
    const noCoveragePath = join(scratch, 'no-coverage.json')
    writeFileSync(
      noCoveragePath,
      JSON.stringify({
        rounds: 2,
        mustfix: { unresolved: 0 },
        invalid: [],
        ledger: { discovered: 2, completed: 0, skipped: 0, timedOut: 0, notReached: 2 },
      }),
    )
    assert.equal(runCli(['verdict', '--in', noCoveragePath]).stdout, coverageCappedSentinel(2))
  } finally {
    rmSync(scratch, { recursive: true, force: true })
  }
})

test('CLI `confidence --in` prints the derived confidence grade and reasons', () => {
  const scratch = mkdtempSync(join(tmpdir(), 'bs-review-caps-'))
  try {
    const reportPath = join(scratch, 'report.json')
    writeFileSync(
      reportPath,
      JSON.stringify({
        panel: { initial: ['one'], reviewers: ['one'] },
        mustfix: { unresolved: 0, items: [] },
        invalid: [],
        ledger: cleanLedger,
      }),
    )
    const result = runCli(['confidence', '--in', reportPath])
    assert.equal(result.status, 0, result.stderr)
    assert.deepEqual(JSON.parse(result.stdout), {
      grade: 'Low',
      reasons: ['single-sample-panel'],
    })

    const unreadable = runCli(['confidence', '--in', join(scratch, 'missing.json')])
    assert.notEqual(unreadable.status, 0)
    assert.match(unreadable.stderr, /unable to read/i)
  } finally {
    rmSync(scratch, { recursive: true, force: true })
  }
})

test('CLI `classify --in` prints whole-text sentinel classification and rejects unreadable files', () => {
  const scratch = mkdtempSync(join(tmpdir(), 'bs-review-caps-'))
  try {
    const reportPath = join(scratch, 'report.txt')
    writeFileSync(reportPath, `body\n${cappedSentinel(1)}\n`)
    const classified = runCli(['classify', '--in', reportPath])
    assert.equal(classified.status, 0)
    assert.deepEqual(JSON.parse(classified.stdout), { status: 'capped', rounds: 1 })

    const missing = runCli(['classify', '--in', join(scratch, 'missing.txt')])
    assert.notEqual(missing.status, 0)
    assert.match(missing.stderr, /unable to read/i)
  } finally {
    rmSync(scratch, { recursive: true, force: true })
  }
})

test('CLI `oscillation` round-trips the deterministic guard and rejects malformed input', () => {
  const repeated = finding('a.go', 12, 'check the branch')
  const input = {
    previousRound: { mustFix: [repeated] },
    currentRound: { mustFix: [repeated] },
    dispositions: {},
  }
  const r = runCli(['oscillation', JSON.stringify(input)])
  assert.equal(r.status, 0)
  assert.deepEqual(JSON.parse(r.stdout), classifyOscillation(input))

  const malformed = runCli(['oscillation', 'not-json'])
  assert.notEqual(malformed.status, 0)
  assert.match(malformed.stderr, /oscillation requires one JSON object argument/)
  assert.equal(malformed.stdout, '')
})

test('CLI `oscillation --in` reads the guard payload from a file', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-oscillation-'))
  try {
    const file = join(dir, 'oscillation.json')
    const repeated = finding('a.go', 1, 'stuck')
    const input = {
      previousRound: { mustFix: [repeated] },
      currentRound: { mustFix: [repeated] },
      dispositions: {},
    }
    writeFileSync(file, JSON.stringify(input))
    const r = runCli(['oscillation', '--in', file])
    assert.equal(r.status, 0)
    assert.deepEqual(JSON.parse(r.stdout), classifyOscillation(input))
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI `oscillation` rejects parsed malformed payloads', () => {
  const missingRoundList = runCli([
    'oscillation',
    JSON.stringify({ previousRound: {}, currentRound: { mustFix: [] }, dispositions: {} }),
  ])
  assert.equal(missingRoundList.status, 2)
  assert.match(missingRoundList.stderr, /oscillation payload is malformed/)

  const malformedFinding = runCli([
    'oscillation',
    JSON.stringify({
      previousRound: { mustFix: [{ title: 'missing file' }] },
      currentRound: { mustFix: [{ title: 'missing file' }] },
      dispositions: {},
    }),
  ])
  assert.equal(malformedFinding.status, 2)
  assert.match(malformedFinding.stderr, /malformed finding/)

  const malformedDisposition = runCli([
    'oscillation',
    JSON.stringify({
      previousRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      currentRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      dispositions: { fixed: { file: 'a.go', line: 1, title: 'stuck' } },
    }),
  ])
  assert.equal(malformedDisposition.status, 2)
  assert.match(malformedDisposition.stderr, /fixed dispositions must be an array/)

  const invalidDispositionContainer = runCli([
    'oscillation',
    JSON.stringify({
      previousRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      currentRound: { mustFix: [finding('a.go', 1, 'stuck')] },
      dispositions: 7,
    }),
  ])
  assert.equal(invalidDispositionContainer.status, 2)
  assert.match(invalidDispositionContainer.stderr, /dispositions must be an object or array/)
})

test('CLI exits non-zero on an unknown subcommand', () => {
  assert.notEqual(runCli(['bogus']).status, 0)
})

test('admitDispatchedRound admits guaranteed and below-cap rounds, then refuses at the cap', () => {
  assert.deepEqual(
    admitDispatchedRound({ guaranteed: true, dispatchedRoundsUsed: 999, maxDispatchedRounds: 1 }),
    { admit: true, reason: 'guaranteed' },
  )
  assert.deepEqual(admitDispatchedRound({ dispatchedRoundsUsed: 5, maxDispatchedRounds: 6 }), {
    admit: true,
    reason: 'below-cap',
  })
  assert.deepEqual(admitDispatchedRound({ dispatchedRoundsUsed: 6, maxDispatchedRounds: 6 }), {
    admit: false,
    reason: 'round-cap',
  })
  assert.deepEqual(admitDispatchedRound({ dispatchedRoundsUsed: '5', maxDispatchedRounds: 6 }), {
    admit: false,
    reason: 'round-cap',
  })
})

test('admitDispatchedRound reason set is closed and reachable', () => {
  const seen = new Set([
    admitDispatchedRound({ guaranteed: true }).reason,
    admitDispatchedRound({ dispatchedRoundsUsed: 0, maxDispatchedRounds: 1 }).reason,
    admitDispatchedRound({ dispatchedRoundsUsed: 1, maxDispatchedRounds: 1 }).reason,
  ])
  assert.deepEqual([...seen].sort(), [...ADMIT_DISPATCHED_ROUND_REASONS].sort())
})

test('admitConfirmingRound refuses exactly the unchanged no-op case', () => {
  const seen = new Set()
  let refused = 0
  for (const tipUnchanged of [false, true]) {
    for (const fixedCount of [0, 1]) {
      for (const verifiedCount of [0, 1]) {
        for (const carriedClaimCount of [0, 1]) {
          for (const invalidCount of [0, 1]) {
            const result = admitConfirmingRound({
              tipUnchanged,
              fixedCount,
              verifiedCount,
              carriedClaimCount,
              invalidCount,
            })
            seen.add(result.reason)
            if (!result.admit) {
              refused += 1
              assert.deepEqual(
                { tipUnchanged, fixedCount, verifiedCount, carriedClaimCount, invalidCount },
                {
                  tipUnchanged: true,
                  fixedCount: 0,
                  verifiedCount: 0,
                  carriedClaimCount: 0,
                  invalidCount: 0,
                },
              )
              assert.equal(result.reason, 'unchanged-tip')
            }
          }
        }
      }
    }
  }
  assert.equal(refused, 1)
  assert.deepEqual([...seen].sort(), [...ADMIT_CONFIRMING_ROUND_REASONS].sort())
})

test('admitConfirmingRound names the single changed conjunct reason', () => {
  assert.deepEqual(admitConfirmingRound({ tipUnchanged: false }), {
    admit: true,
    reason: 'tip-changed',
  })
  assert.deepEqual(admitConfirmingRound({ tipUnchanged: true, fixedCount: 1 }), {
    admit: true,
    reason: 'fixed',
  })
  assert.deepEqual(admitConfirmingRound({ tipUnchanged: true, verifiedCount: 1 }), {
    admit: true,
    reason: 'verified',
  })
  assert.deepEqual(admitConfirmingRound({ tipUnchanged: true, carriedClaimCount: 1 }), {
    admit: true,
    reason: 'carried-claim',
  })
  assert.deepEqual(admitConfirmingRound({ tipUnchanged: true, invalidCount: 1 }), {
    admit: true,
    reason: 'invalid-open',
  })
})

test('CLI admission verbs return decision-table JSON and reject malformed input', () => {
  assert.deepEqual(
    JSON.parse(runCli(['admit-dispatched-round', '{"dispatchedRoundsUsed":6}']).stdout),
    { admit: false, reason: 'round-cap' },
  )
  assert.deepEqual(JSON.parse(runCli(['admit-confirming-round', '{"tipUnchanged":true}']).stdout), {
    admit: false,
    reason: 'unchanged-tip',
  })
  for (const verb of ['admit-dispatched-round', 'admit-confirming-round']) {
    const r = runCli([verb, 'not-json'])
    assert.notEqual(r.status, 0)
    assert.match(r.stderr, new RegExp(`${verb} requires one JSON object argument`))
  }
})

// ---------------------------------------------------------------------------
// admitFixRound — the fix-round decision table, including the must-fix override.
// ---------------------------------------------------------------------------

/** The flagship case: past the deadline, an unattempted must-fix, overrun unspent. */
const FLAGSHIP = Object.freeze({
  remainingSeconds: 830,
  fixRoundSeconds: 1200,
  openMustFix: true,
  unattemptedMustFix: true,
  roundsUsed: 2,
  maxRounds: 3,
  overrunRoundsUsed: 0,
})

test('the overrun allowance is exactly one round', () => {
  assert.equal(MUSTFIX_OVERRUN_ROUNDS, 1)
  assert.equal(DEFAULT_FIX_ROUND_SECONDS, 1200)
  // The seconds form is the REPORTED total, derived — never independently set.
  assert.equal(MUSTFIX_OVERRUN_SECONDS, MUSTFIX_OVERRUN_ROUNDS * DEFAULT_FIX_ROUND_SECONDS)
  // It must sit inside the caller's post-review reserve (POST_REVIEW_RESERVE_MINUTES
  // = 25 -> 1500s), or an override round could overrun the reserve it borrows from.
  assert.ok(MUSTFIX_OVERRUN_SECONDS <= 25 * 60, 'overrun fits inside the post-review reserve')
})

test('admitFixRound admits an unattempted must-fix past the deadline (the BOS ticket case)', () => {
  // Before this override existed, 830 < 1200 refused the round outright and the
  // run terminated BLOCKED naming the clock rather than the finding.
  assert.deepEqual(admitFixRound(FLAGSHIP), { admit: true, reason: 'mustfix-override' })
})

test('admitFixRound refuses when nothing must-fix is open, whatever the budget', () => {
  // openMustFix: false short-circuits, so a huge remainder cannot reach `within-budget`.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: 5000, openMustFix: false }), {
    admit: false,
    reason: 'no-open-mustfix',
  })
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: 0, openMustFix: false }), {
    admit: false,
    reason: 'no-open-mustfix',
  })
})

test('admitFixRound admits on budget only while carrying a finding', () => {
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: 5000 }), {
    admit: true,
    reason: 'within-budget',
  })
  // Exactly the whole allowance still fits — the gate is `>=`, not `>`.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: 1200 }), {
    admit: true,
    reason: 'within-budget',
  })
})

test('admitFixRound treats a null remainder as NO deadline, never as zero', () => {
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: null }), {
    admit: true,
    reason: 'within-budget',
  })
  const { remainingSeconds: _omitted, ...noDeadline } = FLAGSHIP
  assert.deepEqual(admitFixRound(noDeadline), { admit: true, reason: 'within-budget' })
})

test('admitFixRound still admits at a fully spent remainder', () => {
  // Zero left is the case the override exists for: the finding is located,
  // fixable, and nobody has tried yet.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: 0 }), {
    admit: true,
    reason: 'mustfix-override',
  })
})

test('admitFixRound spends the overrun allowance exactly once', () => {
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, overrunRoundsUsed: 1 }), {
    admit: false,
    reason: 'overrun-exhausted',
  })
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, overrunRoundsUsed: 7 }), {
    admit: false,
    reason: 'overrun-exhausted',
  })
})

test('admitFixRound refuses an already-attempted finding past the deadline', () => {
  // Attempted-and-failed is a lawful terminal state; it must not buy a second allowance.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, unattemptedMustFix: false }), {
    admit: false,
    reason: 'all-attempted',
  })
})

test('admitFixRound evaluates the round cap FIRST and never overrides it', () => {
  // Every override input is maximally favorable here; the cap still wins.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, roundsUsed: 3, maxRounds: 3 }), {
    admit: false,
    reason: 'round-cap',
  })
  assert.deepEqual(
    admitFixRound({ ...FLAGSHIP, roundsUsed: 3, maxRounds: 3, remainingSeconds: 99999 }),
    { admit: false, reason: 'round-cap' },
  )
  // The cap itself stays lower-only: an inflated maxRounds cannot raise it.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, roundsUsed: 3, maxRounds: 99 }), {
    admit: false,
    reason: 'round-cap',
  })
  // A lowered cap is honored.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, roundsUsed: 1, maxRounds: 1 }), {
    admit: false,
    reason: 'round-cap',
  })
})

test('admitFixRound falls back in the REFUSING direction on malformed counts', () => {
  // A malformed round count reads as already at the cap. `undefined` is excluded
  // deliberately: an ABSENT key is not malformed, it takes the documented default
  // of 0 (asserted below), while an explicit `null` is an unreadable value.
  for (const roundsUsed of ['2', 2.5, -1, Number.NaN, null, {}]) {
    assert.deepEqual(
      admitFixRound({ ...FLAGSHIP, roundsUsed }),
      { admit: false, reason: 'round-cap' },
      `roundsUsed ${String(roundsUsed)} must fail closed`,
    )
  }
  // ...and a malformed overrun count reads as already spent.
  for (const overrunRoundsUsed of ['0', 0.5, -1, Number.NaN, {}]) {
    assert.deepEqual(
      admitFixRound({ ...FLAGSHIP, overrunRoundsUsed }),
      { admit: false, reason: 'overrun-exhausted' },
      `overrunRoundsUsed ${String(overrunRoundsUsed)} must fail closed`,
    )
  }
  // A malformed remainder reads as spent, so it must EARN the bounded override
  // rather than being granted free `within-budget` admission.
  for (const remainingSeconds of ['5000', Number.NaN, Number.POSITIVE_INFINITY, -5, {}]) {
    assert.deepEqual(
      admitFixRound({ ...FLAGSHIP, remainingSeconds, overrunRoundsUsed: 1 }),
      { admit: false, reason: 'overrun-exhausted' },
      `remainingSeconds ${String(remainingSeconds)} must not grant free admission`,
    )
  }
  // A malformed round price falls back to the documented default.
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: 1199, fixRoundSeconds: 0 }), {
    admit: true,
    reason: 'mustfix-override',
  })
  assert.deepEqual(admitFixRound({ ...FLAGSHIP, remainingSeconds: 1201, fixRoundSeconds: 'x' }), {
    admit: true,
    reason: 'within-budget',
  })
  // An absent count is not a malformed one: it takes the documented default of 0.
  const { roundsUsed: _r, overrunRoundsUsed: _o, ...absent } = FLAGSHIP
  assert.deepEqual(admitFixRound(absent), { admit: true, reason: 'mustfix-override' })
  // No input at all must not admit — nothing is open, so there is nothing to buy.
  assert.deepEqual(admitFixRound(), { admit: false, reason: 'no-open-mustfix' })
  assert.deepEqual(admitFixRound({}), { admit: false, reason: 'no-open-mustfix' })
})

test('every reason in the closed set is reachable, and nothing outside it is returned', () => {
  const reached = new Set()
  for (const input of [
    { ...FLAGSHIP, remainingSeconds: 5000 },
    FLAGSHIP,
    { ...FLAGSHIP, openMustFix: false },
    { ...FLAGSHIP, unattemptedMustFix: false },
    { ...FLAGSHIP, overrunRoundsUsed: 1 },
    { ...FLAGSHIP, roundsUsed: 3 },
  ]) {
    const { admit, reason } = admitFixRound(input)
    assert.equal(typeof admit, 'boolean')
    assert.ok(ADMIT_FIX_ROUND_REASONS.includes(reason), `reason ${reason} is in the closed set`)
    reached.add(reason)
  }
  assert.deepEqual([...reached].sort(), [...ADMIT_FIX_ROUND_REASONS].sort())
})

test('CLI `admit-fix-round` prints the same decision the function returns', () => {
  const r = runCli(['admit-fix-round', JSON.stringify(FLAGSHIP)])
  assert.equal(r.status, 0)
  assert.deepEqual(JSON.parse(r.stdout), admitFixRound(FLAGSHIP))
  // JSON `null` survives as "no deadline", not as 0.
  const noDeadline = runCli([
    'admit-fix-round',
    JSON.stringify({ ...FLAGSHIP, remainingSeconds: null }),
  ])
  assert.deepEqual(JSON.parse(noDeadline.stdout), { admit: true, reason: 'within-budget' })
})

test('CLI `admit-fix-round` rejects a missing or non-object argument', () => {
  for (const args of [
    ['admit-fix-round'],
    ['admit-fix-round', 'not json'],
    ['admit-fix-round', '[]'],
    ['admit-fix-round', 'null'],
    ['admit-fix-round', '3'],
  ]) {
    const r = runCli(args)
    assert.notEqual(r.status, 0)
    assert.match(r.stderr, /requires one JSON object argument/)
    assert.equal(r.stdout, '')
  }
})

// ---------------------------------------------------------------------------
// Skill-file byte-stability guard — the sentinels the emitter prints must stay
// byte-identical in the boss-review skill that documents them.
// ---------------------------------------------------------------------------

// boss-review is a published core: its canonical committed home is the skillinstall
// payload (BOS-271), which the public mirror keeps (only .claude/.codex are stripped),
// so this cross-file assertion normally runs everywhere. The existsSync skip stays as a
// defensive guard against an unexpectedly absent source rather than ENOENT-ing.
const bsReviewSkillPath = fileURLToPath(
  new URL('../services/boss/internal/skillinstall/skills/boss-review/SKILL.md', import.meta.url),
)
test(
  'boss-review SKILL.md still carries the byte-identical sentinels',
  {
    skip: !existsSync(bsReviewSkillPath) && 'boss-review SKILL.md absent',
  },
  () => {
    const skill = readFileSync(bsReviewSkillPath, 'utf8')
    assert.ok(skill.includes(cleanSentinel()), 'clean sentinel present in boss-review SKILL.md')
    assert.ok(
      skill.includes(
        'bs-review capped: unresolved must-fix findings or invalid evidence remain after',
      ),
      'capped sentinel prefix present in boss-review SKILL.md',
    )
    assert.ok(
      skill.includes('bs-review clean: no changes to review.'),
      'empty-diff clean variant present in boss-review SKILL.md',
    )
    // The overrun constants live in two places by necessity — this module (the
    // decision table) and the skill's `## Caller deadline` constants block (the
    // prose an agent reads). Pin them against each other so they cannot drift.
    assert.ok(
      skill.includes(`MUSTFIX_OVERRUN_ROUNDS  = ${MUSTFIX_OVERRUN_ROUNDS}`),
      'MUSTFIX_OVERRUN_ROUNDS agrees with the boss-review constants block',
    )
    assert.ok(
      skill.includes(
        `* 60          # = ${DEFAULT_FIX_ROUND_SECONDS} — the unit the comparison uses`,
      ),
      'DEFAULT_FIX_ROUND_SECONDS agrees with the boss-review constants block',
    )
    assert.ok(
      skill.includes(`# = ${MUSTFIX_OVERRUN_SECONDS} — the reported total`),
      'MUSTFIX_OVERRUN_SECONDS agrees with the boss-review constants block',
    )
  },
)

// ---------------------------------------------------------------------------
// BOS-1209: one gate-outcome line per admission. The three admit-* verbs ARE
// the gate — one runs per review round and either admits it or refuses — so the
// recorded reason reuses the decision's own closed reason vocabulary. Every
// other verb is pure computation and must record nothing.
// ---------------------------------------------------------------------------

const recordedOutcomes = (outcomes) =>
  readFileSync(outcomes, 'utf8')
    .split('\n')
    .filter((line) => line !== '')
    .map((line) => line.split('\t').slice(1))

test('admit-fix-round records one line per invocation, reusing its own reason token', () => {
  const outcomes = join(mkdtempSync(join(tmpdir(), 'bs-review-caps-outcomes-')), 'outcomes.tsv')
  const env = { BOSS_GATE_OUTCOME_FILE: outcomes }

  const admitted = runCli(['admit-fix-round', JSON.stringify({ openMustFix: true })], env)
  assert.equal(admitted.status, 0, admitted.stderr)
  assert.deepEqual(JSON.parse(admitted.stdout), { admit: true, reason: 'within-budget' })
  assert.deepEqual(recordedOutcomes(outcomes), [
    ['bs-review-caps.admit-fix-round', 'pass', 'within-budget'],
  ])

  const refused = runCli(['admit-fix-round', JSON.stringify({ openMustFix: false })], env)
  assert.equal(refused.status, 0, 'a refusal is a verdict on stdout, not a non-zero exit')
  assert.deepEqual(JSON.parse(refused.stdout), { admit: false, reason: 'no-open-mustfix' })
  assert.deepEqual(recordedOutcomes(outcomes)[1], [
    'bs-review-caps.admit-fix-round',
    'fire',
    'no-open-mustfix',
  ])
  assert.ok(ADMIT_FIX_ROUND_REASONS.includes(recordedOutcomes(outcomes)[1][2]))
})

test('the other two admission verbs record under their own gate ids', () => {
  const outcomes = join(mkdtempSync(join(tmpdir(), 'bs-review-caps-outcomes-')), 'outcomes.tsv')
  const env = { BOSS_GATE_OUTCOME_FILE: outcomes }

  const dispatched = runCli(['admit-dispatched-round', JSON.stringify({ guaranteed: true })], env)
  assert.deepEqual(JSON.parse(dispatched.stdout), { admit: true, reason: 'guaranteed' })

  const confirming = runCli(['admit-confirming-round', JSON.stringify({ tipUnchanged: true })], env)
  assert.deepEqual(JSON.parse(confirming.stdout), { admit: false, reason: 'unchanged-tip' })

  assert.deepEqual(recordedOutcomes(outcomes), [
    ['bs-review-caps.admit-dispatched-round', 'pass', 'guaranteed'],
    ['bs-review-caps.admit-confirming-round', 'fire', 'unchanged-tip'],
  ])
  assert.ok(ADMIT_DISPATCHED_ROUND_REASONS.includes('guaranteed'))
  assert.ok(ADMIT_CONFIRMING_ROUND_REASONS.includes('unchanged-tip'))
})

test('a malformed admission argument records one fire line and still exits 2', () => {
  const outcomes = join(mkdtempSync(join(tmpdir(), 'bs-review-caps-outcomes-')), 'outcomes.tsv')
  const res = runCli(['admit-fix-round', '{not json'], { BOSS_GATE_OUTCOME_FILE: outcomes })
  assert.equal(res.status, 2)
  assert.match(res.stderr, /admit-fix-round requires one JSON object argument/)
  assert.deepEqual(recordedOutcomes(outcomes), [
    ['bs-review-caps.admit-fix-round', 'fire', 'malformed-input'],
  ])
})

test('the pure-computation verbs record nothing — they have no verdict to record', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-caps-outcomes-'))
  const outcomes = join(dir, 'outcomes.tsv')
  const env = { BOSS_GATE_OUTCOME_FILE: outcomes }
  for (const args of [
    ['rounds'],
    ['dispatched-rounds'],
    ['sentinel', 'capped', '1'],
    ['match', 'x'],
  ]) {
    const res = runCli(args, env)
    assert.equal(res.status, 0, `${args.join(' ')}: ${res.stderr}`)
  }
  assert.equal(existsSync(outcomes), false, 'a non-gate verb must not create an outcome file')
})
