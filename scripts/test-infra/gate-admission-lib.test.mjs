import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { tempDir } from './test-tmpdir.mjs'
import {
  admissionConfig,
  bazelResourceFlags,
  gateBazelEnv,
  staleSlot,
  inheritedAdmission,
  acquireGateSlot,
  acquireGateSlotSync,
  releaseGateSlot,
  admitGate,
  gateClass,
  renderAdmissionLine,
  ADMISSION_TIMEOUT_SIGNATURE,
} from './gate-admission-lib.mjs'

test('configuration coordinates homes and disables admission explicitly or in CI', () => {
  assert.equal(admissionConfig({}, 10).slots, 2)
  assert.equal(admissionConfig({}, 2).slots, 1)
  assert.equal(admissionConfig({ BOSS_GATE_MAX_CONCURRENT: '0' }, 10).slots, 0)
  assert.equal(admissionConfig({ CI: '1' }, 10).slots, 0)
  assert.equal(admissionConfig({ BOSS_GATE_MAX_CONCURRENT: '3' }, 10).slots, 3)
})

test('dead and malformed records are stale; live inherited tokens require an absolute slot path', () => {
  assert.equal(
    staleSlot({ pid: 7 }, () => false),
    true,
  )
  assert.equal(
    staleSlot(null, () => true),
    true,
  )
  assert.equal(
    staleSlot({ pid: 7 }, () => true),
    false,
  )
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: '/slots/slot-0', BOSS_GATE_ADMISSION_DIR: '/slots' },
      () => ({ pid: 7 }),
      () => true,
    ),
    '/slots/slot-0',
  )
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: '/slots/slot-0', BOSS_GATE_ADMISSION_DIR: '/slots' },
      () => ({ pid: 7 }),
      () => false,
    ),
    null,
  )
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: 'foreign' },
      () => ({ pid: 7 }),
      () => true,
    ),
    null,
  )
})

test('stale records are reclaimed in the same pass and release checks ownership', async () => {
  const dir = tempDir('gate-admission-')
  fs.writeFileSync(path.join(dir, 'slot-0'), JSON.stringify({ pid: 123 }))
  const handle = await acquireGateSlot({
    dir,
    slots: 1,
    timeoutMs: 100,
    pollMs: 1,
    isAlive: () => false,
    sleep: () => assert.fail('must not wait'),
  })
  assert.equal(handle.waitedMs, 0)
  fs.writeFileSync(handle.file, JSON.stringify({ pid: 999 }))
  releaseGateSlot(handle)
  assert.equal(fs.existsSync(handle.file), true)
  fs.writeFileSync(handle.file, JSON.stringify({ pid: process.pid }))
  releaseGateSlot(handle)
  assert.equal(fs.existsSync(handle.file), false)
})

test('queue logs only holder changes and refuses without a slot at its budget', async () => {
  const dir = tempDir('gate-admission-')
  fs.writeFileSync(
    path.join(dir, 'slot-0'),
    JSON.stringify({ pid: 123, cwd: '/one/worktree', label: 'test' }),
  )
  let clock = 0
  const logs = []
  await assert.rejects(
    acquireGateSlot({
      dir,
      slots: 1,
      timeoutMs: 30,
      pollMs: 10,
      isAlive: () => true,
      now: () => clock,
      sleep: async (ms) => {
        clock += ms
      },
      log: (line) => logs.push(line),
    }),
    { code: 'GATE_ADMISSION_TIMEOUT' },
  )
  assert.equal(logs.length, 1)
  assert.match(logs[0], /123.*worktree.*test/)
})

test('a live token from another slot directory does not bypass admission', () => {
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: '/foreign/slot-0', BOSS_GATE_ADMISSION_DIR: '/slots' },
      () => ({ pid: 7 }),
      () => true,
    ),
    null,
  )
})

test('Bazel CPU shares follow admission width and clear bypassed flags', () => {
  for (const [slots, cpus, share] of [
    [1, 10, 10],
    [2, 10, 5],
    [4, 10, 2],
    [4, 2, 1],
  ]) {
    const flags = [`--local_resources=cpu=${share}`, `--local_test_jobs=${share}`]
    assert.deepEqual(bazelResourceFlags({ slots, cpus }), flags)
    assert.deepEqual(gateBazelEnv({ slots }, cpus), { BOSS_GATE_BAZEL_FLAGS: flags.join(' ') })
  }
  for (const slots of [0, -1, 1.5, undefined]) {
    assert.deepEqual(bazelResourceFlags({ slots, cpus: 10 }), [])
    assert.deepEqual(gateBazelEnv({ slots }, 10), { BOSS_GATE_BAZEL_FLAGS: '' })
  }
})

