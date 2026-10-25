import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { loadSkillConfig, validateLocalVerification } from '../skill-config.mjs'
import { classify, retry } from './acceptance-retry.mjs'

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
