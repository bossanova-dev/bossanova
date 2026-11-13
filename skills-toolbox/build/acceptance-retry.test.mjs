import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { loadSkillConfig, validateLocalVerification } from '../skill-config.mjs'
import { fileStore } from '../ci-wait.mjs'
import { certify, classify, retry } from './acceptance-retry.mjs'

// Producer shapes from services/boss/api/cli/skills/skill_sync.go: the downgrade
// lead, next action, inline drift line and gate rows are the public signal.
const lead =
  'reinstall withheld [downgrade] — the installed copy is newer than this checkout (it is on origin/HEAD); a reinstall from here would downgrade it:'
const label = 'boss-build/toolbox/ready-gate.mjs (content, ahead)'
const inline = `  ⚠ drift — installed skills differ from the selected payload; ${lead} ${label}; next: update this checkout to origin/HEAD, then re-run this check`
const gate = `boss skills gate: claude skill drift detected\n  - ${label}\n  ${lead}\n  next: update this checkout to origin/HEAD, then re-run this check`

for (const [name, output, status, behind, expected] of [
  ['inline downgrade', inline, 1, 2, 'rebase-retry'],
  ['gate downgrade', gate, 1, 2, 'rebase-retry'],
  [
    'make wrapper',
    `./bin/boss skills check\n${inline}\nmake: *** [skills-check] Error 1`,
    1,
    2,
    'rebase-retry',
  ],
  ['success', inline, 0, 2, 'no-retry'],
  ['lost', inline.replace('[downgrade]', '[lost]'), 1, 2, 'no-retry'],
  ['undecided', inline.replace('[downgrade]', '[undecided]'), 1, 2, 'no-retry'],
  ['runnable remedy', `${inline}; run \`boss skills install\``, 1, 2, 'no-retry'],
  [
    'binary stale',
    `${inline}\n  ⚠ drift — this running boss binary's embedded skills are behind checkout source`,
    1,
    2,
    'no-retry',
  ],
  ['mixed gate', `${gate}\n  - boss-plan/SKILL.md (content, behind)`, 1, 2, 'no-retry'],
  ['not behind', inline, 1, 0, 'no-retry'],
  ['unknown behind', inline, 1, 'unevaluated', 'no-retry'],
  ['test failure', 'FAIL TestSomething\nnot ok 1', 1, 2, 'no-retry'],
  ['empty', '', 1, 2, 'no-retry'],
  ['unable check', `${inline}\nunable to check installed skills`, 1, 2, 'no-retry'],
  ['empty labels', inline.replace(label, ''), 1, 2, 'no-retry'],
  [
    'second drift real',
    `${inline}\n  ⚠ drift — installed skills differ from the selected payload; run \`boss skills install\``,
    1,
    2,
    'no-retry',
  ],
]) {
  test(`classify ${name}`, () => {
    const result = classify({ status, output, base: 'origin/main', count: () => behind })
    assert.equal(result.verdict, expected)
    assert.equal(result.signal, expected === 'rebase-retry' ? 'skills-checkout-behind' : null)
  })
}

