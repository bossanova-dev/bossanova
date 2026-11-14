// The repo's verify policy parks visual changes, migrations, authentication, billing,
// credentials and API contracts, plus infrastructure, CI and the verify judge itself.
// The evidence harness and ordinary skill markdown remain unparked. Policy-surface edits
// still ask for review because they control the judge and its extension dispatch.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  levelAreasFor,
  decideVerdict,
  policySurfaceHits,
} from '../../skills-toolbox/verify/verify-gate.mjs'
import { CONFIG_FILENAME, verifyPolicyFromConfigText } from '../../skills-toolbox/skill-config.mjs'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '../..')
const configText = readFileSync(join(ROOT, CONFIG_FILENAME), 'utf8')
const policy = verifyPolicyFromConfigText(configText, CONFIG_FILENAME)
// The repo's merge rules, which the gate reads from the same base-tip block.
const mergePolicy = JSON.parse(configText).verifyDefaults?.merge ?? null

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
const POLICY_REASONS = ['human-review:asks']

for (const changes of [0, 2, undefined]) {
  test(`area mapping preserves UI rename changes ${changes} without a patch`, () => {
    const { areas } = levelAreasFor(
      [
        {
          filename: 'src/ui/New.tsx',
          previous_filename: 'src/ui/Old.tsx',
          status: 'renamed',
          changes,
        },
      ],
      { review: { alwaysReview: [], noSignOff: [] } },
    )
    assert.deepEqual(
      areas.map(({ area, defaultAsk }) => [area, defaultAsk]),
      [changes === 0 ? ['housekeeping', 'never'] : ['ui', 'unless-proven']],
    )
  })
}

// [path, exact park reasons]. Tests beside sensitive files park too: globs cannot exclude them.
const SAMPLE_PATHS = [
  ['services/web/src/shared/ui/Button.tsx', ['human-review:ui']],
  ['services/web/src/shared/ui/Button.test.tsx', ['human-review:ui']],
  ['services/web/src/lib/x.ts', []],
  ['lib/ui-tokens/index.css', ['human-review:ui']],
  ['lib/ui-tokens/package.json', ['human-review:asks']],
  ['services/web/index.html', ['human-review:ui']],
  ['services/web/public/_headers', []],
  ['services/marketing/src/pages/index.astro', ['human-review:ui']],
  ['services/marketing/public/_redirects', []],
  ['services/docs/docs/intro.md', ['human-review:ui']],
  ['services/docs/package.json', ['human-review:asks']],
  ['services/docs/styles/BossanovaProse/BannedWords.yml', ['human-review:ui']],
  ['services/boss/api/tui/home/home_view.go', ['human-review:ui']],
  ['services/bosso/internal/auth/x.go', ['human-review:security']],
  ['services/bossd/migrations/001.sql', ['human-review:database']],
  ['services/bosso/migrations_postgres/001.sql', ['human-review:database']],
  ['.github/workflows/ci.yml', ['always-human-path']],
  ['infra/environments/main.tf', ['always-human-path']],
  ['services/bossd/internal/db/session_store.go', []],
  ['.claude/skills/boss-build-ce/SKILL.md', ['human-review:asks']],
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
  ['.claude/settings.json', ['human-review:asks']],
]

// Facts for a head that passes every other gate: open, ready, CI settled and green on the head,
// receipt posted, base green, no verify extensions installed.
function factsFor(changedPaths) {
  // The production path -> hit mapping `judge` applies, so the test pins the real gate.
  const { areas } = levelAreasFor(changedPaths, policy)
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
    areas,
    judgeAreas: [],
    review: policy.review,
    mergePolicy,
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
  test(`a PR touching only ${path} asks through its built-in area`, () => {
    const facts = factsFor([path])
    assert.equal(facts.policySurfaceHit, false)
    assert.equal(decideVerdict(facts).verdict, path.startsWith('docs/') ? 'pass' : 'human')
  })
}

test('harness edits mixed with ordinary changes ask for review', () => {
  const r = decideVerdict(factsFor([CONTROL_PATH, ...HARNESS_PATHS]))
  assert.equal(r.verdict, 'human', `reasons: ${r.reasons}`)
})

// The gate counts the policy surface while the base policy is configured.
for (const path of POLICY_SURFACE_PATHS) {
  test(`a PR touching ${path} still asks for review`, () => {
    const facts = factsFor([path])
    assert.equal(facts.policySurfaceHit, true)
    const r = decideVerdict(facts)
    assert.equal(r.verdict, 'human')
    assert.deepEqual(r.reasons, POLICY_REASONS)
  })
}

test('a policy-surface edit mixed with harness edits still parks', () => {
  const r = decideVerdict(factsFor([HARNESS_PATHS[0], POLICY_SURFACE_PATHS[1]]))
  assert.equal(r.verdict, 'human')
  assert.deepEqual(r.reasons, POLICY_REASONS)
})

test('a PR outside the review paths and the policy surface passes', () => {
  const r = decideVerdict(factsFor([CONTROL_PATH]))
  assert.equal(r.verdict, 'pass')
})
