import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const runner = fileURLToPath(new URL('../gate-run.mjs', import.meta.url))
function call(args, env = {}) {
  const childEnv = { ...process.env, BOSS_GATE_MIN_FREE_GIB: '0', ...env }
  delete childEnv.NODE_TEST_CONTEXT
  return spawnSync(process.execPath, [runner, ...args], {
    encoding: 'utf8',
    env: childEnv,
    timeout: 20000,
  })
}
function fixture(t, body, env = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-isolation-test-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const file = path.join(dir, 'fixture.test.mjs')
  fs.writeFileSync(file, `import test from 'node:test'; import fs from 'node:fs';\n${body}`)
  const start = call(['start', '--', process.execPath, '--test', file], env)
  assert.equal(start.status, 0, start.stderr)
  const run = start.stdout.split('\n')[0]
  t.after(() => {
    call(['stop', run])
    fs.rmSync(run, { recursive: true, force: true })
  })
  return { dir, file, run, wait: () => call(['wait', '--timeout', '15000', run]) }
}
function alive(pid) {
  try {
    if (process.platform === 'linux') {
      const stat = fs.readFileSync(`/proc/${pid}/stat`, 'utf8')
      if (stat.slice(stat.lastIndexOf(')') + 2).startsWith('Z')) return false
    }
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}
function until(fn) {
  const deadline = Date.now() + 10000
  while (Date.now() < deadline) {
    if (fn()) return
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 25)
  }
  assert.fail('condition did not arrive')
}
test('first-run failure passes alone, preserving original status and evidence order', (t) => {
  const f = fixture(
    t,
    `test('marker', () => { const marker = new URL('./marker', import.meta.url); if (!fs.existsSync(marker)) { fs.writeFileSync(marker, '1'); throw Error('first run') } })`,
  )
  const result = f.wait()
  assert.equal(result.status, 95, result.stdout + result.stderr)
  assert.equal(result.stdout.split('\n')[0], 'GATE FLAKY-UNDER-LOAD (exit 1)')
  assert.equal(
    fs.readFileSync(path.join(f.run, 'verdict'), 'utf8'),
    'GATE FLAKY-UNDER-LOAD (exit 1)\n',
  )
  assert.equal(fs.readFileSync(path.join(f.run, 'status'), 'utf8'), '1\n')
  assert.match(result.stdout, /isolation:.*fixture.test.mjs/)
  assert.match(result.stdout, /\nload: /)
  assert.match(result.stdout.trim().split('\n').at(-1), /^evidence:/)
  const before = fs
    .readdirSync(f.run)
    .map((name) => [name, fs.statSync(path.join(f.run, name)).mtimeMs])
  assert.match(call(['stop', f.run]).stdout, /^GATE FLAKY-UNDER-LOAD/)
  assert.deepEqual(
    fs.readdirSync(f.run).map((name) => [name, fs.statSync(path.join(f.run, name)).mtimeMs]),
    before,
  )
})
test('always failing target reproduces alone', (t) => {
  const f = fixture(t, `test('fails', () => { throw Error('always') })`)
  const result = f.wait()
  assert.equal(result.status, 1)
  assert.match(result.stdout, /^GATE FAILED \(exit 1\)/)
  assert.match(result.stdout, /isolation:.*reproduced alone/)
  assert.match(result.stdout, /\nload:/)
})
test('bare failing command finds no target and creates no rerun logs', (t) => {
  const f = fixture(t, `process.exit(1)`)
  // Run a genuinely bare command: node --test would attribute its file wrapper.
  const start = call(['start', '--', process.execPath, '-e', 'process.exit(1)'])
  const run = start.stdout.split('\n')[0]
  t.after(() => fs.rmSync(run, { recursive: true, force: true }))
  const result = call(['wait', '--timeout', '10000', run])
  assert.equal(result.status, 1)
  assert.match(result.stdout, /isolation:.*no re-runnable failure target/)
  assert.equal(fs.existsSync(path.join(run, 'isolation')), false)
  f.wait()
})
test('opt-out keeps failure and creates no isolation directory', (t) => {
  const f = fixture(
    t,
    `test('marker', () => { const marker = new URL('./marker', import.meta.url); if (!fs.existsSync(marker)) { fs.writeFileSync(marker, '1'); throw Error('first run') } })`,
    { BOSS_GATE_ISOLATE: '0' },
  )
  const result = f.wait()
  assert.equal(result.status, 1)
  assert.equal(fs.existsSync(path.join(f.run, 'isolation')), false)
})
test('environment failure bypasses isolation and reports load', (t) => {
  const f = fixture(t, `console.error('No space left on device'); process.exit(1)`)
  const result = f.wait()
  assert.equal(result.status, 75, result.stdout)
  assert.match(result.stdout, /^GATE ENVIRONMENT FAILURE/)
  assert.match(result.stdout, /\nload:/)
  assert.equal(fs.existsSync(path.join(f.run, 'isolation')), false)
})
test('target timeout kills isolated child before terminal status', (t) => {
  const f = fixture(
    t,
    `test('slow rerun', async () => { const marker = new URL('./marker', import.meta.url); if (!fs.existsSync(marker)) { fs.writeFileSync(marker, '1'); throw Error('first') } await new Promise(r => setTimeout(r, 60000)) })`,
    { BOSS_GATE_ISOLATE_TARGET_TIMEOUT_MS: '100' },
  )
  const result = f.wait()
  assert.equal(result.status, 1, result.stdout)
  const pid = Number(fs.readFileSync(path.join(f.run, 'child-pid'), 'utf8'))
  assert.equal(alive(pid), false)
  assert.notEqual(
    JSON.parse(fs.readFileSync(path.join(f.run, 'isolation.json'))).decision,
    'flaky-under-load',
  )
})
test('stop during isolation reclaims the active rerun', (t) => {
  const f = fixture(
    t,
    `test('slow rerun', async () => { const marker = new URL('./marker', import.meta.url); if (!fs.existsSync(marker)) { fs.writeFileSync(marker, '1'); throw Error('first') } fs.writeFileSync(new URL('./rerunning', import.meta.url), '1'); await new Promise(r => setTimeout(r, 60000)) })`,
  )
  until(() => fs.existsSync(path.join(f.dir, 'rerunning')))
  const pid = Number(fs.readFileSync(path.join(f.run, 'child-pid'), 'utf8'))
  assert.equal(fs.existsSync(path.join(f.run, 'status')), false)
  const pending = call(['wait', '--timeout', '1', f.run])
  assert.match(pending.stdout, /isolation.*log.*ago/)
  assert.match(call(['stop', f.run]).stdout, /^GATE STOPPED/)
  assert.equal(f.wait().status, 96)
  assert.equal(alive(pid), false)
})
test('target cap prevents every rerun', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-isolation-cap-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const files = [1, 2].map((n) => {
    const f = path.join(dir, `${n}.test.mjs`)
    fs.writeFileSync(f, `import test from 'node:test'; test('fails', () => {throw Error('red')})`)
    return f
  })
  const start = call(['start', '--', process.execPath, '--test', ...files], {
    BOSS_GATE_ISOLATE_MAX_TARGETS: '1',
  })
  const run = start.stdout.split('\n')[0]
  t.after(() => fs.rmSync(run, { recursive: true, force: true }))
  const result = call(['wait', '--timeout', '10000', run])
  assert.equal(result.status, 1)
  assert.match(result.stdout, /not attempted - 2 targets exceed the cap/)
  assert.equal(fs.existsSync(path.join(run, 'isolation')), false)
})
test('legacy failed run without load samples reports unavailable', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-isolation-legacy-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  fs.writeFileSync(path.join(dir, 'status'), '1\n')
  fs.writeFileSync(path.join(dir, 'log'), 'error\n')
  assert.match(call(['wait', dir]).stdout, /\nload: unavailable/)
})