const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' }
function git(repo, ...args) {
  const result = spawnSync('git', args, { cwd: repo, env, encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr)
  return result.stdout.trim()
}
function fixture(t, conflict = false) {
  const root = tempDir('acceptance-retry-test-', t)
  git(root, 'init', '--bare', 'origin.git')
  git(root, 'init', '-b', 'main', 'repo')
  const repo = join(root, 'repo')
  git(repo, 'config', 'user.name', 'Test')
  git(repo, 'config', 'user.email', 'test@example.invalid')
  git(repo, 'config', 'commit.gpgsign', 'false')
  git(repo, 'config', 'core.hooksPath', '/dev/null')
  writeFileSync(join(repo, 'common.txt'), 'initial\n')
  git(repo, 'add', 'common.txt')
  git(repo, 'commit', '-m', 'initial')
  git(repo, 'remote', 'add', 'origin', join(root, 'origin.git'))
  git(repo, 'push', 'origin', 'main')
  git(repo, 'checkout', '-b', 'feature')
  writeFileSync(join(repo, conflict ? 'common.txt' : 'feature.txt'), 'feature\n')
  git(repo, 'add', '.')
  git(repo, 'commit', '-m', 'feature')
  git(repo, 'checkout', 'main')
  writeFileSync(join(repo, conflict ? 'common.txt' : 'from-base.txt'), 'base\n')
  git(repo, 'add', '.')
  git(repo, 'commit', '-m', 'base')
  git(repo, 'push', 'origin', 'main')
  git(repo, 'checkout', 'feature')
  return { repo, root }
}
function snapshot(repo) {
  return [
    git(repo, 'rev-parse', 'HEAD'),
    git(repo, 'branch', '--show-current'),
    git(repo, 'status', '--porcelain=v1'),
    git(repo, 'ls-files', '--stage'),
    git(repo, 'worktree', 'list', '--porcelain'),
  ]
}
for (const scenario of ['pass', 'fail', 'conflict', 'fetch', 'timeout']) {
  test(`retry ${scenario} preserves caller and cleans scratch`, async (t) => {
    const { repo, root } = fixture(t, scenario === 'conflict')
    // Preserve staged as well as unstaged caller work.
    writeFileSync(join(repo, 'local.txt'), 'caller\n')
    git(repo, 'add', 'local.txt')
    const before = snapshot(repo)
    const entries = readdirSync(root).sort()
    const command =
      scenario === 'timeout' ? 'sleep 5' : scenario === 'fail' ? 'exit 7' : 'test -f from-base.txt'
    const outputFile = join(root, 'output.log')
    const result = await retry({
      repo,
      base: scenario === 'fetch' ? 'missing/main' : 'origin/main',
      command,
      timeoutMs: scenario === 'timeout' ? 50 : 10000,
      outputFile,
      tmpRoot: root,
    })
    const expected = {
      pass: 'pass',
      fail: 'fail',
      conflict: 'rebase-conflict',
      fetch: 'unevaluated',
      timeout: 'unevaluated',
    }[scenario]
    assert.equal(result.result, expected, JSON.stringify(result))
    assert.equal(result.met, scenario === 'pass')
    assert.equal(result.head, before[0])
    assert.deepEqual(snapshot(repo), before)
    assert.deepEqual(
      readdirSync(root)
        .filter((name) => name !== 'output.log')
        .sort(),
      entries,
    )
    if (scenario !== 'fetch') assert.equal(result.baseSha, git(repo, 'rev-parse', 'origin/main'))
    if (scenario === 'pass') {
      assert.ok(result.rebasedHead)
      assert.equal(result.status, 0)
      assert.equal(
        validateLocalVerification(
          loadSkillConfig(),
          `## Local verification\n- ${result.evidence}\n`,
        ).ok,
        true,
      )
      assert.ok(existsSync(outputFile))
    } else
      assert.match(
        result.reason,
        scenario === 'fail' ? /origin\/main@[0-9a-f]+.*exit 7/ : /fetch-failed|timeout|conflict/,
      )
  })
}

test('retry streams command output without a capture buffer', async (t) => {
  const { repo, root } = fixture(t)
  const outputFile = join(root, 'stream.log')
  const result = await retry({
    repo,
    base: 'origin/main',
    command: `printf 'command output'`,
    outputFile,
    tmpRoot: root,
  })
  assert.equal(result.result, 'pass')
  assert.equal(readFileSync(outputFile, 'utf8'), 'command output')
})

const helper = fileURLToPath(new URL('./acceptance-retry.mjs', import.meta.url))
test('CLI fails closed on unreadable input and rejects invalid arguments', (t) => {
  const root = tempDir('acceptance-cli-test-', t)
  for (const [args, status] of [
    [
      [
        'classify',
        '--status',
        '1',
        '--output-file',
        join(root, 'missing'),
        '--base',
        'origin/main',
      ],
      3,
    ],
    [['classify', '--status', 'bad', '--output-file', 'missing', '--base', 'origin/main'], 2],
    [['retry', '--command', 'true', '--base', 'origin/main', '--timeout-ms', '0'], 2],
    [['retry', '--command', 'true', '--base', 'origin/main', '--unknown'], 2],
  ]) {
    const result = spawnSync(process.execPath, [helper, ...args], { encoding: 'utf8' })
    assert.equal(result.status, status, result.stderr)
    if (status === 3) assert.equal(JSON.parse(result.stdout).verdict, 'no-retry')
  }
})

const planText = `## Acceptance criteria
- [ ] passes — check: \`printf passed\`
- [ ] fails — check: \`exit 7\`
- [ ] needs a human
- [ ] (verify-only) runnable — check: \`true\`
\`\`\`md
- [ ] fenced — check: \`false\`
\`\`\`
## Original notes
- [ ] history — check: \`false\`
`
function certificationFixture(t, text = planText) {
  const { repo, root } = fixture(t)
  const plan = join(root, 'plan.md')
  writeFileSync(plan, text)
  return { repo, root, plan, base: 'origin/main', out: join(root, 'acceptance.json') }
}

test('certify reports every active criterion and valid evidence with real commands', async (t) => {
  const opts = certificationFixture(t)
  const result = await certify(opts)
  assert.deepEqual(
    result.criteria.map((c) => [c.id, c.result]),
    [
      ['AC1', 'met'],
      ['AC2', 'unmet'],
      ['AC3', 'unchecked'],
      ['AC4', 'met'],
    ],
  )
  assert.equal(result.criteria[1].exit, 7)
  assert.equal(result.criteria[2].reason, 'no-check')
  assert.equal(result.criteria[3].verifyOnly, true)
  for (const c of result.criteria.filter((c) => c.result === 'met'))
    assert.equal(
      validateLocalVerification(loadSkillConfig(), `## Local verification\n- ${c.evidence}\n`).ok,
      true,
    )
  assert.equal(readFileSync(result.criteria[0].outputFile, 'utf8'), 'passed')
})

for (const retried of ['pass', 'fail'])
  test(`certify folds retry ${retried} into the criterion`, async (t) => {
    const opts = certificationFixture(t, '## Acceptance criteria\n- [ ] check — check: `false`\n')
    const result = await certify({
      ...opts,
      classify: () => ({ verdict: 'rebase-retry' }),
      retry: async () => ({
        result: retried,
        reason: 'retry result',
        evidence: 'rebased-retry: `false` → pass in scratch',
      }),
    })
    assert.equal(result.criteria[0].result, retried === 'pass' ? 'met' : 'unmet')
    assert.equal(result.criteria[0].retried, true)
    assert.equal(result.criteria[0].reason, 'retry result')
  })

test('certify honours timeout, output cap and overall budget', async (t) => {
  const opts = certificationFixture(t)
  let clock = 0
  const report = await certify({
    ...opts,
    budgetMs: 10,
    now: () => clock,
    execute: async ({ outputFile }) => {
      writeFileSync(outputFile, 'x'.repeat(4000))
      clock = 10
      return { status: null, timedOut: true }
    },
  })
  assert.equal(report.criteria[0].result, 'unmet')
  assert.equal(report.criteria[0].timedOut, true)
  assert.ok(Buffer.byteLength(report.criteria[0].tail) <= 1500)
  assert.equal(readFileSync(report.criteria[0].outputFile, 'utf8').length, 4000)
  assert.deepEqual(
    report.criteria.slice(1).map((c) => c.reason),
    ['budget-spent', 'no-check', 'budget-spent'],
  )
})

test('certification passes share an absolute deadline', async (t) => {
  const opts = certificationFixture(t, '## Acceptance criteria\n- [ ] check — check: `true`\n')
  let clock = 100
  const timeouts = []
  const execute = async ({ outputFile, timeoutMs }) => {
    timeouts.push(timeoutMs)
    writeFileSync(outputFile, 'passed')
    clock += 6
    return { status: 0 }
  }
  const shared = { ...opts, deadlineMs: 110, budgetMs: 100, now: () => clock, execute }
  assert.equal((await certify(shared)).criteria[0].result, 'met')
  assert.equal((await certify(shared)).criteria[0].result, 'met')
  const expired = await certify(shared)
  assert.equal(expired.criteria[0].result, 'unchecked')
  assert.equal(expired.criteria[0].reason, 'budget-spent')
  assert.deepEqual(timeouts, [10, 4])
})

test('certify reuses only matching trees and criteria, including message-only amendments', async (t) => {
  const opts = certificationFixture(t)
  const a = await certify(opts)
  const reuse = join(opts.root, 'verdict.json')
  writeFileSync(reuse, JSON.stringify({ acceptance: a }))
  const mustNotRun = async () => {
    assert.fail('reused certification ran a command')
  }
  assert.equal((await certify({ ...opts, reuse, execute: mustNotRun })).reused, true)
  git(opts.repo, 'commit', '--amend', '-m', 'new message')
  assert.equal((await certify({ ...opts, reuse, execute: mustNotRun })).reused, true)
  writeFileSync(join(opts.repo, 'feature.txt'), 'new tree')
  git(opts.repo, 'add', 'feature.txt')
  git(opts.repo, 'commit', '-m', 'changed tree')
  assert.equal((await certify({ ...opts, reuse })).reused, undefined)
  writeFileSync(
    reuse,
    JSON.stringify({ acceptance: { ...a, tree: git(opts.repo, 'rev-parse', 'HEAD^{tree}') } }),
  )
  writeFileSync(opts.plan, '## Acceptance criteria\n- [ ] changed — check: `true`\n')
  assert.equal((await certify({ ...opts, reuse })).reused, undefined)
})

test('certify CLI rejects missing plans and malformed bases without output, and accepts empty plans', (t) => {
  const opts = certificationFixture(t, '# No criteria\n')
  for (const [plan, base, status] of [
    [join(opts.root, 'missing'), 'origin/main', 2],
    [opts.plan, 'main', 2],
    [opts.plan, 'origin/main', 0],
  ]) {
    const result = spawnSync(
      process.execPath,
      [helper, 'certify', '--plan', plan, '--base', base, '--repo', opts.repo, '--out', opts.out],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, status, result.stderr)
    if (status === 2) assert.equal(existsSync(opts.out), false)
    else assert.deepEqual(JSON.parse(readFileSync(opts.out, 'utf8')).criteria, [])
  }
})

// Commit `files` ({path: content}) on the fixture's feature branch.
function seed(repo, files) {
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(dirname(join(repo, path)), { recursive: true })
    writeFileSync(join(repo, path), content)
  }
  git(repo, 'add', '.')
  git(repo, 'commit', '-m', 'seed')
}
// Plain scripts, not node:test files: a nested `node --test` reports to this runner, not its exit.
const passingTest = 'process.exitCode = 0\n'
const failingTest = 'process.exitCode = 1\n'
const derivedConfig = JSON.stringify({
  acceptance: { derivedChecks: [{ glob: '**/*.test.mjs', command: 'node {file}' }] },
})
const namedTestPlan = `## Acceptance criteria
- [ ] the helper works (\`helper.test.mjs\`)
- [ ] the nested helper works — test in \`pkg/nested.test.mjs\`.
- [ ] the broken helper works (\`broken.test.mjs\`)
- [ ] an ambiguous name proves nothing (\`dup.test.mjs\`)
- [ ] prose only
`

