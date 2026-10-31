import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import { spawn, spawnSync } from 'node:child_process'
import { once } from 'node:events'
import { test } from 'node:test'
import { tempDir } from './test-tmpdir.mjs'
import { runGolangciSync, golangciAdmissionConfig } from './golangci-admission.mjs'

function fixture(t) {
  const dir = tempDir('golangci-admission-', t)
  const log = path.join(dir, 'events')
  fs.writeFileSync(
    path.join(dir, 'golangci-lint'),
    `#!/usr/bin/env node
const fs = require('node:fs')
fs.appendFileSync(process.env.EVENTS, JSON.stringify({kind:'start',pid:process.pid,args:process.argv.slice(2),cache:process.env.GOLANGCI_LINT_CACHE,shared:fs.existsSync(require('node:path').join(process.env.BOSS_GOLANGCI_SLOT_DIR,'shared','slot-0')),sentinel:process.env.GOLANGCI_LINT_CACHE ? fs.existsSync(require('node:path').join(process.env.GOLANGCI_LINT_CACHE,'sentinel')) : false})+'\\n')
Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,80)
fs.appendFileSync(process.env.EVENTS, JSON.stringify({kind:'end',pid:process.pid})+'\\n')
`,
    { mode: 0o755 },
  )
  return {
    dir,
    log,
    env: {
      ...process.env,
      PATH: dir + path.delimiter + process.env.PATH,
      EVENTS: log,
      BOSS_GOLANGCI_SLOT_DIR: path.join(dir, 'slots'),
      BOSS_GOLANGCI_SLOTS: '2',
      BOSS_GOLANGCI_ADMISSION_WAIT_MS: '5000',
      GOLANGCI_LINT_CACHE: '',
      BOSS_GATE_LINT_CACHE_POOL: '',
      BOSS_GATE_ADMISSION_DIR: path.join(dir, 'gate-slots'),
    },
  }
}
function events(log) {
  return fs.readFileSync(log, 'utf8').trim().split('\n').map(JSON.parse)
}
function overlap(entries) {
  let active = 0,
    peak = 0
  for (const entry of entries) {
    active += entry.kind === 'start' ? 1 : -1
    peak = Math.max(peak, active)
  }
  assert.equal(active, 0)
  return peak
}
async function concurrent(f, privateCache) {
  const source = `import {runGolangciSync} from ${JSON.stringify(new URL('./golangci-admission.mjs', import.meta.url).href)};
    const r=runGolangciSync(['run','./...'],{privateCache:${privateCache}}); process.exitCode=r.status ?? 1;`
  const children = Array.from({ length: 6 }, () =>
    spawn(process.execPath, ['--input-type=module', '-e', source], {
      env: f.env,
      stdio: ['ignore', 'pipe', 'pipe'],
    }),
  )
  const results = await Promise.all(
    children.map(async (c) => {
      let err = ''
      c.stderr.on('data', (d) => (err += d))
      const [code] = await once(c, 'exit')
      assert.equal(code, 0, err)
    }),
  )
  return results
}
test('six private-cache children respect two host slots and configured CPU width', async (t) => {
  const f = fixture(t)
  await concurrent(f, true)
  const entries = events(f.log)
  const peak = overlap(entries)
  assert.ok(peak <= 2)
  assert.equal(entries.filter((e) => e.kind === 'start').length, 6)
  for (const e of entries.filter((e) => e.kind === 'start')) {
    assert.ok(e.args.includes('--allow-parallel-runners'))
    const width = e.args.indexOf('--concurrency')
    assert.ok(width >= 0)
    assert.equal(e.args[width + 1], String(Math.max(1, Math.floor(os.availableParallelism() / 2))))
  }
  t.diagnostic(`maximum private overlap: ${peak}`)
})
test('shared-cache children serialize before the child timeout begins', async (t) => {
  const f = fixture(t)
  await concurrent(f, false)
  assert.equal(overlap(events(f.log)), 1)
  for (const e of events(f.log).filter((e) => e.kind === 'start'))
    assert.ok(e.args.includes('--allow-serial-runners'))
  t.diagnostic('maximum shared overlap: 1')
})
test('runner flags are owned by the helper and explicit width is preserved', (t) => {
  const f = fixture(t)
  for (const flag of [
    '--allow-parallel-runners',
    '--allow-serial-runners',
    '--allow-serial-runners=true',
  ])
    assert.throws(() => runGolangciSync(['run', flag], { env: f.env }))
  const result = runGolangciSync(['run', '-j', '7', './...'], { env: f.env, privateCache: true })
  assert.equal(result.status, 0)
  assert.ok(!events(f.log)[0].args.includes('--concurrency'))
  assert.equal(golangciAdmissionConfig({}, 10).slots, 2)
  assert.equal(golangciAdmissionConfig({}, 4).slots, 2)
})
test('deadline never spawns a child and dead slot owners are reclaimed', (t) => {
  const f = fixture(t)
  fs.mkdirSync(f.env.BOSS_GOLANGCI_SLOT_DIR)
  for (let i = 0; i < 2; i++)
    fs.writeFileSync(
      path.join(f.env.BOSS_GOLANGCI_SLOT_DIR, `slot-${i}`),
      JSON.stringify({ pid: process.pid }),
    )
  // Capture the deliberate host-failure signature so an unrelated later TAP failure
  // cannot make the outer gate misclassify the whole suite as an environment failure.
  const source = `import {runGolangciSync} from ${JSON.stringify(new URL('./golangci-admission.mjs', import.meta.url).href)};
    process.stdout.write(JSON.stringify(runGolangciSync(['run'], {privateCache:true})))`
  const child = spawnSync(process.execPath, ['--input-type=module', '-e', source], {
    env: { ...f.env, BOSS_GOLANGCI_ADMISSION_WAIT_MS: '10' },
    encoding: 'utf8',
  })
  assert.equal(child.status, 0, child.stderr)
  const result = JSON.parse(child.stdout)
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /golangci-lint: host admission wait exceeded/)
  assert.equal(fs.existsSync(f.log), false)
  for (let i = 0; i < 2; i++)
    fs.writeFileSync(
      path.join(f.env.BOSS_GOLANGCI_SLOT_DIR, `slot-${i}`),
      JSON.stringify({ pid: 2147483647 }),
    )
  assert.equal(runGolangciSync(['run'], { env: f.env, privateCache: true }).status, 0)
})

