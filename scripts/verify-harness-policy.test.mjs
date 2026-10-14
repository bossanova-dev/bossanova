// The repo's verify policy no longer parks a PR for a human just because it edits the verify
// evidence harness (`.claude/skills/verify/**`, `.codex/skills/verify/**`): the core judges and
// dispatches verify extensions from the base revision, and the evidence extension drives the PR head
// with the base revision's harness, so a head's harness edits cannot weaken the base's criteria
// for its own verdict (a feature new in the head brings its head recipe, adding criteria only). The
// verify extension directories (`boss-verify-*`) still park: they decide which extensions exist and
// how they are dispatched.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  changedPathHits,
  decideVerdict,
  policySurfaceHits,
} from '../skills-toolbox/verify-gate.mjs'
import { CONFIG_FILENAME, verifyPolicyFromConfigText } from '../skills-toolbox/skill-config.mjs'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const policy = verifyPolicyFromConfigText(
  readFileSync(join(ROOT, CONFIG_FILENAME), 'utf8'),
  CONFIG_FILENAME,
)

const HARNESS_PATHS = [
  '.claude/skills/verify/lib/pr-evidence.mjs',
  '.claude/skills/verify/bin/control-boss',
  '.claude/skills/verify/features/sessions.md',
  '.codex/skills/verify/lib/stack.mjs',
]
const EXTENSION_PATHS = [
  '.claude/skills/boss-verify-evidence/SKILL.md',
  '.codex/skills/boss-verify-evidence/SKILL.md',
]
const CONTROL_PATH = 'services/bossd/internal/session/tmux_chat.go'

// Facts for a head that passes every other gate: open, ready, CI settled and green on the head,
// receipt posted, base green, no verify extensions installed.
function factsFor(changedPaths) {
  // The production path -> hit mapping `judge` applies, so the test pins the real gate.
  const { alwaysHumanHits, humanReviewHits } = changedPathHits(changedPaths, policy)
  return {
    prState: 'OPEN',
    isDraft: false,
    headSha: 'a'.repeat(40),
    baseSha: 'b'.repeat(40),
    ci: { settled: true, green: true },
    eligibilityReasons: [],
    baseState: 'green',
    requireGreenBase: policy.requireGreenBase,
    receipt: 'success',
    needChangedPaths: true,
    changedPaths,
    alwaysHumanHits,
    humanReviewHits,
    policySurfaceHit: policySurfaceHits(changedPaths).length > 0,
    extensions: [],
  }
}

for (const path of HARNESS_PATHS) {
  test(`a PR touching only ${path} passes without a human`, () => {
    const facts = factsFor([path])
    assert.equal(facts.policySurfaceHit, false)
    const r = decideVerdict(facts)
    // A `pass` verdict carries no `reasons` list; a park carries one.
    const reasons = r.reasons ?? []
    assert.ok(!reasons.includes('always-human-path'), `reasons: ${reasons}`)
    assert.ok(!reasons.includes('human-review:security'), `reasons: ${reasons}`)
    assert.equal(r.verdict, 'pass', `reasons: ${reasons}`)
  })
}

test('harness edits mixed with ordinary changes pass', () => {
  const r = decideVerdict(factsFor([CONTROL_PATH, ...HARNESS_PATHS]))
  assert.equal(r.verdict, 'pass', `reasons: ${r.reasons}`)
})

for (const path of EXTENSION_PATHS) {
  test(`a PR touching ${path} still parks for a human`, () => {
    const r = decideVerdict(factsFor([path]))
    assert.equal(r.verdict, 'human')
    assert.ok(r.reasons.includes('always-human-path'), `reasons: ${r.reasons}`)
    assert.ok(r.reasons.includes('human-review:security'), `reasons: ${r.reasons}`)
  })
}

test('an extension edit mixed with harness edits still parks', () => {
  const r = decideVerdict(factsFor([HARNESS_PATHS[0], EXTENSION_PATHS[0]]))
  assert.equal(r.verdict, 'human')
  assert.ok(r.reasons.includes('always-human-path'))
  assert.ok(r.reasons.includes('human-review:security'))
})

test('a PR outside the harness and the policy still passes', () => {
  const r = decideVerdict(factsFor([CONTROL_PATH]))
  assert.equal(r.verdict, 'pass')
})
