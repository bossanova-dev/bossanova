import { parkNote } from './verify-park-note.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { decideVerdict, judge, merge, post } from './verify-gate.mjs'
import { describeAreas } from './verify-describe.mjs'
import { pathParkFacts } from './verify-path-parks.mjs'
import { verifyPolicyFromConfigText } from '../skill-config.mjs'

const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
const PR_URL = 'https://github.com/o/r/pull/7'
const TRIAL = 'c'.repeat(40)
const AUTO_MODERATE = { auto: [{ level: 'moderate' }, { level: 'low' }] }
const REPO_CONFIG = readFileSync(
  fileURLToPath(new URL('../../.boss-skills.json', import.meta.url)),
  'utf8',
)

/** decideVerdict over a head that changes `paths`, under `verifyDefaults` read from the base tip. */
function decide(paths, verifyDefaults = {}, extra = {}) {
  const text = JSON.stringify({ verifyDefaults })
  const policy = verifyPolicyFromConfigText(text, 'base')
  return decideVerdict({
    ...pathParkFacts(paths, policy),
    review: policy.review,
    mergePolicy: JSON.parse(text).verifyDefaults.merge ?? null,
    changedPaths: paths,
    judgeAreas: [],
    prState: 'OPEN',
    isDraft: false,
    ci: { settled: true, green: true },
    receipt: 'success',
    extensions: [],
    ...extra,
  })
}
const outcome = (r) => [r.verdict, r.reason]

test('with no merge block or an empty one, a high head parks human-merge', () => {
  assert.deepEqual(outcome(decide(['src/a.go'])), ['human', 'human-merge'])
  assert.deepEqual(outcome(decide(['src/a.go'], { merge: {} })), ['human', 'human-merge'])
  assert.deepEqual(outcome(decide(['src/a.go'], { merge: { auto: [] } })), ['human', 'human-merge'])
  assert.deepEqual(outcome(decide(['src/a.go'], {}, { mergePolicy: undefined })), [
    'human',
    'human-merge',
  ])
  assert.deepEqual(outcome(decide(['src/a.go'], { merge: { auto: [{ level: 'high' }] } })), [
    'pass',
    'verified',
  ])
})

test('the floor beats a level-only rule for the head level', () => {
  const cap = {
    key: 'other:src',
    label: 'src',
    cappedAt: '2026-10-01T00:00:00.000Z',
    escapes: [{ original: 1, fix: 2, fixMergedAt: '2026-10-01T00:00:00.000Z' }],
  }
  for (const [label, paths, verifyDefaults, extra, reason] of [
    [
      'alwaysReview',
      ['billing/a.go'],
      { alwaysReview: { Billing: ['billing/**'] } },
      {},
      'always-human-path',
    ],
    [
      'alwaysHumanPaths',
      ['billing/a.go'],
      { alwaysHumanPaths: ['billing/**'] },
      {},
      'always-human-path',
    ],
    [
      'capped area',
      ['src/a.go'],
      {},
      { areaCaps: { caps: [cap], health: [] } },
      'human-review:asks',
    ],
    [
      'verify-policy edit',
      ['.boss-skills.json'],
      { alwaysReview: { X: ['x/**'] } },
      {},
      'human-review:asks',
    ],
  ]) {
    const r = decide(paths, { ...verifyDefaults, merge: AUTO_MODERATE }, extra)
    assert.deepEqual(outcome(r), ['human', reason], label)
  }
})

test('an extension sign-off ask beats a level-only rule', () => {
  const extension = { name: 'boss-verify-sec', role: 'verify' }
  const r = decide(
    ['src/a.go'],
    { merge: AUTO_MODERATE },
    {
      extensions: [extension],
      extensionResults: [
        {
          extension: extension.name,
          result: {
            ok: true,
            extension: extension.name,
            role: 'verify',
            verdict: 'human',
            reason: 'human-review:security',
            evidence: [{ kind: 'video', ref: 'proof.mp4' }],
            findings: [],
          },
        },
      ],
    },
  )
  assert.deepEqual(outcome(r), ['human', 'human-review:security'])
})

