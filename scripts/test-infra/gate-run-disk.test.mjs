import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'
import { createHash } from 'node:crypto'
import { setTimeout } from 'node:timers/promises'

const runner = fileURLToPath(new URL('../gate-run.mjs', import.meta.url))
function fixture(t, mode, body = '', heldLock = false, markerFailure = '', diagnostics = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-disk-test-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const cache = path.join(dir, 'cache')
  fs.mkdirSync(path.join(cache, 'aa'), { recursive: true })
  fs.writeFileSync(
    path.join(cache, 'README'),
    'This directory holds cached build artifacts from the Go build system.\n',
  )
  const old = path.join(cache, 'aa', `${'a'.repeat(64)}-d`)
  const young = path.join(cache, 'aa', `${'b'.repeat(64)}-a`)
  fs.writeFileSync(old, 'old')
  fs.writeFileSync(young, 'young')
  fs.utimesSync(old, new Date(0), new Date(0))
  fs.mkdirSync(path.join(cache, 'bb'))
  fs.writeFileSync(path.join(cache, 'bb', `${'c'.repeat(64)}-d`), 'young')
  const recovered = path.join(dir, 'recovered')
  const depleted = path.join(dir, 'depleted')
  const preload = path.join(dir, 'measure.mjs')
  // Filesystem measurements are injected; no host volume or cache is measured.
  fs.writeFileSync(
    preload,
    `import fs from 'node:fs'; fs.statfsSync = () => ({bsize: 1, bavail: ${heldLock ? `fs.existsSync(${JSON.stringify(recovered)}) ? 100 * 1024 ** 3 : 1` : mode === 'restore' ? `fs.existsSync(${JSON.stringify(old)}) ? 1 : 100 * 1024 ** 3` : mode === 'refuse' ? '1' : mode === 'midrun' ? `fs.existsSync(${JSON.stringify(depleted)}) ? 1 : 100 * 1024 ** 3` : '100 * 1024 ** 3'}});`,
  )
  if (markerFailure || diagnostics.files?.length || diagnostics.log) {
    fs.appendFileSync(
      preload,
      `
const write = fs.writeFileSync;
const writeLog = fs.writeSync;
fs.writeSync = (fd, content, ...args) => {
  if (${JSON.stringify(diagnostics.log ?? false)} && String(content).startsWith('gate-run ')) {
    throw Object.assign(new Error('injected diagnostic log ENOSPC'), {code: 'ENOSPC'});
  }
  if (${JSON.stringify(markerFailure)} === 'reclaim log' &&
      String(content).startsWith('gate-run preflight:')) {
    throw Object.assign(new Error('injected log ENOSPC'), {code: 'ENOSPC'});
  }
  return writeLog(fd, content, ...args);
};
const mkdir = fs.mkdirSync;
const reclaimLock = (file) => String(file).includes('/go-cache-reclaim-');
fs.mkdirSync = (dir, ...args) => {
  if (${JSON.stringify(markerFailure)} === 'reclaim lock dir' && reclaimLock(dir)) {
    throw Object.assign(new Error('injected lock dir ENOSPC'), {code: 'ENOSPC'});
  }
  return mkdir(dir, ...args);
};
fs.writeFileSync = (file, content, ...args) => {
  if (${JSON.stringify(diagnostics.files ?? [])}.some(name => String(file).includes('/' + name + '.'))) {
    write(file, '', ...args);
    throw Object.assign(new Error('injected diagnostic ENOSPC'), {code: 'ENOSPC'});
  }
  if (${JSON.stringify(markerFailure)} === 'reclaim lock candidate' && reclaimLock(file)) {
    // A full volume can allocate the inode and then fail the write.
    write(file, '', ...args);
    throw Object.assign(new Error('injected lock candidate ENOSPC'), {code: 'ENOSPC'});
  }
  if (${JSON.stringify(Boolean(markerFailure))} && String(file).includes('/preflight.') &&
      (${JSON.stringify(markerFailure)} === 'starting reclaim' ||
       String(content).includes(${JSON.stringify(markerFailure)}))) {
    throw Object.assign(new Error('injected marker ENOSPC'), {code: 'ENOSPC'});
  }
  return write(file, content, ...args);
};`,
    )
  }
  const env = {
    ...process.env,
    GOCACHE: cache,
    HOME: dir,
    BOSS_GATE_ADMISSION_DIR: path.join(dir, 'slots'),
    BOSS_GATE_ADMISSION_POLL_MS: '10',
    BOSS_GATE_ADMITTED: '',
    CI: '',
    TMPDIR: dir,
    NODE_OPTIONS: `--import=${preload}`,
    BOSS_GATE_MIN_FREE_GIB: mode === 'disabled' ? '0' : '20',
  }
  delete env.NODE_TEST_CONTEXT
  const call = (args) =>
    spawnSync(process.execPath, [runner, ...args], {
      cwd: dir,
      env,
      encoding: 'utf8',
      timeout: 20000,
    })
  const lockDir = path.join(
    env.BOSS_GATE_ADMISSION_DIR,
    'go-cache-reclaim-' + createHash('sha256').update(cache).digest('hex').slice(0, 12),
  )
  const lock = path.join(lockDir, 'slot-0')
  if (heldLock) {
    fs.mkdirSync(lockDir, { recursive: true })
    fs.writeFileSync(lock, JSON.stringify({ pid: process.pid, cwd: dir, label: 'fixture reclaim' }))
  }
  const marker = path.join(dir, 'marker')
  const started = call([
    'start',
    '--',
    process.execPath,
    '-e',
    body.replace('DEPLETED', JSON.stringify(depleted)) ||
      `require('fs').writeFileSync(${JSON.stringify(marker)}, 'ran'); console.log('child launched')`,
  ])
  assert.equal(started.status, 0, started.stderr)
  const run = started.stdout.split('\n')[0]
  t.after(() => {
    call(['stop', run])
    fs.rmSync(run, { recursive: true, force: true, maxRetries: 3 })
  })
  return {
    run,
    old,
    lockDir,
    young,
    marker,
    recovered,
    lock,
    call,
    wait: () => call(['wait', '--timeout', '15000', run]),
  }
}
test('disk refusal evicts old fixture entries, preserves young entries and never launches child', (t) => {
  const f = fixture(t, 'refuse')
  const result = f.wait()
  assert.equal(result.status, 75, result.stdout + result.stderr)
  assert.match(result.stdout, /^GATE ENVIRONMENT FAILURE \(exit 75\)\n/)
  assert.match(result.stdout, /kind: disk-exhaustion/)
  assert.match(result.stdout, /\ndisk: /)
  assert.equal(fs.existsSync(f.marker), false)
  assert.equal(fs.existsSync(f.old), false)
  assert.equal(fs.existsSync(f.young), true)
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.run, 'disk.json'))).refused, true)
  assert.equal(
    fs.readFileSync(path.join(f.run, 'verdict'), 'utf8'),
    'GATE ENVIRONMENT FAILURE (exit 75)\n',
  )
})
test('reclaim restores headroom and launches the gate', (t) => {
  const f = fixture(t, 'restore')
  const result = f.wait()
  assert.equal(result.status, 0, result.stdout)
  assert.equal(fs.existsSync(f.marker), true)
  assert.equal(fs.existsSync(f.old), false)
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.run, 'disk.json'))).refused, false)
  assert.equal(fs.readFileSync(path.join(f.run, 'verdict'), 'utf8'), 'GATE PASSED\n')
  const log = fs.readFileSync(path.join(f.run, 'log'), 'utf8')
  assert.match(
    log,
    /gate-run preflight: .*below floor[\s\S]*sizing Go cache:[\s\S]*evicting Go cache:[\s\S]*reclaimed[\s\S]*child launched/,
  )
  assert.equal(fs.existsSync(path.join(f.run, 'preflight')), false)
})
test('zero floor disables all disk preflight work', (t) => {
  const f = fixture(t, 'disabled')
  assert.equal(f.wait().status, 0)
  assert.equal(fs.existsSync(f.marker), true)
  assert.equal(fs.existsSync(f.old), true)
  assert.equal(fs.existsSync(path.join(f.run, 'disk.json')), false)
})
test('mid-run disk exhaustion reclaims once and preserves the failed result', (t) => {
  const f = fixture(
    t,
    'midrun',
    `require('fs').writeFileSync(DEPLETED, '1'); console.error('No space left on device'); process.exit(1)`,
  )
  const result = f.wait()
  assert.equal(result.status, 75, result.stdout)
  const disk = JSON.parse(fs.readFileSync(path.join(f.run, 'disk.json')))
  assert.equal(disk.refused, false)
  assert.equal(fs.existsSync(f.old), false)
  assert.ok(disk.reclaimedBytes > 0)
  assert.equal(disk.atExit.length, 1)
  assert.match(result.stdout, /\ndisk: /)
})

