import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const runner = fileURLToPath(new URL('../gate-run.mjs', import.meta.url))
function fixture(t, mode, body = '') {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-disk-test-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
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
  const depleted = path.join(dir, 'depleted')
  const preload = path.join(dir, 'measure.mjs')
  // Filesystem measurements are injected; no host volume or cache is measured.
  fs.writeFileSync(
    preload,
    `import fs from 'node:fs'; fs.statfsSync = () => ({bsize: 1, bavail: ${mode === 'restore' ? `fs.existsSync(${JSON.stringify(old)}) ? 1 : 100 * 1024 ** 3` : mode === 'refuse' ? '1' : mode === 'midrun' ? `fs.existsSync(${JSON.stringify(depleted)}) ? 1 : 100 * 1024 ** 3` : '100 * 1024 ** 3'}});`,
  )
  const env = {
    ...process.env,
    GOCACHE: cache,
    HOME: dir,
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
  const marker = path.join(dir, 'marker')
  const started = call([
    'start',
    '--',
    process.execPath,
    '-e',
    body.replace('DEPLETED', JSON.stringify(depleted)) ||
      `require('fs').writeFileSync(${JSON.stringify(marker)}, 'ran')`,
  ])
  assert.equal(started.status, 0, started.stderr)
  const run = started.stdout.split('\n')[0]
  t.after(() => {
    call(['stop', run])
    fs.rmSync(run, { recursive: true, force: true })
  })
  return { run, old, young, marker, wait: () => call(['wait', '--timeout', '15000', run]) }
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