test('an uncovered ask parks with its own code beside a covered one', () => {
  const merge = { auto: [{ area: 'migrations' }] }
  const r = decide(['db/001.sql', 'main.tf'], { merge })
  assert.deepEqual(outcome(r), ['human', 'human-review:infra'])
  assert.deepEqual(r.labels, ['Infrastructure'])
})

test('a judge-raised ask is not covered by a level-only rule', () => {
  const judgeAreas = [
    {
      area: 'judge:access',
      label: 'Access',
      paths: ['src/a.go'],
      defaultAsk: 'always',
      source: 'judge',
      reason: 'removed an authorization check',
    },
  ]
  const r = decide(['src/a.go'], { merge: AUTO_MODERATE }, { judgeAreas })
  assert.deepEqual(outcome(r), ['human', 'human-review:judge'])
})

test('an additive rule parks an api-contract change a describer calls breaking or failed', () => {
  const merge = { auto: [{ area: 'api-contract', change: 'additive' }] }
  const policy = verifyPolicyFromConfigText(JSON.stringify({ verifyDefaults: { merge } }), 'base')
  const [built] = pathParkFacts(['api/x.proto'], policy).areas.filter(
    (area) => area.area === 'api-contract',
  )
  // The core diff says additive, as apiSchemaChange reports it before describers run.
  const additive = { ...built, change: 'additive', changeReasons: [] }
  const describer = { extension: 'boss-verify-describe-x', area: 'api-contract' }
  const described = (outcomes) => ({ areas: describeAreas([additive], outcomes).areas })
  const ok = (breaking) => ({
    ...describer,
    ok: true,
    description: { items: ['field removed'], links: [], breaking },
  })
  assert.deepEqual(outcome(decide(['api/x.proto'], { merge }, described([ok(false)]))), [
    'pass',
    'verified',
  ])
  for (const [label, outcomes] of [
    ['describer breaking', [ok(true)]],
    ['describer failed', [{ ...describer, ok: false, reason: 'timeout' }]],
  ]) {
    assert.equal(described(outcomes).areas[0].change, 'breaking', label)
    const r = decide(['api/x.proto'], { merge }, described(outcomes))
    assert.deepEqual(outcome(r), ['human', 'human-review:api-contract'], label)
  }
})

test("Bossanova's own rules merge a high head and a tested UI head, and park an unproven one", () => {
  const { verifyDefaults } = JSON.parse(REPO_CONFIG)
  assert.ok(verifyDefaults.merge, 'the repository declares its merge rules')
  assert.deepEqual(outcome(decide(['services/bossd/internal/x.go'], verifyDefaults)), [
    'pass',
    'verified',
  ])
  const ui = ['services/web/src/routes/Home.tsx']
  assert.deepEqual(outcome(decide(ui, verifyDefaults, { uiTests: { covered: true } })), [
    'pass',
    'verified',
  ])
  assert.deepEqual(outcome(decide(ui, verifyDefaults, { uiTests: { covered: false } })), [
    'human',
    'human-review:ui',
  ])
  const tui = ['services/boss/api/tui/x.go', ...ui]
  assert.deepEqual(outcome(decide(tui, verifyDefaults, { uiTests: { covered: true } })), [
    'human',
    'human-review:ui',
  ])
})

/**
 * A judge world over one PR, its changed files, the base tip's config and the head's. `trial`
 * makes the gate's session a verify-only one with policy ref `dev-config`: `session` is its row
 * (null for an unreadable read), `config` the config on origin/dev-config at TRIAL, and `missing`
 * a ref origin does not have. `verify` is a `boss/verify` status standing on the head.
 */