test('gateClass admits only lint, format, build and codegen-check argv as light', () => {
  const light = [
    ['make', 'lint'],
    ['make', 'lint-scripts'],
    ['make', '-C', 'services/docs', 'lint'],
    ['make', 'format'],
    ['make', 'format-affected'],
    ['make', 'build'],
    ['make', 'build-boss'],
    ['make', 'bin/bossd'],
    ['make', '-B', 'bin/boss'],
    ['make', 'codex-skills-check'],
    ['make', 'generate'],
    ['make', 'RACE=1', 'lint'],
    ['/usr/bin/make', '-j', '4', 'lint', 'format'],
    ['make', '-j', 'lint'],
    ['buf', 'lint'],
    ['/repo/bin/buf', 'format'],
    ['buf', 'breaking'],
    ['node', 'scripts/lint/lint-affected.mjs'],
    [process.execPath, 'scripts/lint/lint-docs-affected.mjs', '--base', 'main'],
  ]
  const heavy = [
    [],
    ['make'],
    ['make', 'RACE=1'],
    ['make', '-C', 'services/docs'],
    ['make', 'test'],
    ['make', 'test-bossd'],
    ['make', 'test-affected'],
    ['make', 'test-smoke'],
    ['make', 'lint', 'test'],
    ['make', 'test-race'],
    ['make', 'lint-test-check'],
    ['make', 'linter'],
    ['bazel', 'test', '//...'],
    ['buf', 'generate'],
    ['node', '-e', ''],
    ['node', 'scripts/lint/nested/deep.mjs'],
    ['node', 'scripts/test-infra/run-gate.mjs'],
    ['sh', '-c', 'make lint'],
    ['go', 'test', './...'],
    [
      'node',
      'scripts/test-infra/gate-cache.mjs',
      'run',
      '--site',
      'test-smoke',
      '--command',
      'make test-smoke',
      '--',
      'make',
      'test-smoke',
    ],
    ['./scripts/unknown.sh'],
  ]
  for (const argv of light) assert.equal(gateClass(argv), 'light', JSON.stringify(argv))
  for (const argv of heavy) assert.equal(gateClass(argv), 'heavy', JSON.stringify(argv))
  assert.equal(gateClass(undefined), 'heavy')
})

test('the light pool lives beside the heavy pool and bypasses only itself at zero', () => {
  const base = { BOSS_GATE_ADMISSION_DIR: '/slots' }
  const heavy = admissionConfig(base, 10)
  const light = admissionConfig(base, 10, 'light')
  assert.deepEqual([heavy.dir, heavy.slots, heavy.pool], ['/slots', 2, 'heavy'])
  assert.deepEqual([light.dir, light.slots, light.pool], ['/slots/light', 2, 'light'])
  assert.equal(admissionConfig(base, 2, 'light').slots, 2)
  assert.equal(admissionConfig(base, 20, 'light').slots, 5)
  assert.equal(
    admissionConfig({ ...base, BOSS_GATE_LIGHT_MAX_CONCURRENT: '6' }, 10, 'light').slots,
    6,
  )
  assert.equal(admissionConfig({ ...base, BOSS_GATE_MAX_CONCURRENT: '6' }, 10, 'light').slots, 2)
  const lightOff = { ...base, BOSS_GATE_LIGHT_MAX_CONCURRENT: '0' }
  assert.equal(admissionConfig(lightOff, 10, 'light').slots, 0)
  assert.equal(admissionConfig(lightOff, 10).slots, 2)
  for (const bypass of [{ CI: '1' }, { BOSS_GATE_MAX_CONCURRENT: '0' }]) {
    const env = { ...base, ...bypass, BOSS_GATE_LIGHT_MAX_CONCURRENT: '4' }
    assert.equal(admissionConfig(env, 10).slots, 0)
    assert.equal(admissionConfig(env, 10, 'light').slots, 0)
  }
})

test('live tokens from either pool are inherited; dead and foreign ones are not', () => {
  const env = (token) => ({ BOSS_GATE_ADMITTED: token, BOSS_GATE_ADMISSION_DIR: '/slots' })
  const live = () => true
  const read = () => ({ pid: 7 })
  assert.equal(inheritedAdmission(env('/slots/light/slot-1'), read, live), '/slots/light/slot-1')
  assert.equal(inheritedAdmission(env('/slots/slot-1'), read, live), '/slots/slot-1')
  assert.equal(
    inheritedAdmission(env('/slots/light/slot-1'), read, () => false),
    null,
  )
  for (const foreign of ['/slots/other/slot-0', '/foreign/light/slot-0', '/slots/light/x'])
    assert.equal(inheritedAdmission(env(foreign), read, live), null, foreign)
})