test('a live child retains its slot and admission queue time is outside timeout', async (t) => {
  const f = fixture(t)
  fs.mkdirSync(f.env.BOSS_GOLANGCI_SLOT_DIR)
  const file = path.join(f.env.BOSS_GOLANGCI_SLOT_DIR, 'slot-0')
  const holder = spawn(
    process.execPath,
    [
      '-e',
      `const fs=require('node:fs'); fs.writeFileSync(process.argv[1],JSON.stringify({pid:process.pid})); console.log('ready'); setTimeout(()=>{fs.unlinkSync(process.argv[1]);},3000)`,
      file,
    ],
    { stdio: ['ignore', 'pipe', 'inherit'] },
  )
  const ended = once(holder, 'exit')
  await once(holder.stdout, 'data')
  const result = runGolangciSync(['run'], {
    env: { ...f.env, BOSS_GOLANGCI_SLOTS: '1' },
    privateCache: true,
    timeout: 2000,
    encoding: 'utf8',
  })
  assert.equal(result.status, 0, result.stderr)
  await ended
  assert.equal(events(f.log).length, 2)
})
test('concurrent reclaimers preserve exactly one holder of a stale slot', async (t) => {
  const f = fixture(t)
  f.env.BOSS_GOLANGCI_SLOTS = '1'
  fs.mkdirSync(f.env.BOSS_GOLANGCI_SLOT_DIR)
  fs.writeFileSync(
    path.join(f.env.BOSS_GOLANGCI_SLOT_DIR, 'slot-0'),
    JSON.stringify({ pid: 2147483647 }),
  )
  await concurrent(f, true)
  assert.equal(overlap(events(f.log)), 1)
})

const runGate = new URL('./run-gate.mjs', import.meta.url).pathname
const lintSource = `import {runGolangciSync} from ${JSON.stringify(new URL('./golangci-admission.mjs', import.meta.url).href)};
  const result=runGolangciSync(['run','./...']); process.exitCode=result.status ?? 1;`

test('cache precedence preserves private, explicit, pool and default ownership', (t) => {
  for (const row of [
    { privateCache: true, cache: 'private', pool: '1', parallel: true, shared: false },
    { privateCache: false, cache: 'caller', pool: '1', parallel: false, shared: true },
    { privateCache: false, cache: '', pool: '1', parallel: true, shared: false },
    { privateCache: false, cache: '', pool: '', parallel: false, shared: true },
  ]) {
    const f = fixture(t)
    const cache = row.cache ? path.join(f.dir, row.cache) : ''
    const result = runGolangciSync(['run'], {
      privateCache: row.privateCache,
      env: { ...f.env, GOLANGCI_LINT_CACHE: cache, BOSS_GATE_LINT_CACHE_POOL: row.pool },
    })
    assert.equal(result.status, 0, result.stderr)
    const entry = events(f.log)[0]
    assert.equal(
      entry.cache,
      cache || (row.pool ? path.join(f.env.BOSS_GOLANGCI_SLOT_DIR, 'caches', 'slot-0') : ''),
    )
    assert.equal(entry.args.includes('--allow-parallel-runners'), row.parallel)
    assert.equal(entry.args.includes('--allow-serial-runners'), !row.parallel)
    assert.equal(entry.shared, row.shared)
  }
})