function fixture({ paths, baseConfig, headConfig = {}, trial = null, verify = null }) {
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const fail = () => ({ ok: false, exit: 1, output: '' })
  const trees = []
  const config = (value) =>
    ok({
      type: 'file',
      encoding: 'base64',
      content: Buffer.from(JSON.stringify(value)).toString('base64'),
    })
  const read = (command, args) => {
    if (trial && command === 'boss' && args[0] === 'show')
      return trial.session === null
        ? fail()
        : ok({ session: trial.session ?? { is_verify_only: true, policy_ref: 'dev-config' } })
    if (trial && command === 'git' && args[0] === 'ls-remote')
      return trial.missing
        ? { ok: false, exit: 2, output: '' }
        : { ok: true, exit: 0, output: `${TRIAL}\trefs/heads/dev-config` }
    if (command !== 'gh') return fail()
    if (trial && args.includes(`repos/o/r/contents/.boss-skills.json?ref=${TRIAL}`))
      return config(trial.config ?? baseConfig)
    if (args[0] === 'pr')
      return ok({
        state: 'OPEN',
        number: 7,
        url: PR_URL,
        title: 'feat: a thing',
        body: 'Tracker issue: https://linear.app/acme/issue/ABC-1/thing',
        isDraft: false,
        additions: 10,
        deletions: 2,
        headRefOid: HEAD,
        baseRefName: 'main',
        mergeStateStatus: 'CLEAN',
        statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'SUCCESS' }],
      })
    const path = args.find((arg) => /^repos\/|^graphql$/.test(arg)) ?? args[1]
    if (path === 'graphql')
      return ok({
        data: {
          repository: { pullRequest: { reviews: { nodes: [] }, timelineItems: { nodes: [] } } },
        },
      })
    if (/\/pulls\/7\/files/.test(path)) return ok([paths.map((filename) => ({ filename }))])
    // The verify judge already ran on this head and raised nothing (`W10` is `[]`).
    if (path.includes('/comments?'))
      return ok([
        [
          {
            id: 1,
            user: { login: 'judge-fixture' },
            author_association: 'MEMBER',
            created_at: '2026-10-01T00:00:00Z',
            body: `<!-- boss-judge-record head=${HEAD} areas=W10 -->`,
          },
        ],
      ])
    if (path === 'repos/o/r/commits/main') return ok({ sha: BASE })
    if (path.endsWith('/permission')) return ok({ permission: 'admin' })
    if (path === `repos/o/r/contents/.boss-skills.json?ref=${BASE}`)
      return ok({
        type: 'file',
        encoding: 'base64',
        content: Buffer.from(JSON.stringify(baseConfig)).toString('base64'),
      })
    if (path.includes('/contents/')) return { ...fail(), payload: { status: '404' } }
    if (path.includes('/statuses'))
      return ok([
        [
          ...(verify
            ? [{ context: 'boss/verify', updated_at: '2026-10-01T12:00:00Z', ...verify }]
            : []),
          { context: 'boss/build', state: 'success', updated_at: '2026-10-01T00:00:00Z' },
        ],
      ])
    if (path.includes('actions/runs?'))
      return ok([
        {
          workflow_runs: path.includes(HEAD)
            ? [{ name: 'CI', status: 'completed', conclusion: 'success', head_sha: HEAD }]
            : [],
        },
      ])
    if (path.includes('/check-runs'))
      return ok([
        {
          total_count: 1,
          check_runs: [{ name: 'ci', status: 'completed', conclusion: 'success' }],
        },
      ])
    if (path.endsWith('/status')) return ok({ statuses: [] })
    return fail()
  }
  return {
    read,
    now: () => Date.parse('2026-10-02T00:00:00Z'),
    discover: () => ({ extensions: [], skipped: [] }),
    trustedTree: ({ baseSha }) => {
      trees.push(baseSha)
      return { ok: true, root: '/trusted' }
    },
    trees,
    treeAreaFacts: pathParkFacts,
    loadConfig: () => headConfig,
    exists: () => false,
    ...(trial ? { sessionId: 'sess-trial', runDir: null } : {}),
  }
}

const AUTO_HIGH = { verifyDefaults: { merge: { auto: [{ level: 'high' }] } } }