test('supervised gate and nested wrappers inherit the pool without creating a cache', (t) => {
  const f = fixture(
    t,
    `
    import {spawnSync} from 'node:child_process';
    console.log('pool=' + process.env.BOSS_GATE_LINT_CACHE_POOL + ',cache=' + process.env.GOLANGCI_LINT_CACHE);
    const nested = spawnSync(process.execPath, ['scripts/test-infra/run-gate.mjs', '--', process.execPath, '-e', 'console.log("nested=" + process.env.BOSS_GATE_LINT_CACHE_POOL + ",cache=" + process.env.GOLANGCI_LINT_CACHE)'], {encoding:'utf8'});
    process.stdout.write(nested.stdout);
    if (nested.status !== 0) process.exit(1);
  `,
    { GOLANGCI_LINT_CACHE: '', BOSS_GATE_PROCESS_GROUP: '' },
  )
  const result = f.wait()
  assert.equal(result.status, 0, result.stdout)
  const log = fs.readFileSync(path.join(f.run, 'log'), 'utf8')
  assert.match(log, /pool=1,cache=\n/)
  assert.match(log, /nested=1,cache=\n/)
})

test('supervised wall-clock deadline covers a child cleanup hang', (t) => {
  const f = fixture(t, `console.log('cleanup started'); setInterval(() => {}, 1000)`, {
    BOSS_GATE_TIMEOUT_MS: '500',
    BOSS_GATE_ISOLATE: '0',
  })
  const result = f.wait()
  assert.equal(result.status, 1, result.stdout)
  assert.match(result.stdout, /^GATE FAILED \(exit 124\)/)
  assert.match(fs.readFileSync(path.join(f.run, 'log'), 'utf8'), /wall-clock deadline/)
})