test('light admissions take a light slot but export the heavy-width Bazel share', async () => {
  const dir = tempDir('gate-admission-')
  const env = { BOSS_GATE_ADMISSION_DIR: dir, BOSS_GATE_MAX_CONCURRENT: '2' }
  const handle = await admitGate(env, { argv: ['make', 'lint'], label: 'make lint' })
  try {
    assert.equal(handle.pool, 'light')
    assert.equal(path.dirname(handle.file), path.join(dir, 'light'))
    assert.deepEqual(fs.readdirSync(dir), ['light'])
    assert.deepEqual(gateBazelEnv(handle, 10), gateBazelEnv({ slots: 2 }, 10))
    const nested = await admitGate(
      { ...env, BOSS_GATE_ADMITTED: handle.file },
      { argv: ['make', 'test'] },
    )
    assert.deepEqual([nested.inherited, nested.pool, nested.file], [true, 'light', handle.file])
    assert.deepEqual(gateBazelEnv(nested, 10), gateBazelEnv({ slots: 2 }, 10))
  } finally {
    releaseGateSlot(handle)
  }
  assert.deepEqual(fs.readdirSync(path.join(dir, 'light')), [])
  const heavy = await admitGate(env, { argv: ['make', 'test'] })
  releaseGateSlot(heavy)
  assert.deepEqual([heavy.pool, path.dirname(heavy.file)], ['heavy', dir])
  const bypassed = await admitGate({ ...env, CI: '1' }, { argv: ['make', 'lint'] })
  assert.deepEqual([bypassed.file, bypassed.pool], [null, 'light'])
  assert.deepEqual(gateBazelEnv(bypassed, 10), { BOSS_GATE_BAZEL_FLAGS: '' })
  const lightOff = await admitGate(
    { ...env, BOSS_GATE_LIGHT_MAX_CONCURRENT: '0' },
    { argv: ['make', 'lint'] },
  )
  assert.equal(lightOff.file, null)
  assert.deepEqual(gateBazelEnv(lightOff, 10), gateBazelEnv({ slots: 2 }, 10))
})

test('a light queue names its pool and times out with the shared admission signature', async () => {
  const dir = tempDir('gate-admission-')
  fs.mkdirSync(path.join(dir, 'light'))
  fs.writeFileSync(path.join(dir, 'light', 'slot-0'), JSON.stringify({ pid: process.pid }))
  const logs = []
  await assert.rejects(
    admitGate(
      {
        BOSS_GATE_ADMISSION_DIR: dir,
        BOSS_GATE_LIGHT_MAX_CONCURRENT: '1',
        BOSS_GATE_ADMISSION_TIMEOUT_MS: '20',
        BOSS_GATE_ADMISSION_POLL_MS: '5',
      },
      { argv: ['buf', 'lint'], log: (line) => logs.push(line) },
    ),
    (error) =>
      error.code === 'GATE_ADMISSION_TIMEOUT' &&
      error.message.startsWith(ADMISSION_TIMEOUT_SIGNATURE),
  )
  assert.match(logs[0], /^gate-run admission: queued for a light gate slot - 0 ahead, held by /)
})

test('admission lines name the pool and read a pool-less record as heavy', () => {
  assert.equal(
    renderAdmissionLine({ waitedMs: 0, slots: 2, pool: 'light' }),
    'admission: waited 0.0s for a light gate slot (light pool, 2 slots)',
  )
  assert.equal(
    renderAdmissionLine({ waitedMs: 303000, slots: 2 }),
    'admission: waited 303.0s for a heavy gate slot (heavy pool, 2 slots)',
  )
  assert.equal(renderAdmissionLine(null), 'admission: unavailable')
})

function ticket(dir, { pid = 123, startedAt = 0, modifiedAt = startedAt, pollMs } = {}) {
  const file = path.join(dir, `.candidate-${pid}-older`)
  fs.writeFileSync(
    file,
    JSON.stringify({ pid, startedAt: new Date(startedAt).toISOString(), pollMs }),
  )
  fs.utimesSync(file, new Date(modifiedAt), new Date(modifiedAt))
  return file
}

test('a fresh older ticket prevents overtaking even when the slot is free', async () => {
  const dir = tempDir('gate-admission-')
  const older = ticket(dir)
  let clock = 10
  const options = {
    dir,
    slots: 1,
    timeoutMs: 30,
    pollMs: 10,
    isAlive: () => true,
    now: () => clock,
    sleep: async (ms) => {
      clock += ms
    },
  }
  const logs = []
  const waits = []
  await assert.rejects(
    acquireGateSlot({
      ...options,
      log: (line) => logs.push(line),
      onWait: (...args) => waits.push(args),
    }),
    { code: 'GATE_ADMISSION_TIMEOUT' },
  )
  assert.equal(fs.existsSync(path.join(dir, 'slot-0')), false)
  assert.equal(logs.length, 1)
  assert.match(logs[0], /1 ahead/)
  assert.deepEqual(waits, [['', 'host', 1]])
  fs.unlinkSync(older)
  const handle = await acquireGateSlot(options)
  releaseGateSlot(handle)
  assert.deepEqual(fs.readdirSync(dir), [])
})