test('judge: a trial session reads policy and extensions from origin/<ref> and parks what it would merge', async () => {
  const plain = await judge(
    { pr: 7, repo: 'o/r' },
    fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH }),
  )
  assert.deepEqual(outcome(plain), ['pass', 'verified'])
  assert.equal(plain.trialPolicy, undefined)

  const trialPass = fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH, trial: {} })
  const parked = await judge({ pr: 7, repo: 'o/r' }, trialPass)
  assert.deepEqual(outcome(parked), ['human', 'trial-policy'])
  assert.deepEqual(parked.reasons, ['trial-policy'])
  assert.equal(parked.label, 'trial policy dev-config@ccccccc')
  assert.deepEqual(parked.trialOf, { verdict: 'pass', reason: 'verified' })
  assert.deepEqual(parked.trialPolicy, { ref: 'dev-config', sha: TRIAL })
  assert.deepEqual(
    trialPass.trees,
    [TRIAL],
    'the trusted tree is the trial commit, never the base tip',
  )

  // The trial ref's own policy decides: a trial config with no merge rule holds a high head.
  const stricter = fixture({
    paths: ['src/a.go'],
    baseConfig: AUTO_HIGH,
    trial: { config: { verifyDefaults: {} } },
  })
  const held = await judge({ pr: 7, repo: 'o/r' }, stricter)
  assert.deepEqual(outcome(held), ['human', 'trial-policy'])
  assert.deepEqual(held.trialOf, { verdict: 'human', reason: 'human-merge' })
  // An approval of the park waives nothing in a trial session: it parks again.
  const waived = await judge({ pr: 7, repo: 'o/r', waive: 'trial-policy' }, stricter)
  assert.deepEqual(outcome(waived), ['human', 'trial-policy'])
})

// A standing park was raised under base policy; held, it would stop the trial's own judge running.
test('judge: a standing base-policy park holds an ordinary judge but never a trial', async () => {
  const park = { state: 'pending', description: 'needs human: worktree-diverged' }
  const held = await judge(
    { pr: 7, repo: 'o/r' },
    fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH, verify: park }),
  )
  assert.deepEqual([...outcome(held), held.held], ['human', 'worktree-diverged', true])
  const trial = await judge(
    { pr: 7, repo: 'o/r' },
    fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH, verify: park, trial: {} }),
  )
  assert.deepEqual(outcome(trial), ['human', 'trial-policy'])
  assert.equal(trial.held, undefined)
  assert.deepEqual(trial.trialOf, { verdict: 'pass', reason: 'verified' })
})

test('judge: a trial ref origin does not have is refused, and an unreadable session waits', async () => {
  const missing = fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH, trial: { missing: true } })
  await assert.rejects(
    judge({ pr: 7, repo: 'o/r' }, missing),
    /"dev-config" is not a branch on origin/,
  )
  assert.deepEqual(missing.trees, [], 'a refused ref materializes no tree')

  const unreadable = fixture({
    paths: ['src/a.go'],
    baseConfig: AUTO_HIGH,
    trial: { session: null },
  })
  assert.deepEqual(outcome(await judge({ pr: 7, repo: 'o/r' }, unreadable)), [
    'wait',
    'policy-ref-unreadable',
  ])
  const stale = fixture({
    paths: ['src/a.go'],
    baseConfig: AUTO_HIGH,
    trial: { session: { is_verify_only: true } },
  })
  assert.deepEqual(outcome(await judge({ pr: 7, repo: 'o/r' }, stale)), [
    'wait',
    'policy-ref-unreadable',
  ])
})

test('post and merge: a trial session never posts a pass or merges', async () => {
  const deps = fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH, trial: {} })
  for (const verdict of ['pass', 'defect', 'blocked'])
    assert.deepEqual(await post({ pr: 7, repo: 'o/r', head: HEAD, verdict }, deps), {
      verdict,
      posted: false,
      reason: 'trial-policy',
    })
  const merged = await merge({ pr: 7, repo: 'o/r', head: HEAD }, deps)
  assert.equal(merged.merged, false)
  assert.deepEqual([merged.verdict, merged.reason], ['human', 'trial-policy'])
  assert.deepEqual(merged.writes, [])
  const unknown = fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH, trial: { session: null } })
  const waited = await merge({ pr: 7, repo: 'o/r', head: HEAD }, unknown)
  assert.deepEqual(
    [waited.merged, waited.verdict, waited.reason],
    [false, 'wait', 'policy-ref-unreadable'],
  )
})

