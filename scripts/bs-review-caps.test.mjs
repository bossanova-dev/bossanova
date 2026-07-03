import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
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
  assert.equal(cappedSentinel(3), 'bs-review capped: open must-fix findings remain after 3 rounds.')
  assert.equal(cappedSentinel(2), 'bs-review capped: open must-fix findings remain after 2 rounds.')
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
    matchSentinel('bs-review capped: open must-fix findings remain after 3 rounds.'),
    {
      status: 'capped',
      rounds: 3,
    },
  )
})

test('matchSentinel rejects malformed capped sentinels', () => {
  assert.equal(
    matchSentinel('bs-review capped: open must-fix findings remain after 0 rounds.'),
    null,
  )
  assert.equal(
    matchSentinel('bs-review capped: open must-fix findings remain after abc rounds.'),
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
    'bs-review capped: open must-fix findings remain after 3 rounds.',
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
  const r = runCli(['match', 'bs-review capped: open must-fix findings remain after 2 rounds.'])
  assert.deepEqual(JSON.parse(r.stdout), { status: 'capped', rounds: 2 })
})

test('CLI exits non-zero on an unknown subcommand', () => {
  assert.notEqual(runCli(['bogus']).status, 0)
})

// ---------------------------------------------------------------------------
// Skill-file byte-stability guard — the sentinels the emitter prints must stay
// byte-identical in the bs-review skill that documents them.
// ---------------------------------------------------------------------------

test('bs-review SKILL.md still carries the byte-identical sentinels', () => {
  const skill = readFileSync(
    new URL('../.claude/skills/bs-review/SKILL.md', import.meta.url),
    'utf8',
  )
  assert.ok(skill.includes(cleanSentinel()), 'clean sentinel present in bs-review SKILL.md')
  assert.ok(
    skill.includes('bs-review capped: open must-fix findings remain after'),
    'capped sentinel prefix present in bs-review SKILL.md',
  )
  assert.ok(
    skill.includes('bs-review clean: no changes to review.'),
    'empty-diff clean variant present in bs-review SKILL.md',
  )
})
