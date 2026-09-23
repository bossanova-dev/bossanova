// Tests for check-await-mechanism — the gate that keeps "await it, never `run_in_background`"
// from being a mandate with no mechanism attached (BOS-1278).
//
// Every case below is built from a CONSTRUCTED skill tree on disk, not from a fixture string, so
// the discovery walk, the published/repo-local split, and the obligation set are all exercised the
// way the real scan exercises them. The last test runs the gate against the real tree.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  AWAIT_MECHANISM,
  BASH_TIMEOUT_FLOOR_MS,
  DISPATCH_SIGNALS,
  NEUTRAL_TIMEOUT_BOUND,
  PUBLISHED_ROOT,
  SKILL_ROOTS,
  checkAwaitMechanism,
  discoverSkills,
  orchestratesDispatch,
  statedBashTimeoutMs,
} from './check-await-mechanism.mjs'

/** Build a throwaway repo root holding the named skills. */
function tree(skills) {
  const root = mkdtempSync(join(tmpdir(), 'caw-'))
  for (const [relDir, files] of Object.entries(skills)) {
    for (const [name, body] of Object.entries(files)) {
      const full = join(root, relDir, name)
      mkdirSync(join(full, '..'), { recursive: true })
      writeFileSync(full, body)
    }
  }
  return root
}

const REPO_LOCAL = SKILL_ROOTS.find((r) => r !== PUBLISHED_ROOT)

const COMPLIANT_LOCAL = `Dispatch an awaited subagent_type: general-purpose worker.
Classify it with ${AWAIT_MECHANISM} and poll with an explicit timeout: 600000.`

test('a skill is in class only when its own markdown shows it DISPATCHING', () => {
  // The discriminator the enumeration settled on: the obligation belongs where the dispatch is
  // orchestrated. A skill that is dispatched — a lens, a round, a brief a worker reads — carries
  // none of this vocabulary and needs no exclusion rule to stay out.
  for (const signal of DISPATCH_SIGNALS) {
    assert.equal(orchestratesDispatch(`text ${signal} text`), true, signal)
  }
  assert.equal(
    orchestratesDispatch('This extension is dispatched by a core and returns a report.'),
    false,
  )
})

test('an in-class repo-local skill must name BOTH the mechanism and a long Bash timeout', () => {
  const missingBoth = tree({
    [`${REPO_LOCAL}/sweep-x`]: { 'SKILL.md': 'Await it — never run_in_background.' },
  })
  const result = checkAwaitMechanism({ root: missingBoth })
  assert.equal(result.ok, false)
  assert.equal(result.failures.length, 2, 'the two obligations are reported separately')
  assert.ok(result.failures.some((f) => f.includes('no await mechanism')))
  assert.ok(result.failures.some((f) => f.includes('no explicit Bash timeout')))

  // Able to fire in the other direction: the SAME skill with both named is clean, so the
  // refusal above is the omission's doing and not a blanket failure.
  const compliant = tree({ [`${REPO_LOCAL}/sweep-x`]: { 'SKILL.md': COMPLIANT_LOCAL } })
  assert.deepEqual(checkAwaitMechanism({ root: compliant }).failures, [])
})

test('the obligation is satisfied anywhere in the skill, not only in SKILL.md', () => {
  // The debt sweep factored its whole dispatch contract into a reference. The contract is
  // per-skill, so a gate that demanded SKILL.md would force a duplicate of it.
  const split = tree({
    [`${REPO_LOCAL}/sweep-y`]: {
      'SKILL.md': 'Dispatch with subagent_type: general-purpose; see the reference.',
      'references/dispatch.md': `Use ${AWAIT_MECHANISM} with an explicit timeout: 600000.`,
    },
  })
  assert.deepEqual(checkAwaitMechanism({ root: split }).failures, [])
})

test('a short explicit timeout is refused — it restates the default rather than surviving it', () => {
  // 120000 is the harness threshold itself. Accepting it would let a skill satisfy the letter of
  // the rule with the exact value that produces the defect.
  assert.equal(statedBashTimeoutMs('timeout: 600000'), 600_000)
  assert.equal(statedBashTimeoutMs('timeout: 120000 and timeout: 900000'), 900_000, 'longest wins')
  assert.equal(statedBashTimeoutMs('no timeout stated at all'), null)

  const short = tree({
    [`${REPO_LOCAL}/sweep-z`]: {
      'SKILL.md': `subagent_type: general-purpose with ${AWAIT_MECHANISM}, timeout: 120000.`,
    },
  })
  const failures = checkAwaitMechanism({ root: short }).failures
  assert.equal(failures.length, 1)
  assert.ok(failures[0].includes(`below the ${BASH_TIMEOUT_FLOOR_MS} floor`))
})