test('the oldest waiter wins a release even when the younger polls first', async () => {
  const dir = tempDir('gate-admission-')
  let clock = 0
  const options = {
    dir,
    slots: 1,
    timeoutMs: 100,
    pollMs: 1,
    isAlive: () => true,
    now: () => clock,
  }
  const holder = await acquireGateSlot(options)
  const wakeOlder = []
  const wakeYounger = []
  const older = acquireGateSlot({
    ...options,
    sleep: () => new Promise((resolve) => wakeOlder.push(resolve)),
  })
  clock++
  let youngerAdmitted = false
  const younger = acquireGateSlot({
    ...options,
    sleep: () => new Promise((resolve) => wakeYounger.push(resolve)),
  }).then((handle) => {
    youngerAdmitted = true
    return handle
  })
  releaseGateSlot(holder)
  wakeYounger.shift()()
  await Promise.resolve()
  assert.equal(youngerAdmitted, false)
  assert.equal(fs.existsSync(path.join(dir, 'slot-0')), false)
  wakeOlder.shift()()
  const first = await older
  assert.equal(youngerAdmitted, false)
  releaseGateSlot(first)
  wakeYounger.shift()()
  const second = await younger
  releaseGateSlot(second)
  assert.deepEqual(fs.readdirSync(dir), [])
})

test('free-slot count admits the next ranked waiter without leaving capacity idle', async () => {
  const dir = tempDir('gate-admission-')
  const older = ticket(dir)
  const handle = await acquireGateSlot({
    dir,
    slots: 2,
    timeoutMs: 100,
    pollMs: 1,
    isAlive: () => true,
    now: () => 10,
    sleep: () => assert.fail('two free slots allow two ranked waiters'),
  })
  releaseGateSlot(handle)
  assert.equal(fs.existsSync(older), true)
})

test('dead tickets are removed while stale live tickets are skipped without deletion', async () => {
  for (const dead of [true, false]) {
    const dir = tempDir('gate-admission-')
    const older = ticket(dir)
    const handle = await acquireGateSlot({
      dir,
      slots: 1,
      timeoutMs: 100,
      pollMs: 1,
      isAlive: (pid) => !dead || pid === process.pid,
      now: () => 30001,
      sleep: () => assert.fail('dead or stale ticket must not block'),
    })
    releaseGateSlot(handle)
    assert.equal(fs.existsSync(older), !dead)
  }
})

test('ticket freshness covers ten of its own polling intervals', async () => {
  const dir = tempDir('gate-admission-')
  const older = ticket(dir, { pollMs: 5000 })
  let clock = 40000
  await assert.rejects(
    acquireGateSlot({
      dir,
      slots: 1,
      timeoutMs: 10,
      pollMs: 10,
      isAlive: () => true,
      now: () => clock,
      sleep: async (ms) => {
        clock += ms
      },
    }),
    { code: 'GATE_ADMISSION_TIMEOUT' },
  )
  assert.equal(fs.existsSync(older), true)
  assert.equal(fs.existsSync(path.join(dir, 'slot-0')), false)
})

test('a candidate still being published is left for its owner to finish', async () => {
  const dir = tempDir('gate-admission-')
  const candidate = path.join(dir, '.candidate-publishing')
  fs.writeFileSync(candidate, '')
  const handle = await acquireGateSlot({
    dir,
    slots: 1,
    timeoutMs: 100,
    pollMs: 1,
    isAlive: () => true,
    sleep: () => assert.fail('an unpublished ticket has no queue position yet'),
  })
  releaseGateSlot(handle)
  assert.equal(fs.existsSync(candidate), true, 'the writer must retain its ticket for heartbeat')
})

test('a synchronous slot wait stops cooperatively and preserves its live holder', () => {
  const dir = tempDir('gate-admission-')
  const slot = path.join(dir, 'slot-0')
  const holder = JSON.stringify({ pid: process.pid, label: 'held' })
  fs.writeFileSync(slot, holder)
  let stopped = false
  let clock = 0
  assert.throws(
    () =>
      acquireGateSlotSync({
        dir,
        slots: 1,
        timeoutMs: 30,
        pollMs: 1,
        now: () => clock++,
        shouldStop: () => stopped,
        onWait: () => {
          stopped = true
        },
      }),
    { code: 'GATE_ADMISSION_STOPPED' },
  )
  assert.equal(stopped, true)
  assert.equal(fs.readFileSync(slot, 'utf8'), holder)
  assert.deepEqual(fs.readdirSync(dir), ['slot-0'])
})