test('certify derives a configured check from a criterion that names its test file', async (t) => {
  const opts = certificationFixture(t, namedTestPlan)
  seed(opts.repo, {
    '.boss-skills.json': derivedConfig,
    'tests/helper.test.mjs': passingTest,
    'pkg/nested.test.mjs': passingTest,
    'tests/broken.test.mjs': failingTest,
    'a/dup.test.mjs': passingTest,
    'b/dup.test.mjs': passingTest,
  })
  const result = await certify(opts)
  assert.deepEqual(
    result.criteria.map((c) => [c.result, c.check, c.derivedCheck ?? null, c.reason]),
    [
      ['met', null, 'node tests/helper.test.mjs', ''],
      ['met', null, 'node pkg/nested.test.mjs', ''],
      ['unmet', null, 'node tests/broken.test.mjs', 'exit 1'],
      ['unchecked', null, null, 'no-check'],
      ['unchecked', null, null, 'no-check'],
    ],
  )
  for (const c of result.criteria.filter((c) => c.result === 'met'))
    assert.equal(
      validateLocalVerification(loadSkillConfig(), `## Local verification\n- ${c.evidence}\n`).ok,
      true,
      c.evidence,
    )
})

test('certify derives nothing without a configured mapping', async (t) => {
  const opts = certificationFixture(t, namedTestPlan)
  seed(opts.repo, { 'tests/helper.test.mjs': passingTest })
  const result = await certify(opts)
  assert.deepEqual(
    result.criteria.map((c) => [c.result, c.reason]),
    Array(5).fill(['unchecked', 'no-check']),
  )
})

