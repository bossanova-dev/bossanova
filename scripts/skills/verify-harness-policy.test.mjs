// The repo's verify policy parks visual changes, migrations, authentication, billing,
// credentials and API contracts, plus infrastructure, CI and the verify judge itself.
// The evidence harness and ordinary skill markdown remain unparked. Policy-surface edits
// still park every category because they control the judge and its extension dispatch.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  changedPathHits,
  decideVerdict,
  policySurfaceHits,
} from '../../skills-toolbox/verify/verify-gate.mjs'
import { CONFIG_FILENAME, verifyPolicyFromConfigText } from '../../skills-toolbox/skill-config.mjs'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '../..')
const policy = verifyPolicyFromConfigText(
  readFileSync(join(ROOT, CONFIG_FILENAME), 'utf8'),
  CONFIG_FILENAME,
)

const HARNESS_PATHS = [
  '.claude/skills/verify/lib/pr-evidence.mjs',
  '.claude/skills/verify/bin/control-boss',
  'docs/features/sessions.md',
  '.codex/skills/verify/lib/stack.mjs',
]
const POLICY_SURFACE_PATHS = [
  CONFIG_FILENAME,
  '.claude/skills/boss-verify-evidence/SKILL.md',
  '.codex/skills/boss-verify-evidence/SKILL.md',
]
const CONTROL_PATH = 'services/bossd/internal/session/tmux_chat.go'
const ALL_CATEGORIES = ['human-review:database', 'human-review:security', 'human-review:ui']

// [path, exact park reasons]. Tests beside sensitive files park too: globs cannot exclude them.
const SAMPLE_PATHS = [
  ['services/web/src/shared/ui/Button.tsx', ['human-review:ui']],
  ['services/web/src/shared/ui/Button.test.tsx', ['human-review:ui']],
  ['services/web/src/lib/x.ts', []],
  ['lib/ui-tokens/index.css', ['human-review:ui']],
  ['lib/ui-tokens/package.json', []],
  ['services/web/index.html', ['human-review:ui']],
  ['services/web/public/_headers', []],
  ['services/marketing/src/pages/index.astro', ['human-review:ui']],
  ['services/marketing/public/_redirects', []],
  ['services/docs/docs/intro.md', ['human-review:ui']],
  ['services/docs/package.json', []],
  ['services/docs/styles/BossanovaProse/BannedWords.yml', []],
  ['services/boss/api/tui/home/home_view.go', ['human-review:ui']],
  ['services/bosso/internal/auth/x.go', ['human-review:security']],
  ['services/bossd/migrations/001.sql', ['human-review:database']],
  ['services/bosso/migrations_postgres/001.sql', ['human-review:database']],
  ['.github/workflows/ci.yml', ['always-human-path']],
  ['infra/environments/main.tf', ['always-human-path']],
  ['services/bossd/internal/db/session_store.go', []],
  ['.claude/skills/boss-build-ce/SKILL.md', []],
  ['docs/skills/factory.md', []],
  ['services/bosso/api/orchestrator/agentaccounts/credentials.go', ['human-review:security']],
  ['services/bossd/internal/devicekey/devicekey.go', ['human-review:security']],
  ['services/bossd/api/upstream/credential_devices.go', ['human-review:security']],
  ['lib/bossalib/contract/daemon/keyringutil/keyring.go', ['human-review:security']],
  ['services/mcp-gateway/internal/auth/auth.go', ['human-review:security']],
  ['proto/bossanova/v1/models.proto', ['human-review:security']],
  [
    'lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/toolbox/verify/verify-gate.mjs',
    ['always-human-path'],
  ],
  ['.claude/settings.json', []],
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

function assertPathVerdict(paths, expectedReasons) {
  const facts = factsFor(paths)
  assert.equal(facts.policySurfaceHit, false)
  const r = decideVerdict(facts)
  assert.equal(r.verdict, expectedReasons.length ? 'human' : 'pass', `reasons: ${r.reasons}`)
  assert.deepEqual([...(r.reasons ?? [])].sort(), [...expectedReasons].sort())
}

for (const [path, expectedReasons] of SAMPLE_PATHS) {
  test(`a PR touching only ${path} ${expectedReasons.length ? `parks as ${expectedReasons.join(', ')}` : 'passes without a human'}`, () => {
    assertPathVerdict([path], expectedReasons)
  })
}

// Historical changes replayed through representative paths in today's tree.
const REPLAY_ROWS = [
  [
    '#3407',
    [
      'services/bossd/migrations/20261020130000_rename_attempts_to_fix_attempts.sql',
      'proto/bossanova/v1/models.proto',
    ],
    ['human-review:database', 'human-review:security'],
  ],
  [
    '#3415',
    ['services/bossd/migrations/20261020140000_rename_resume_run_key_to_budget_key.sql'],
    ['human-review:database'],
  ],
  ['#2996', ['services/bosso/internal/billing/organizations.go'], ['human-review:security']],
  ['#3001', ['services/bosso/internal/domain/policy/session.go'], ['human-review:security']],
  [
    '#3002',
    ['services/bosso/internal/daemonproxy/session_org_authz.go'],
    ['human-review:security'],
  ],
  [
    '#3609',
    ['services/bosso/api/orchestrator/entitlement_interceptor.go'],
    ['human-review:security'],
  ],
  ...['#2988', '#3106', '#3117', '#3149', '#3172', '#3177', '#3434'].map((pr) => [
    pr,
    ['skills-toolbox/verify/verify-gate.mjs'],
    ['always-human-path'],
  ]),
  ['verify route', ['skills-toolbox/verify/verify-route.mjs'], ['always-human-path']],
  ['verify range', ['skills-toolbox/verify/verify-range.mjs'], ['always-human-path']],
  [
    '#3597',
    ['services/bosso/api/auth/jwt.go', 'infra/environments/main.tf'],
    ['human-review:security', 'always-human-path'],
  ],
  ['#3701', ['services/mcp-gateway/api/gateway/middleware.go'], ['human-review:security']],
]

for (const [name, paths, expectedReasons] of REPLAY_ROWS) {
  test(`evidence replay ${name} parks as ${expectedReasons.join(', ')}`, () => {
    assertPathVerdict(paths, expectedReasons)
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

// The gate counts the policy surface while the base policy is configured.
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

test('a PR outside the review paths and the policy surface passes', () => {
  const r = decideVerdict(factsFor([CONTROL_PATH]))
  assert.equal(r.verdict, 'pass')
})
