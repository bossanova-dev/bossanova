import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const helper = fileURLToPath(new URL('./verification-receipt.mjs', import.meta.url))
async function fixture(t) {
  const cwd = await mkdtemp(path.join(tmpdir(), 'verification-receipt-'))
  t.after(() => rm(cwd, { recursive: true, force: true, maxRetries: 3 }))
  assert.equal(spawnSync('git', ['init', '--quiet'], { cwd }).status, 0)
  assert.equal(spawnSync('git', ['config', 'commit.gpgsign', 'false'], { cwd }).status, 0)
  const call = (...args) =>
    spawnSync(process.execPath, [helper, ...args], { cwd, encoding: 'utf8' })
  const receipt = (id, dispatch = 'task-1') =>
    path.join(cwd, '.git/boss-verification', dispatch, `${id}.json`)
  return { cwd, call, receipt }
}
const command = (source) => [process.execPath, '-e', source]

test('run exposes pending then persists terminal output without dirtying the tree', async (t) => {
  const f = await fixture(t)
  const result = f.call(
    'run',
    '--dispatch',
    'task-1',
    '--id',
    'test',
    '--',
    ...command(
      `const fs=require('node:fs'); if(JSON.parse(fs.readFileSync(process.env.BOSS_VERIFICATION_RECEIPT)).status!=='pending')process.exit(4); console.log('passed'); console.error('diagnostic')`,
    ),
  )
  assert.equal(result.status, 0, result.stderr)
  const receipt = JSON.parse(await readFile(f.receipt('test'), 'utf8'))
  assert.equal(receipt.status, 'pass')
  assert.equal(receipt.exitCode, 0)
  assert.match(receipt.summary, /passed/)
  assert.match(await readFile(receipt.logPath, 'utf8'), /diagnostic/)
  assert.equal(
    spawnSync('git', ['status', '--porcelain'], { cwd: f.cwd, encoding: 'utf8' }).stdout,
    '',
  )
  assert.equal(f.call('check', '--dispatch', 'task-1', '--id', 'test').status, 0)
})
test('run preserves failing exits and spawn errors', async (t) => {
  const f = await fixture(t)
  assert.equal(
    f.call('run', '--dispatch', 'task-1', '--id', 'fail', '--', ...command('process.exit(3)'))
      .status,
    3,
  )
  assert.equal(f.call('check', '--dispatch', 'task-1', '--id', 'fail').status, 1)
  assert.equal(
    f.call('run', '--dispatch', 'task-1', '--id', 'missing', '--', '/no-such-receipt-command')
      .status,
    127,
  )
})
async function pending(f, t) {
  const child = spawn(
    process.execPath,
    [
      helper,
      'run',
      '--dispatch',
      'task-1',
      '--id',
      'slow',
      '--',
      ...command('setTimeout(()=>console.log("done"), 1200)'),
    ],
    { cwd: f.cwd, stdio: 'ignore' },
  )
  t.after(() => {
    if (child.exitCode === null) child.kill('SIGKILL')
  })
  const ended = new Promise((resolve) => child.on('exit', resolve))
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      await readFile(f.receipt('slow'))
      return { child, ended }
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 10))
    }
  }
  throw new Error('pending receipt never appeared')
}
test('killed wrapper leaves orphaned evidence', async (t) => {
  const f = await fixture(t)
  const { child, ended } = await pending(f, t)
  child.kill('SIGKILL')
  await ended
  const result = f.call('check', '--dispatch', 'task-1', '--id', 'slow')
  assert.equal(result.status, 2)
  assert.equal(JSON.parse(result.stdout).receipts[0].status, 'orphaned')
})
test('live and unreported pending receipts block; bounded wait collects the result', async (t) => {
  const f = await fixture(t)
  f.call('run', '--dispatch', 'task-1', '--id', 'passed', '--', ...command(''))
  const { ended } = await pending(f, t)
  const result = f.call('check', '--dispatch', 'task-1', '--id', 'passed')
  assert.equal(result.status, 2)
  assert.equal(
    JSON.parse(result.stdout).receipts.find((receipt) => receipt.id === 'slow').status,
    'pending',
  )
  assert.equal(
    f.call('check', '--dispatch', 'task-1', '--id', 'slow', '--wait-ms', '5000').status,
    0,
  )
  await ended
})
test('absent and unreadable receipts are incomplete; clear is isolated and idempotent', async (t) => {
  const f = await fixture(t)
  const result = f.call('check', '--dispatch', 'task-1', '--id', 'absent')
  assert.equal(result.status, 2)
  assert.equal(JSON.parse(result.stdout).receipts[0].status, 'absent')
  f.call('run', '--dispatch', 'task-1', '--id', 'bad', '--', ...command(''))
  await writeFile(f.receipt('bad'), '{')
  assert.equal(
    JSON.parse(f.call('check', '--dispatch', 'task-1').stdout).incomplete[0].reason,
    'unreadable',
  )
  f.call('run', '--dispatch', 'other', '--id', 'kept', '--', ...command(''))
  assert.equal(f.call('clear', '--dispatch', 'task-1').status, 0)
  assert.equal(f.call('clear', '--dispatch', 'task-1').status, 0)
  assert.equal(f.call('check', '--dispatch', 'other', '--id', 'kept').status, 0)
  const removed = f.call('check', '--dispatch', 'task-1', '--id', 'bad')
  assert.equal(removed.status, 2)
  assert.equal(JSON.parse(removed.stdout).receipts[0].status, 'absent')
})
test('unsafe identifiers and malformed flags are usage errors and write nothing', async (t) => {
  const f = await fixture(t)
  for (const value of ['../x', 'Upper', '']) {
    assert.equal(f.call('clear', '--dispatch', value).status, 64)
    assert.equal(f.call('check', '--dispatch', 'safe', '--id', value).status, 64)
  }
  assert.equal(f.call('check', '--dispatch', 'safe', '--wait-ms', '-1').status, 64)
  assert.equal(f.call('check', '--dispatch', 'safe', '--unknown').status, 64)
})