const fullGatePlan = `## Acceptance criteria
- [ ] Lint and the full suite pass — check: \`make lint && make test\`
- [ ] the slow probe passes — check: \`sleep 600\`
`
function recordCiWait(dir, headSha, state) {
  fileStore({ dir, pr: 12 }).save({
    version: 1,
    pr: 12,
    headSha,
    readings: [],
    lastVerdict: { state, reason: 'ok', atMs: 1, trend: 'converging' },
  })
}
function fullGateFixture(t) {
  const opts = certificationFixture(t, fullGatePlan)
  seed(opts.repo, {
    '.boss-skills.json': JSON.stringify({ commands: { lint: 'make lint', test: 'make test' } }),
  })
  const ciWaitStateDir = join(opts.root, 'ci-wait')
  const ran = []
  const execute = async ({ command, outputFile }) => {
    ran.push(command)
    writeFileSync(outputFile, '')
    return { status: null, timedOut: true }
  }
  return { ...opts, ciWaitStateDir, execute, ran, head: git(opts.repo, 'rev-parse', 'HEAD') }
}

test('certify meets a full-gate criterion from the recorded green CI wait on the head', async (t) => {
  const opts = fullGateFixture(t)
  recordCiWait(opts.ciWaitStateDir, opts.head, 'settled')
  const result = await certify(opts)
  assert.deepEqual(
    result.criteria.map((c) => [c.result, c.reason]),
    [
      ['met', ''],
      ['unmet', 'timeout'],
    ],
  )
  assert.deepEqual(opts.ran, ['sleep 600'])
  assert.equal(result.criteria[0].satisfiedBy, 'ci-wait')
  assert.equal(
    validateLocalVerification(
      loadSkillConfig(),
      `## Local verification\n- ${result.criteria[0].evidence}\n`,
    ).ok,
    true,
  )
})