test('a PUBLISHED core SUBSTITUTES the agent-neutral bound for one agent’s timeout parameter', () => {
  // `timeout:` is a Bash-tool parameter of one agent. A published core installs into every user's
  // global tree and must name the agent-neutral shape, so requiring the concrete value there would
  // trade this defect for the one dispatch-graph-audit.md already forbids. What it may NOT do is
  // state no bound at all — an `if (published) continue` would pass exactly that skill, leaving the
  // reasoning that justifies the exemption documented and unenforced.
  const core = tree({
    [`${PUBLISHED_ROOT}/boss-x`]: {
      'SKILL.md':
        `Dispatch subagent_type: general-purpose and classify with ${AWAIT_MECHANISM}, ` +
        `bounded by ${NEUTRAL_TIMEOUT_BOUND}.`,
    },
  })
  assert.deepEqual(checkAwaitMechanism({ root: core }).failures, [])

  const unbounded = tree({
    [`${PUBLISHED_ROOT}/boss-z`]: {
      'SKILL.md': `Dispatch subagent_type: general-purpose and classify with ${AWAIT_MECHANISM}.`,
    },
  })
  const unboundedFailures = checkAwaitMechanism({ root: unbounded }).failures
  assert.equal(unboundedFailures.length, 1)
  assert.ok(unboundedFailures[0].includes(NEUTRAL_TIMEOUT_BOUND))

  // A published core is never asked for the concrete value, so naming the neutral bound alone —
  // with no `timeout:` anywhere — is a pass, not a near-miss.
  assert.equal(statedBashTimeoutMs(`bounded by ${NEUTRAL_TIMEOUT_BOUND}`), null)

  // The mechanism is owed on top of the bound, so both clauses fire independently.
  const bare = tree({
    [`${PUBLISHED_ROOT}/boss-y`]: { 'SKILL.md': 'Await it — never run_in_background.' },
  })
  const failures = checkAwaitMechanism({ root: bare }).failures
  assert.equal(failures.length, 2)
  assert.ok(failures.some((f) => f.includes('no await mechanism')))
  assert.ok(failures.some((f) => f.includes(NEUTRAL_TIMEOUT_BOUND)))
})

test('an empty scan FAILS rather than passing vacuously', () => {
  // A walk that finds nothing and returns ok is indistinguishable from a clean tree, which is how
  // a gate keeps reporting green after a root is renamed out from under it.
  const empty = tree({ [`${REPO_LOCAL}/not-a-dispatcher`]: { 'SKILL.md': 'Read-only guidance.' } })
  const result = checkAwaitMechanism({ root: empty })
  assert.equal(result.ok, false)
  assert.equal(result.checked, 0)
  assert.ok(result.failures[0].includes('found nothing to check'))
})

test('discovery reads both authoring roots and never the generated codex mirror', () => {
  const both = tree({
    [`${REPO_LOCAL}/local-one`]: { 'SKILL.md': 'x' },
    [`${PUBLISHED_ROOT}/core-one`]: { 'SKILL.md': 'x' },
    '.codex/skills/mirror-one': { 'SKILL.md': 'x' },
  })
  const dirs = discoverSkills({ root: both }).map((s) => s.dir)
  assert.deepEqual(dirs.sort(), [`${PUBLISHED_ROOT}/core-one`, `${REPO_LOCAL}/local-one`].sort())
})

test('the real tree satisfies the gate, over a non-empty discovered class', () => {
  const result = checkAwaitMechanism()
  assert.deepEqual(result.failures, [])
  // Non-vacuity: this repo really does orchestrate dispatches, in both roots. A scan that found
  // one root only would pass the assertion above while checking half the class.
  assert.ok(result.checked >= 10, `expected a substantial class, got ${result.checked}`)
  for (const root of SKILL_ROOTS) {
    assert.ok(
      result.inClass.some((dir) => dir.startsWith(`${root}/`)),
      `no in-class skill discovered under ${root}`,
    )
  }
})