test('load diagnostic ENOSPC preserves post-child disk recovery', (t) => {
  const f = fixture(
    t,
    'midrun',
    `require('fs').writeFileSync(DEPLETED, '1'); console.error('No space left on device'); process.exit(1)`,
    false,
    '',
    { files: ['load.json'] },
  )
  const result = f.wait()
  assert.equal(result.status, 75, result.stdout + result.stderr)
  assert.match(result.stdout, /^GATE ENVIRONMENT FAILURE/)
  assert.match(result.stdout, /kind: disk-exhaustion/)
  assert.equal(fs.existsSync(f.old), false)
  assert.ok(JSON.parse(fs.readFileSync(path.join(f.run, 'disk.json'))).reclaimedBytes > 0)
  assert.equal(
    fs.readdirSync(f.run).some((name) => name.startsWith('load.json')),
    false,
  )
})

test('load diagnostic ENOSPC preserves a passing child status', (t) => {
  const f = fixture(t, 'disabled', '', false, '', { files: ['load.json'] })
  const result = f.wait()
  assert.equal(result.status, 0, result.stdout + result.stderr)
  assert.match(result.stdout, /^GATE PASSED/)
  assert.equal(fs.readFileSync(path.join(f.run, 'status'), 'utf8').trim(), '0')
  assert.equal(
    fs.readdirSync(f.run).some((name) => name.startsWith('load.json')),
    false,
  )
})

