import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import {
  DEFAULT_REVIEW_MAX_ROUNDS,
  resolveMaxRounds,
  reviewMaxRounds,
  CLEAN_PREFIX,
  CAPPED_PREFIX,
  cleanSentinel,
  cappedSentinel,
  matchSentinel,
  DEFAULT_FIX_ROUND_SECONDS,
  MUSTFIX_OVERRUN_ROUNDS,
  MUSTFIX_OVERRUN_SECONDS,
  ADMIT_FIX_ROUND_REASONS,
  admitFixRound,
} from './bs-review-caps.mjs'

const scriptPath = fileURLToPath(new URL('./bs-review-caps.mjs', import.meta.url))

/** Run the CLI block as a subprocess with an overlaid env; return trimmed stdout + status. */
function runCli(args = [], env = {}) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], {
    encoding: 'utf8',
    env: { ...process.env, ...env },
  })
  return { stdout: res.stdout.trim(), stderr: res.stderr.trim(), status: res.status }
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
})

// ---------------------------------------------------------------------------
// CLI — the surface the skill prose invokes.
// ---------------------------------------------------------------------------

test('CLI `rounds` reads the env and clamps lower-only', () => {
  assert.equal(runCli(['rounds'], { BS_REVIEW_MAX_ROUNDS: '2' }).stdout, '2')
  assert.equal(runCli(['rounds'], { BS_REVIEW_MAX_ROUNDS: '9' }).stdout, '3')
  assert.equal(runCli(['rounds'], { BS_REVIEW_MAX_ROUNDS: '' }).stdout, '3')
})

test('CLI `sentinel clean|capped` prints byte-identical lines', () => {
  assert.equal(runCli(['sentinel', 'clean']).stdout, 'bs-review clean: no open must-fix findings.')
  assert.equal(
    runCli(['sentinel', 'capped', '3']).stdout,
    'bs-review capped: unresolved must-fix findings or invalid evidence remain after 3 rounds.',
  )
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

test('CLI exits non-zero on an unknown subcommand', () => {
  assert.notEqual(runCli(['bogus']).status, 0)
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
