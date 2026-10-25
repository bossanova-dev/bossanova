import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { setTimeout } from 'node:timers/promises'
import { test } from 'node:test'
import { tempDir } from './test-tmpdir.mjs'

const runner = fileURLToPath(new URL('../gate-run.mjs', import.meta.url))
const wrapper = fileURLToPath(new URL('./run-gate.mjs', import.meta.url))
function fixture(t) {
  const dir = tempDir('gate-run-admission-')
  const slots = path.join(dir, 'slots')
  const env = {
    ...process.env,
    HOME: dir,
    TMPDIR: dir,
    GOCACHE: path.join(dir, 'gocache'),
    CI: '',
    BOSS_GATE_ADMITTED: '',
    BOSS_GATE_ADMISSION_DIR: slots,
    BOSS_GATE_MAX_CONCURRENT: '1',
    BOSS_GATE_ADMISSION_POLL_MS: '10',
    BOSS_GATE_MIN_FREE_GIB: '0',
    BOSS_GATE_ISOLATE: '0',
  }
  delete env.NODE_TEST_CONTEXT
  const call = (script, args, extra = {}) =>
    spawnSync(process.execPath, [script, ...args], {
      cwd: dir,
      env: { ...env, ...extra },
      encoding: 'utf8',
      timeout: 15000,
    })
  const start = (body, extra) => {
    const result = call(runner, ['start', '--', process.execPath, '-e', body], extra)
    assert.equal(result.status, 0, result.stderr)
    const run = result.stdout.split('\n')[0]
    t.after(() => {
      call(runner, ['stop', run])
      fs.rmSync(run, { recursive: true, force: true, maxRetries: 3 })
    })
    return run
  }
  const wait = (run, timeout = '10000') => call(runner, ['wait', '--timeout', timeout, run])
  return { dir, slots, env, call, start, wait }
}
async function until(check) {
  const deadline = Date.now() + 10000
  while (!check()) {
    assert.ok(Date.now() < deadline, 'fixture failed to reach expected state')
    await setTimeout(10)
  }
}
const write = (file) => `require('fs').writeFileSync(${JSON.stringify(file)}, 'ran');`

test('two gates serialize commands and expose queued phase and admission evidence', async (t) => {
  const f = fixture(t)
  const firstMarker = path.join(f.dir, 'first')
  const release = path.join(f.dir, 'release')
  const first = f.start(
    `${write(firstMarker)} const timer = setInterval(() => { if(require('fs').existsSync(${JSON.stringify(release)})) {clearInterval(timer)} }, 10)`,
  )
  await until(() => fs.existsSync(firstMarker))
  const secondMarker = path.join(f.dir, 'second')
  const second = f.start(write(secondMarker))
  await until(() => fs.existsSync(path.join(second, 'queued')))
  const queued = f.wait(second, '0')
  assert.match(queued.stdout, /phase: queued for a host gate slot/)
  assert.equal(fs.existsSync(secondMarker), false)
  fs.writeFileSync(release, 'go')
  assert.equal(f.wait(first).status, 0)
  assert.equal(f.wait(second).status, 0)
  assert.equal(fs.existsSync(secondMarker), true)
  assert.ok(JSON.parse(fs.readFileSync(path.join(second, 'admission.json'))).waitedMs > 0)
  assert.deepEqual(fs.readdirSync(f.slots), [])
})

test('nested gate-run to run-gate and run-gate to run-gate inherit the single slot', (t) => {
  const f = fixture(t)
  const nested = `${write(path.join(f.dir, 'nested'))}`
  const body = `const r = require('child_process').spawnSync(process.execPath, [${JSON.stringify(wrapper)}, '--', process.execPath, ${JSON.stringify(wrapper)}, '--', process.execPath, '-e', ${JSON.stringify(nested)}], {stdio: 'inherit'}); process.exit(r.status)`
  assert.equal(f.wait(f.start(body)).status, 0)
  assert.equal(fs.existsSync(path.join(f.dir, 'nested')), true)
  assert.deepEqual(fs.readdirSync(f.slots), [])
})

test('timeouts refuse both wrappers before the command and leave the live holder intact', (t) => {
  const f = fixture(t)
  fs.mkdirSync(f.slots)
  const slot = path.join(f.slots, 'slot-0')
  fs.writeFileSync(slot, JSON.stringify({ pid: process.pid, cwd: f.dir, label: 'held' }))
  const marker = path.join(f.dir, 'never')
  const run = f.start(write(marker), { BOSS_GATE_ADMISSION_TIMEOUT_MS: '30' })
  const waited = f.wait(run)
  assert.equal(waited.status, 75, waited.stdout)
  assert.match(waited.stdout, /^GATE ENVIRONMENT FAILURE \(exit 75\)/)
  assert.match(waited.stdout, /kind: host-gate-admission/)
  const direct = f.call(wrapper, ['--', process.execPath, '-e', write(marker)], {
    BOSS_GATE_ADMISSION_TIMEOUT_MS: '30',
  })
  assert.equal(direct.status, 75)
  assert.match(direct.stderr, /ENVIRONMENT FAILURE.*host-gate-admission/)
  assert.equal(fs.existsSync(marker), false)
  assert.deepEqual(fs.readdirSync(f.slots), ['slot-0'])
})

test('dead inherited tokens are ignored and stale slots reclaimed', (t) => {
  const f = fixture(t)
  fs.mkdirSync(f.slots)
  const slot = path.join(f.slots, 'slot-0')
  fs.writeFileSync(slot, JSON.stringify({ pid: 2147483647 }))
  const run = f.start('process.exit(0)', { BOSS_GATE_ADMITTED: slot })
  assert.equal(f.wait(run).status, 0)
  assert.equal(JSON.parse(fs.readFileSync(path.join(run, 'admission.json'))).inherited, false)
  assert.deepEqual(fs.readdirSync(f.slots), [])
})

for (const mode of ['fail', 'stop', 'refusal']) {
  test(`slot is released after ${mode}`, async (t) => {
    const f = fixture(t)
    const marker = path.join(f.dir, 'started')
    const run = f.start(
      mode === 'stop' ? `${write(marker)} setInterval(() => {}, 1000)` : 'process.exit(1)',
      mode === 'refusal' ? { BOSS_GATE_MIN_FREE_GIB: '999999' } : {},
    )
    if (mode === 'stop') {
      await until(() => fs.existsSync(marker))
      f.call(runner, ['stop', run])
    }
    const result = f.wait(run)
    assert.equal(result.status, mode === 'stop' ? 96 : mode === 'refusal' ? 75 : 1, result.stdout)
    assert.deepEqual(fs.readdirSync(f.slots), [])
    assert.match(result.stdout, /admission:|GATE STOPPED/)
  })
}