test('admission diagnostic ENOSPC still reclaims and launches the child', (t) => {
  const f = fixture(t, 'restore', '', false, '', { files: ['admission.json'] })
  const result = f.wait()
  assert.equal(result.status, 0, result.stdout + result.stderr)
  assert.match(result.stdout, /^GATE PASSED/)
  assert.equal(fs.existsSync(f.old), false)
  assert.equal(fs.existsSync(f.marker), true)
})

test('diagnostic file and log ENOSPC preserves the child failure status', (t) => {
  const f = fixture(
    t,
    'midrun',
    `require('fs').writeFileSync(DEPLETED, '1'); console.error('No space left on device'); process.exit(1)`,
    false,
    '',
    { files: ['load.json', 'disk.json'], log: true },
  )
  const result = f.wait()
  assert.equal(result.status, 75, result.stdout + result.stderr)
  assert.match(result.stdout, /^GATE ENVIRONMENT FAILURE/)
  assert.match(result.stdout, /kind: disk-exhaustion/)
  assert.equal(fs.readFileSync(path.join(f.run, 'status'), 'utf8').trim(), '1')
  assert.equal(fs.existsSync(f.old), false)
})

for (const phase of ['starting reclaim', 'sizing Go cache', 'evicting Go cache', 'reclaim log']) {
  test(`mid-run reclaim survives ENOSPC writing ${phase} marker`, (t) => {
    const f = fixture(
      t,
      'midrun',
      `require('fs').writeFileSync(DEPLETED, '1'); console.error('No space left on device'); process.exit(1)`,
      false,
      phase,
    )
    const result = f.wait()
    assert.equal(result.status, 75, result.stdout + result.stderr)
    assert.equal(fs.existsSync(f.old), false)
    assert.equal(fs.existsSync(f.young), true)
    const disk = JSON.parse(fs.readFileSync(path.join(f.run, 'disk.json')))
    assert.ok(disk.reclaimedBytes > 0)
    assert.equal(disk.atExit.length, 1)
  })
}

for (const failure of ['reclaim lock dir', 'reclaim lock candidate']) {
  test(`reclaim continues unlocked when ENOSPC prevents allocating the ${failure.slice(13)}`, (t) => {
    const f = fixture(t, 'restore', '', false, failure)
    const result = f.wait()
    assert.equal(result.status, 0, result.stdout + result.stderr)
    assert.equal(fs.existsSync(f.old), false)
    assert.equal(fs.existsSync(f.young), true)
    assert.equal(fs.existsSync(f.marker), true)
    assert.equal(JSON.parse(fs.readFileSync(path.join(f.run, 'disk.json'))).refused, false)
    const log = fs.readFileSync(path.join(f.run, 'log'), 'utf8')
    assert.match(log, /reclaim lock unavailable; continuing unlocked: injected lock .*ENOSPC/)
    const leftovers = fs.existsSync(f.lockDir) ? fs.readdirSync(f.lockDir) : []
    assert.deepEqual(leftovers, [])
  })
}

async function until(check) {
  const deadline = Date.now() + 10000
  while (!check()) {
    assert.ok(Date.now() < deadline, 'fixture failed to reach expected state')
    await setTimeout(10)
  }
}
const waiting = (f) => fs.readFileSync(path.join(f.run, 'log'), 'utf8').includes('waiting for')
test('reclaim wait exposes its phase and remeasures before scanning', async (t) => {
  const f = fixture(t, 'restore', '', true)
  await until(() => waiting(f))
  assert.match(f.call(['wait', '--timeout', '0', f.run]).stdout, /phase: disk preflight/)
  fs.writeFileSync(f.recovered, 'yes')
  fs.unlinkSync(f.lock)
  assert.equal(f.wait().status, 0)
  const log = fs.readFileSync(path.join(f.run, 'log'), 'utf8')
  assert.match(log, /headroom recovered/)
  assert.doesNotMatch(log, /sizing Go cache/)
  assert.equal(fs.existsSync(f.old), true)
  assert.equal(fs.existsSync(f.marker), true)
})
test('stop cancels a reclaim lock wait without launching the child', async (t) => {
  const f = fixture(t, 'restore', '', true)
  await until(() => waiting(f))
  const pid = Number(fs.readFileSync(path.join(f.run, 'pid'), 'utf8'))
  f.call(['stop', f.run])
  assert.match(f.wait().stdout, /^GATE STOPPED/)
  await until(() => {
    try {
      process.kill(pid, 0)
      return false
    } catch {
      return true
    }
  })
  assert.equal(fs.existsSync(f.old), true)
  assert.equal(fs.existsSync(f.marker), false)
  assert.equal(fs.existsSync(path.join(f.run, 'preflight')), false)
})