test('stop reclaims resistant nested wrapper descendants before terminal status', (t) => {
  const f = fixture(
    t,
    `
    import {spawn} from 'node:child_process';
    const code = 'process.on("SIGTERM", () => {}); console.log(JSON.stringify({pid: process.pid})); setInterval(() => {}, 1000)';
    const child = spawn(process.execPath, ['scripts/test-infra/run-gate.mjs', '--', process.execPath, '-e', code]);
    child.stdout.once('data', data => fs.writeFileSync(new URL('./nested', import.meta.url), data));
    await new Promise(() => {});
  `,
    { GOLANGCI_LINT_CACHE: '', BOSS_GATE_PROCESS_GROUP: '1' },
  )
  until(() => fs.existsSync(path.join(f.dir, 'nested')))
  const { pid } = JSON.parse(fs.readFileSync(path.join(f.dir, 'nested'), 'utf8'))
  assert.match(call(['stop', f.run]).stdout, /^GATE STOPPED/)
  assert.equal(f.wait().status, 96)
  assert.equal(alive(pid), false)
})

test('supervised deadline outlives a successful leader with resistant descendants', (t) => {
  const f = fixture(
    t,
    `
    import {spawn} from 'node:child_process';
    const child = spawn(process.execPath, ['-e', 'process.on("SIGTERM", () => {}); console.log("ready"); setInterval(() => {}, 1000)']);
    child.stdout.once('data', () => {
      fs.writeFileSync(new URL('./descendant', import.meta.url), JSON.stringify({pid:child.pid}));
      process.exit(0);
    });
  `,
    { BOSS_GATE_TIMEOUT_MS: '3000', BOSS_GATE_ISOLATE: '0', GOLANGCI_LINT_CACHE: '' },
  )
  const result = f.wait()
  assert.equal(result.status, 1, result.stdout)
  assert.match(result.stdout, /^GATE FAILED \(exit 124\)/)
  const { pid } = JSON.parse(fs.readFileSync(path.join(f.dir, 'descendant'), 'utf8'))
  assert.equal(alive(pid), false)
})

test('stopping descendants after a successful leader exit cannot report a pass', (t) => {
  const f = fixture(
    t,
    `
    import {spawn} from 'node:child_process';
    const child = spawn(process.execPath, ['-e', 'process.on("SIGTERM", () => {}); console.log("ready"); setInterval(() => {}, 1000)']);
    child.stdout.once('data', () => {
      fs.writeFileSync(new URL('./descendant', import.meta.url), String(child.pid));
      process.exit(0);
    });
  `,
    { BOSS_GATE_ISOLATE: '0' },
  )
  until(() => fs.existsSync(path.join(f.dir, 'descendant')))
  until(() => !alive(Number(fs.readFileSync(path.join(f.run, 'child-pid'), 'utf8'))))
  assert.match(call(['stop', f.run]).stdout, /^GATE STOPPED/)
  assert.equal(f.wait().status, 96)
  assert.equal(alive(Number(fs.readFileSync(path.join(f.dir, 'descendant'), 'utf8'))), false)
})
