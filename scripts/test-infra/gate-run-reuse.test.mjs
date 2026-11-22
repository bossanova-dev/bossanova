import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout } from 'node:timers/promises'
import test from 'node:test'
import { tempDir } from './test-tmpdir.mjs'

const runner = fileURLToPath(new URL('../gate-run.mjs', import.meta.url))
function fixture(t) {
  const dir = tempDir('gate-run-reuse-')
  const runs = []
  const waiters = []
  const state = path.join(dir, 'state')
  fs.mkdirSync(state)
  const env = {
    ...process.env,
    HOME: state,
    TMPDIR: state,
    CI: '',
    BASE_REF: 'refs/remotes/origin/main',
    BOSS_GATE_RUN_INDEX_DIR: path.join(state, 'index'),
    BOSS_GATE_REUSE: '1',
    BOSS_GATE_FORCE_UNCACHED: '',
    BOSS_GATE_REUSE_WINDOW_MS: '',
    BOSS_GATE_ADMITTED: '',
    BOSS_GATE_MAX_CONCURRENT: '0',
    BOSS_GATE_ISOLATE: '0',
    BOSS_GATE_MIN_FREE_GIB: '0',
  }
  delete env.NODE_TEST_CONTEXT
  const git = (args) => {
    const result = spawnSync('git', args, { cwd: dir, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
  }
  git(['init'])
  git(['config', 'user.name', 'Gate Test'])
  git(['config', 'user.email', 'gate@example.com'])
  git(['config', 'commit.gpgsign', 'false'])
  fs.writeFileSync(path.join(dir, '.gitignore'), 'state/\n')
  fs.writeFileSync(path.join(dir, 'input'), 'original\n')
  fs.writeFileSync(
    path.join(dir, 'Makefile'),
    `test: test-affected\ntest-affected:\n\t@"${process.execPath}" gate.cjs\ntest-race: test-affected\n`,
  )
  fs.writeFileSync(
    path.join(dir, 'gate.cjs'),
    `
const fs = require('fs');
fs.appendFileSync('state/count', 'run\\n');
if (fs.existsSync('state/change')) fs.writeFileSync('input', 'changed\\n');
const finish = () => process.exit(fs.existsSync('state/fail') ? 1 : 0);
if (fs.existsSync('state/hold')) {
  const timer = setInterval(() => { if (fs.existsSync('state/release')) { clearInterval(timer); finish(); } }, 10);
} else finish();
`,
  )
  git(['add', '.gitignore', 'input', 'Makefile', 'gate.cjs'])
  git(['commit', '-m', 'initial'])
  git(['update-ref', 'refs/remotes/origin/main', 'HEAD'])
  const call = (args, extra = {}) =>
    spawnSync(process.execPath, [runner, ...args], {
      cwd: dir,
      env: { ...env, ...extra },
      encoding: 'utf8',
      timeout: 30000,
    })
  t.after(() => {
    for (const child of waiters) child.kill()
    for (const run of runs.reverse()) call(['stop', run])
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  })
  const start = (goal = 'test-affected', extra = {}) => {
    const result = call(['start', '--', 'make', goal], extra)
    assert.equal(result.status, 0, result.stdout + result.stderr)
    const run = result.stdout.split('\n')[0]
    runs.push(run)
    return { run, result }
  }
  const wait = ({ run }) => call(['wait', '--timeout', '20000', run])
  const waitActive = async ({ run }) => {
    const hook = `const timer = globalThis.setTimeout; globalThis.setTimeout = (...args) => { process.send?.('polling'); return timer(...args); };`
    const child = spawn(
      process.execPath,
      [
        '--import',
        `data:text/javascript,${encodeURIComponent(hook)}`,
        runner,
        'wait',
        '--timeout',
        '20000',
        run,
      ],
      {
        cwd: dir,
        env,
        stdio: ['ignore', 'pipe', 'pipe', 'ipc'],
        timeout: 30000,
      },
    )
    waiters.push(child)
    let stdout = ''
    let stderr = ''
    child.stdout.on('data', (chunk) => {
      stdout += chunk
    })
    child.stderr.on('data', (chunk) => {
      stderr += chunk
    })
    const done = new Promise((resolve, reject) => {
      child.once('error', reject)
      child.once('close', (status) => resolve({ status, stdout, stderr }))
    })
    await new Promise((resolve, reject) => {
      child.once('message', resolve)
      child.once('error', reject)
      child.once('exit', () => reject(new Error('wait exited before polling')))
    })
    return { done }
  }
  const mark = (name) => fs.writeFileSync(path.join(state, name), '1')
  const count = () =>
    fs.existsSync(path.join(state, 'count'))
      ? fs.readFileSync(path.join(state, 'count'), 'utf8').trim().split('\n').length
      : 0
  return { dir, state, start, wait, waitActive, call, mark, count }
}
async function until(check) {
  const deadline = Date.now() + 20000
  while (!check()) {
    assert.ok(Date.now() < deadline, 'fixture did not reach its expected state')
    await setTimeout(10)
  }
}
function passed(result) {
  assert.equal(result.status, 0, result.stdout + result.stderr)
  assert.match(result.stdout, /^GATE PASSED\n/)
  assert.match(result.stdout.trim().split('\n').at(-1), /^evidence:/)
}

test('alias callers join one live gate and report its evidence and durable verdict', async (t) => {
  const f = fixture(t)
  f.mark('hold')
  const source = f.start()
  await until(() => f.count() === 1)
  const joined = f.start('test')
  assert.notEqual(joined.run, source.run)
  assert.equal(joined.result.stdout.split('\n')[2], path.join(source.run, 'log'))
  assert.match(joined.result.stderr, /joined live/)
  f.mark('release')
  passed(f.wait(source))
  const result = f.wait(joined)
  passed(result)
  assert.match(result.stdout, /\nreused: joined live run /)
  assert.equal(fs.readFileSync(path.join(joined.run, 'verdict'), 'utf8'), 'GATE PASSED\n')
  assert.equal(f.count(), 1)
})

test('same-tree passes are reused, while edits, failed sources and forced runs re-execute', (t) => {
  const f = fixture(t)
  passed(f.wait(f.start()))
  const reused = f.start()
  passed(f.wait(reused))
  assert.match(f.wait(reused).stdout, /\nreused: passed run /)
  assert.equal(f.count(), 1)
  fs.writeFileSync(path.join(f.dir, 'input'), 'edited\n')
  passed(f.wait(f.start()))
  assert.equal(f.count(), 2)
  for (let i = 0; i < 2; i++)
    passed(f.wait(f.start('test-affected', { BOSS_GATE_FORCE_UNCACHED: '1' })))
  assert.equal(f.count(), 4)
  fs.writeFileSync(path.join(f.dir, 'input'), 'failure tree\n')
  f.mark('fail')
  assert.equal(f.wait(f.start()).status, 1)
  fs.rmSync(path.join(f.state, 'fail'))
  passed(f.wait(f.start()))
  assert.equal(f.count(), 6)
})

test('a write during the source run prevents reuse even after the tree is restored', (t) => {
  const f = fixture(t)
  f.mark('change')
  passed(f.wait(f.start()))
  fs.rmSync(path.join(f.state, 'change'))
  fs.writeFileSync(path.join(f.dir, 'input'), 'original\n')
  const next = f.start()
  passed(f.wait(next))
  assert.equal(fs.existsSync(path.join(next.run, 'reuse.json')), false)
  assert.equal(f.count(), 2)
})

test('stopping a joiner leaves its source alive; stopping a reused pass changes no files', async (t) => {
  const f = fixture(t)
  f.mark('hold')
  const source = f.start()
  await until(() => f.count() === 1)
  const joiner = f.start('test')
  assert.equal(f.call(['stop', joiner.run]).status, 0)
  const stopped = f.wait(joiner)
  assert.equal(stopped.status, 96)
  assert.equal(
    stopped.stdout.split('\n').find((line) => line.startsWith('log: ')),
    `log: ${path.join(source.run, 'log')}`,
  )
  assert.equal(fs.existsSync(path.join(source.run, 'log')), true)
  assert.match(
    f.call(['wait', '--timeout', '0', source.run]).stdout,
    /^GATE UNKNOWN - still running/,
  )
  f.mark('release')
  passed(f.wait(source))
  const reused = f.start()
  passed(f.wait(reused))
  const before = fs
    .readdirSync(reused.run)
    .map((name) => [name, fs.statSync(path.join(reused.run, name)).mtimeMs])
  passed(f.call(['stop', reused.run]))
  assert.deepEqual(
    fs
      .readdirSync(reused.run)
      .map((name) => [name, fs.statSync(path.join(reused.run, name)).mtimeMs]),
    before,
  )
  passed(f.wait(reused))
  assert.equal(f.count(), 1)
})

test('source teardown and disappearance reach every joiner without a false pass', async (t) => {
  const f = fixture(t)
  f.mark('hold')
  const source = f.start()
  await until(() => f.count() === 1)
  const joiner = f.start('test')
  assert.equal(f.call(['stop', source.run]).status, 0)
  const result = f.wait(joiner)
  assert.equal(result.status, 96, result.stdout)
  assert.match(result.stdout, /^GATE STOPPED/)
  fs.rmSync(source.run, { recursive: true, force: true, maxRetries: 3 })
  assert.equal(f.wait(joiner).status, 97)
})

test('an active joined wait observes its caller stop before a later source pass', async (t) => {
  const f = fixture(t)
  f.mark('hold')
  const source = f.start()
  await until(() => f.count() === 1)
  const joiner = f.start('test')
  const waiting = await f.waitActive(joiner)
  assert.equal(f.call(['stop', joiner.run]).status, 0)
  f.mark('release')
  const result = await waiting.done
  assert.equal(result.status, 96, result.stdout + result.stderr)
  assert.match(result.stdout, /^GATE STOPPED/)
  assert.equal(
    fs.readFileSync(path.join(joiner.run, 'verdict'), 'utf8'),
    'GATE STOPPED (exit 143)\n',
  )
  passed(f.wait(source))
})