test("post: a trial park's policy record never matches the base policy, so the sweep re-judges it", async () => {
  const adapter = {
    applyIssueWrites: async () => ({ ok: true, outcome: 'ok', applied: [], issue: { id: 'u' } }),
  }
  const recorded = async (trial) => {
    const deps = { ...fixture({ paths: ['src/a.go'], baseConfig: AUTO_HIGH, trial }), adapter }
    const r = await post(
      { pr: 7, repo: 'o/r', head: HEAD, verdict: 'human', reason: 'human-merge', dryRun: true },
      deps,
    )
    assert.equal(r.posted, true)
    const bodies = r.writes.map((w) => String(w.body ?? ''))
    return bodies.map((b) => b.match(/boss-policy-record head=\w+ policy=(\w+)/)?.[1]).find(Boolean)
  }
  const base = await recorded(null)
  const trial = await recorded({})
  assert.match(base, /^[0-9a-f]{16}$/)
  assert.match(trial, /^[0-9a-f]{16}$/)
  assert.notEqual(trial, base)
})

test('judge: a base tip with no merge block parks a high head human-merge', async () => {
  const r = await judge(
    { pr: 7, repo: 'o/r' },
    fixture({ paths: ['src/a.go'], baseConfig: { verifyDefaults: {} } }),
  )
  assert.equal(r.confidence?.level, 'high')
  assert.deepEqual(outcome(r), ['human', 'human-merge'])
})

test('judge: a PR-branch edit to the merge rules has no effect', async () => {
  const base = { verifyDefaults: { merge: { auto: [{ level: 'high' }] } } }
  const head = { verifyDefaults: { merge: { auto: [{ level: 'high' }, { level: 'moderate' }] } } }
  const deps = fixture({ paths: ['main.tf'], baseConfig: base, headConfig: head })
  const r = await judge({ pr: 7, repo: 'o/r' }, deps)
  assert.equal(r.confidence?.level, 'moderate')
  assert.deepEqual(outcome(r), ['human', 'human-review:infra'])
  const granted = fixture({ paths: ['main.tf'], baseConfig: head })
  assert.deepEqual(outcome(await judge({ pr: 7, repo: 'o/r' }, granted)), ['pass', 'verified'])
})

test('judge: merge rules never cover an edit to the policy surface, even with no review globs', async () => {
  const base = { verifyDefaults: { merge: { auto: [{ level: 'moderate' }, { level: 'low' }] } } }
  const r = await judge(
    { pr: 7, repo: 'o/r' },
    fixture({ paths: ['.boss-skills.json'], baseConfig: base }),
  )
  assert.equal(r.facts.policySurfaceHit, true)
  assert.equal(r.verdict, 'human')
  const none = await judge(
    { pr: 7, repo: 'o/r' },
    fixture({ paths: ['.boss-skills.json'], baseConfig: { verifyDefaults: {} } }),
  )
  assert.equal(none.facts.policySurfaceHit, false)
})

test('judge explains breaking and failed describers over an additive schema diff', async () => {
  for (const failed of [false, true]) {
    const deps = fixture({
      paths: ['api/x.proto'],
      baseConfig: {
        verifyDefaults: { merge: { auto: [{ area: 'api-contract', change: 'additive' }] } },
      },
    })
    deps.treeAreaFacts = (files, policy) => {
      const facts = pathParkFacts(files, policy)
      const outcome = {
        extension: 'contract-check',
        area: 'api-contract',
        ok: !failed,
        ...(failed
          ? { reason: 'timeout' }
          : {
              description: {
                items: ['Removed credential field'],
                links: [],
                breaking: true,
              },
            }),
      }
      return {
        ...facts,
        areas: describeAreas(
          facts.areas.map((area) => ({ ...area, change: 'additive' })),
          [outcome],
        ).areas,
      }
    }
    const result = await judge({ pr: 7, repo: 'o/r' }, deps)
    assert.ok(result.reasons.includes('human-review:api-contract'))
    const note = parkNote({
      codes: result.reasons,
      headSha: HEAD,
      facts: { confidence: result.confidence },
    }).body
    assert.match(note, /contract-check/)
    assert.match(note, failed ? /failed.*timeout/ : /breaking.*Removed credential field/)
  }
})
