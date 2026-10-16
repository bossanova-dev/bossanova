// The repo's verify policy parks a PR for a human only for visual changes: the TUI view package and
// the visible parts of services/web, services/marketing and services/docs (`humanReview.ui`).
// Security-sensitive code, migrations, CI and the verify evidence harness (`.claude/skills/verify/**`,
// `.codex/skills/verify/**`) merge without a human. The verify extension directories
// (`boss-verify-*`) and `.boss-skills.json` still park for every category through the policy
// surface: they decide which extensions exist, how they are dispatched, and what parks.
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
import {
  CONFIG_FILENAME,
  policyMirrorGaps,
  verifyPolicyFromConfigText,
} from '../skills-toolbox/skill-config.mjs'

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
const POLICY_SURFACE_PATHS = [
  CONFIG_FILENAME,
  '.claude/skills/boss-verify-evidence/SKILL.md',
  '.codex/skills/boss-verify-evidence/SKILL.md',
]
const CONTROL_PATH = 'services/bossd/internal/session/tmux_chat.go'
const ALL_CATEGORIES = ['human-review:database', 'human-review:security', 'human-review:ui']

// [path, parks as ui]. A test file beside a component parks too: globs cannot exclude it.
const SAMPLE_PATHS = [
  ['services/web/src/components/Button.tsx', true],
  ['services/web/src/components/Button.test.tsx', true],
  ['services/web/src/lib/x.ts', false],
  ['lib/ui-tokens/index.css', true],
  ['lib/ui-tokens/package.json', false],
  ['services/web/index.html', true],
  ['services/web/public/_headers', false],
  ['services/marketing/src/pages/index.astro', true],
  ['services/marketing/public/_redirects', false],
  ['services/docs/docs/intro.md', true],
  ['services/docs/package.json', false],
  ['services/docs/styles/BossanovaProse/BannedWords.yml', false],
  ['services/boss/internal/views/home_view.go', true],
  ['services/bosso/internal/auth/x.go', false],
  ['services/bossd/migrations/001.sql', false],
  ['services/bosso/migrations_postgres/001.sql', false],
  ['.github/workflows/ci.yml', false],
  ['.claude/settings.json', false],
]

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

for (const [path, parks] of SAMPLE_PATHS) {
  test(`a PR touching only ${path} ${parks ? 'parks as ui' : 'passes without a human'}`, () => {
    const facts = factsFor([path])
    assert.equal(facts.policySurfaceHit, false)
    const r = decideVerdict(facts)
    if (parks) {
      assert.equal(r.verdict, 'human')
      assert.deepEqual(r.reasons, ['human-review:ui'])
    } else {
      assert.equal(r.verdict, 'pass', `reasons: ${r.reasons}`)
    }
  })
}

for (const path of HARNESS_PATHS) {
  test(`a PR touching only ${path} passes without a human`, () => {
    const facts = factsFor([path])
    assert.equal(facts.policySurfaceHit, false)
    assert.equal(decideVerdict(facts).verdict, 'pass')
  })
}

test('harness edits mixed with ordinary changes pass', () => {
  const r = decideVerdict(factsFor([CONTROL_PATH, ...HARNESS_PATHS]))
  assert.equal(r.verdict, 'pass', `reasons: ${r.reasons}`)
})

// The gate counts the policy surface only while the base policy is configured; the ui list is what
// keeps it configured now that every other list is empty.
for (const path of POLICY_SURFACE_PATHS) {
  test(`a PR touching ${path} still parks for every category`, () => {
    const facts = factsFor([path])
    assert.equal(facts.policySurfaceHit, true)
    const r = decideVerdict(facts)
    assert.equal(r.verdict, 'human')
    assert.deepEqual(r.reasons, ALL_CATEGORIES)
  })
}

test('a policy-surface edit mixed with harness edits still parks', () => {
  const r = decideVerdict(factsFor([HARNESS_PATHS[0], POLICY_SURFACE_PATHS[1]]))
  assert.equal(r.verdict, 'human')
  assert.deepEqual(r.reasons, ALL_CATEGORIES)
})

test('a PR outside the visual paths and the policy surface passes', () => {
  const r = decideVerdict(factsFor([CONTROL_PATH]))
  assert.equal(r.verdict, 'pass')
})

test('the repo policy has no unsafe legacy mirror gaps', () => {
  assert.deepEqual(
    policyMirrorGaps(policy),
    [],
    'mirror the category glob into alwaysHumanPaths, or remove the mirror entirely',
  )
})