test('sequential gates reuse the acquired slot cache and preserve its contents', (t) => {
  const f = fixture(t)
  f.env.BOSS_GOLANGCI_SLOTS = '1'
  const source = `import fs from 'node:fs'; import path from 'node:path';
    import {runGolangciSync} from ${JSON.stringify(new URL('./golangci-admission.mjs', import.meta.url).href)};
    const result=runGolangciSync(['run']); process.exitCode=result.status ?? 1;`
  // The linter itself writes the sentinel: gates cannot manufacture cache hits.
  fs.appendFileSync(
    path.join(f.dir, 'golangci-lint'),
    `
fs.writeFileSync(require('node:path').join(process.env.GOLANGCI_LINT_CACHE,'sentinel'),'warm')
`,
  )
  for (let n = 0; n < 2; n++) {
    const result = spawnSync(
      process.execPath,
      [runGate, '--', process.execPath, '--input-type=module', '-e', source],
      { env: f.env, encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
  }
  const starts = events(f.log).filter((e) => e.kind === 'start')
  const cache = path.join(f.env.BOSS_GOLANGCI_SLOT_DIR, 'caches', 'slot-0')
  assert.deepEqual(
    starts.map((e) => e.cache),
    [cache, cache],
  )
  assert.deepEqual(
    starts.map((e) => e.sentinel),
    [false, true],
  )
  assert.equal(fs.readFileSync(path.join(cache, 'sentinel'), 'utf8'), 'warm')
})

test('two concurrent gates use distinct warm slot caches without serializing', async (t) => {
  const f = fixture(t)
  for (let index = 0; index < 2; index++) {
    const cache = path.join(f.env.BOSS_GOLANGCI_SLOT_DIR, 'caches', `slot-${index}`)
    fs.mkdirSync(cache, { recursive: true })
    fs.writeFileSync(path.join(cache, 'sentinel'), 'warm')
  }
  fs.writeFileSync(
    path.join(f.dir, 'golangci-lint'),
    fs.readFileSync(path.join(f.dir, 'golangci-lint'), 'utf8').replace(',0,0,80)', ',0,0,500)'),
  )
  const children = Array.from({ length: 2 }, () =>
    spawn(
      process.execPath,
      [runGate, '--', process.execPath, '--input-type=module', '-e', lintSource],
      { env: f.env, stdio: ['ignore', 'pipe', 'pipe'] },
    ),
  )
  await Promise.all(
    children.map(async (child) => {
      let stderr = ''
      child.stderr.on('data', (data) => {
        stderr += data
      })
      const [code] = await once(child, 'close')
      assert.equal(code, 0, stderr)
      assert.doesNotMatch(stderr, /golangci-lint: host admission wait exceeded/)
    }),
  )
  const entries = events(f.log)
  assert.equal(overlap(entries), 2)
  const starts = entries.filter((e) => e.kind === 'start')
  assert.equal(new Set(starts.map((e) => e.cache)).size, 2)
  for (const entry of starts) {
    assert.equal(entry.sentinel, true)
    assert.equal(entry.shared, false)
    assert.ok(entry.args.includes('--allow-parallel-runners'))
  }
})

test('workspace pin reaches the child and unavailable toolchain fails before admission', (t) => {
  const f = fixture(t)
  f.env.GOTOOLCHAIN = ''
  fs.writeFileSync(
    path.join(f.dir, 'go'),
    `#!/bin/sh
if [ "$1" = work ]; then echo '{"Toolchain":"go1.26.5"}';
elif [ "$2" = GOTOOLCHAIN ]; then echo auto;
elif [ -n "$GOTOOLCHAIN" ]; then
  if [ "$FAIL_PROBE" = 1 ]; then echo 'download denied' >&2; exit 1; fi
  echo "$GOTOOLCHAIN"
else echo go1.28.1; fi
`,
    { mode: 0o755 },
  )
  fs.writeFileSync(path.join(f.dir, 'golangci-lint'), '#!/bin/sh\necho "$GOTOOLCHAIN"\n', {
    mode: 0o755,
  })
  const success = runGolangciSync(['run'], { env: f.env })
  assert.equal(success.status, 0)
  assert.equal(success.stdout.trim(), 'go1.26.5')
  fs.rmSync(f.env.BOSS_GOLANGCI_SLOT_DIR, { recursive: true, force: true, maxRetries: 3 })
  const failure = runGolangciSync(['run'], { env: { ...f.env, FAIL_PROBE: '1' } })
  assert.equal(failure.status, 1)
  assert.match(failure.stderr, /go1\.26\.5.*go1\.28\.1.*GOTOOLCHAIN=local/s)
  assert.equal(fs.existsSync(f.env.BOSS_GOLANGCI_SLOT_DIR), false)
})