for (const [name, record] of [
  ['no recorded wait', null],
  ['a failed wait', (opts) => recordCiWait(opts.ciWaitStateDir, opts.head, 'failed')],
  [
    'a green wait on another head',
    (opts) => recordCiWait(opts.ciWaitStateDir, 'f'.repeat(40), 'settled'),
  ],
])
  test(`certify runs a full-gate criterion itself with ${name}`, async (t) => {
    const opts = fullGateFixture(t)
    record?.(opts)
    const result = await certify(opts)
    assert.deepEqual(opts.ran, ['make lint && make test', 'sleep 600'])
    assert.equal(result.criteria[0].result, 'unmet')
  })

test('a reused review block takes recorded green CI for its full-gate timeout', async (t) => {
  const opts = fullGateFixture(t)
  const reviewed = await certify(opts)
  assert.equal(reviewed.criteria[0].reason, 'timeout')
  const reuse = join(opts.root, 'verdict.json')
  writeFileSync(reuse, JSON.stringify({ acceptance: reviewed }))
  recordCiWait(opts.ciWaitStateDir, opts.head, 'settled')
  opts.ran.length = 0
  const result = await certify({ ...opts, reuse })
  assert.equal(result.reused, true)
  assert.deepEqual(opts.ran, [])
  assert.deepEqual(
    result.criteria.map((c) => c.result),
    ['met', 'unmet'],
  )
})