async function trackedFixture(t) {
  const f = await fixture(t)
  await writeFile(path.join(f.cwd, 'tracked'), 'original\n')
  await writeFile(path.join(f.cwd, '.gitignore'), 'ignored\n')
  git(f, 'add', 'tracked', '.gitignore')
  git(f, '-c', 'user.name=Test', '-c', 'user.email=test@example.com', 'commit', '-qm', 'initial')
  return f
}
function git(f, ...args) {
  const result = spawnSync('git', args, { cwd: f.cwd, encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr)
  return result.stdout.trim()
}
function gate(f, source = '', flags = []) {
  return f.call('run', '--dispatch', 'task-1', '--id', 'fresh', ...flags, '--', ...command(source))
}
function checked(f) {
  const result = f.call('check', '--dispatch', 'task-1', '--id', 'fresh')
  return { code: result.status, ...JSON.parse(result.stdout) }
}
test('fresh evidence records content and HEAD without modifying the real index', async (t) => {
  const f = await trackedFixture(t)
  const index = path.join(f.cwd, '.git/index')
  const before = await readFile(index)
  assert.equal(gate(f).status, 0)
  const result = checked(f)
  assert.equal(result.code, 0)
  const receipt = result.receipts[0]
  assert.equal(receipt.version, 2)
  assert.equal(receipt.head, git(f, 'rev-parse', 'HEAD'))
  assert.equal(receipt.writes, false)
  assert.match(receipt.startTree, /^[a-f0-9]{40}$/)
  assert.equal(receipt.startTree, receipt.finishTree)
  assert.deepEqual(await readFile(index), before)
  assert.equal(git(f, 'status', '--porcelain'), '')
  assert.deepEqual((await readdir(path.dirname(f.receipt('fresh')))).sort(), [
    'fresh.json',
    'fresh.log',
  ])
})
test('a gate writing tracked content is stale unless explicitly declared a writer', async (t) => {
  for (const writes of [false, true]) {
    const f = await trackedFixture(t)
    assert.equal(
      gate(f, "require('node:fs').writeFileSync('tracked', 'changed')", writes ? ['--writes'] : [])
        .status,
      0,
    )
    const result = checked(f)
    assert.equal(result.code, writes ? 0 : 2)
    assert.equal(result.receipts[0].status, writes ? 'pass' : 'stale')
    if (!writes) {
      assert.equal(result.incomplete[0].reason, 'changed-during-run')
      assert.equal(result.receipts[0].recordedStatus, 'pass')
      assert.equal(result.receipts[0].exitCode, 0)
    }
  }
})
test('tracked and untracked edits after a gate invalidate evidence, including failures and writers', async (t) => {
  for (const [source, file, flags] of [
    ['', 'tracked', []],
    ['', 'new', []],
    ['process.exit(3)', 'tracked', []],
    ['', 'tracked', ['--writes']],
  ]) {
    const f = await trackedFixture(t)
    assert.equal(gate(f, source, flags).status, source ? 3 : 0)
    await writeFile(path.join(f.cwd, file), 'changed')
    const result = checked(f)
    assert.equal(result.code, 2)
    assert.equal(result.receipts[0].status, 'stale')
    assert.equal(result.incomplete[0].reason, 'changed-after-run')
    assert.equal(result.receipts[0].exitCode, source ? 3 : 0)
    assert.equal(gate(f).status, 0)
    assert.equal(checked(f).code, 0)
  }
})
test('ignored edits and committing identical content preserve freshness', async (t) => {
  const f = await trackedFixture(t)
  await writeFile(path.join(f.cwd, 'tracked'), 'pending commit')
  assert.equal(gate(f).status, 0)
  await writeFile(path.join(f.cwd, 'ignored'), 'not part of tree')
  assert.equal(checked(f).code, 0)
  git(f, 'add', 'tracked')
  git(
    f,
    '-c',
    'user.name=Test',
    '-c',
    'user.email=test@example.com',
    'commit',
    '-qm',
    'same content',
  )
  assert.equal(checked(f).code, 0)
})
test('unborn HEAD has null head and still detects content changes', async (t) => {
  const f = await fixture(t)
  assert.equal(gate(f).status, 0)
  assert.equal(checked(f).receipts[0].head, null)
  assert.equal(checked(f).code, 0)
  await writeFile(path.join(f.cwd, 'new'), 'untracked')
  assert.equal(checked(f).incomplete[0].reason, 'changed-after-run')
})
test('legacy terminal evidence remains usable and missing finish evidence is stale', async (t) => {
  const f = await trackedFixture(t)
  assert.equal(gate(f).status, 0)
  const receipt = JSON.parse(await readFile(f.receipt('fresh'), 'utf8'))
  await writeFile(f.receipt('fresh'), JSON.stringify({ ...receipt, finishTree: null }))
  assert.equal(checked(f).incomplete[0].reason, 'unfingerprinted')
  for (const exitCode of [0, 1]) {
    const legacy = { ...receipt, version: 1, status: exitCode ? 'fail' : 'pass', exitCode }
    for (const key of ['head', 'startTree', 'finishTree', 'writes']) delete legacy[key]
    await writeFile(f.receipt('fresh'), JSON.stringify(legacy))
    const result = checked(f)
    assert.equal(result.code, exitCode)
    assert.equal(result.receipts[0].status, exitCode ? 'fail' : 'pass')
  }
})
test('writes flag is rejected outside run without creating receipt state', async (t) => {
  const f = await fixture(t)
  for (const verb of ['check', 'clear'])
    assert.equal(f.call(verb, '--dispatch', 'task-1', '--writes').status, 64)
  await assert.rejects(readdir(path.dirname(f.receipt('fresh'))), { code: 'ENOENT' })
})
