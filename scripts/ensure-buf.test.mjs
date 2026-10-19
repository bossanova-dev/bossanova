import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const root = fileURLToPath(new URL('..', import.meta.url))
const helper = path.join(root, 'scripts/ensure-buf.sh')
const version = '1.66.1'
function writeExecutable(file, body) {
  fs.writeFileSync(file, `#!/bin/sh\n${body}\n`, { mode: 0o755 })
}
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ensure-buf-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const bin = path.join(dir, 'bin')
  fs.mkdirSync(bin)
  const dest = path.join(dir, `buf-${version}`, 'buf')
  const calls = path.join(dir, 'calls')
  const download = path.join(dir, 'download')
  const sums = path.join(dir, 'sha256.txt')
  const curlLog = path.join(dir, 'curl-log')
  const env = {
    ...process.env,
    PATH: `${bin}:${process.env.PATH}`,
    CALLS: calls,
    DOWNLOAD: download,
    SUMS: sums,
    CURL_LOG: curlLog,
  }
  writeExecutable(
    download,
    `if [ "$1" = --version ]; then echo "\${DOWNLOAD_VERSION:-${version}}"; else echo "$*" >> "$CALLS"; fi`,
  )
  const asset = `buf-${os.type()}-${spawnSync('uname', ['-m'], { encoding: 'utf8' }).stdout.trim()}`
  function checksum(value = createHash('sha256').update(fs.readFileSync(download)).digest('hex')) {
    fs.writeFileSync(sums, `${value}  ${asset}\n`)
  }
  checksum()
  writeExecutable(
    path.join(bin, 'curl'),
    `echo "$*" >> "$CURL_LOG"
[ -z "$CURL_FAIL" ] || exit 22
case "$2" in */sha256.txt) cp "$SUMS" "$4" ;; *) cp "$DOWNLOAD" "$4" ;; esac`,
  )
  // Keep bounded retry behavior without spending 15 seconds on synthetic network failures.
  writeExecutable(path.join(bin, 'sleep'), 'exit 0')
  function run(args) {
    return spawnSync('sh', [helper, ...args, version, dest], { env, encoding: 'utf8' })
  }
  function seed(v = version) {
    fs.mkdirSync(path.dirname(dest), { recursive: true })
    writeExecutable(dest, `if [ "$1" = --version ]; then echo ${v}; else echo "$*" >> "$CALLS"; fi`)
  }
  function make(args) {
    return spawnSync('make', ['-s', ...args, `BUF_CACHE_DIR=${dir}`], {
      cwd: root,
      env,
      encoding: 'utf8',
    })
  }
  return { dir, dest, calls, curlLog, env, checksum, run, seed, make }
}
function succeeded(result) {
  assert.equal(result.status, 0, result.stdout + result.stderr)
}

test('check distinguishes a matching, mismatching, absent and non-executable binary', (t) => {
  const f = fixture(t)
  assert.equal(f.run(['check']).status, 3)
  f.seed('v1.66.1')
  succeeded(f.run(['check']))
  f.seed('1.68.2')
  const wrong = f.run(['check'])
  assert.equal(wrong.status, 1)
  assert.ok(wrong.stderr.includes(f.dest))
  assert.match(wrong.stderr, /want 1\.66\.1.*got 1\.68\.2/s)
  fs.chmodSync(f.dest, 0o644)
  assert.equal(f.run(['check']).status, 3)
})

test('install verifies and installs once; the second call is offline', (t) => {
  const f = fixture(t)
  succeeded(f.run(['install']))
  succeeded(f.run(['check']))
  const first = fs.readFileSync(f.curlLog, 'utf8')
  assert.equal(first.trim().split('\n').length, 2)
  succeeded(f.run(['install']))
  assert.equal(fs.readFileSync(f.curlLog, 'utf8'), first)
  assert.deepEqual(fs.readdirSync(path.dirname(f.dest)), ['buf'])
})

for (const failure of ['checksum', 'version', 'download']) {
  test(`install leaves no destination or temporary files after ${failure} failure`, (t) => {
    const f = fixture(t)
    if (failure === 'checksum') f.checksum('0'.repeat(64))
    if (failure === 'version') f.env.DOWNLOAD_VERSION = '1.68.2'
    if (failure === 'download') f.env.CURL_FAIL = '1'
    assert.notEqual(f.run(['install']).status, 0)
    assert.equal(fs.existsSync(f.dest), false)
    assert.deepEqual(fs.readdirSync(path.dirname(f.dest)), [])
  })
}

test('failed download preserves an existing destination', (t) => {
  const f = fixture(t)
  f.seed('1.68.2')
  const before = fs.readFileSync(f.dest)
  f.env.CURL_FAIL = '1'
  assert.notEqual(f.run(['install']).status, 0)
  assert.deepEqual(fs.readFileSync(f.dest), before)
  assert.deepEqual(fs.readdirSync(path.dirname(f.dest)), ['buf'])
})

test('make proto targets use only the pinned binary despite a shadowing host buf', (t) => {
  const f = fixture(t)
  f.seed()
  const hostLog = path.join(f.dir, 'host-calls')
  writeExecutable(path.join(f.dir, 'bin/buf'), `echo "$*" >> '${hostLog}'; echo 1.68.2`)
  for (const target of ['buf-check-version', 'lint-proto', 'lint-proto-breaking']) {
    succeeded(f.make([target]))
  }
  assert.equal(fs.existsSync(hostLog), false)
  const calls = fs.readFileSync(f.calls, 'utf8')
  assert.match(calls, /^lint$/m)
  assert.match(calls, /^breaking --against /m)
})

test('make rejects a wrong-version override without overwriting it', (t) => {
  const f = fixture(t)
  f.seed('1.68.2')
  const before = fs.readFileSync(f.dest)
  const result = f.make(['buf-check-version', `BUF=${f.dest}`])
  assert.notEqual(result.status, 0)
  assert.ok((result.stdout + result.stderr).includes(f.dest))
  assert.match(result.stdout + result.stderr, /want 1\.66\.1/)
  assert.deepEqual(fs.readFileSync(f.dest), before)
  assert.equal(fs.existsSync(f.curlLog), false)
})

test('dry-run recipes resolve all buf commands to the pin, including the guarded stamp', (t) => {
  const f = fixture(t)
  // -n keeps this recipe inspection from regenerating tracked output.
  for (const target of ['generate', 'lint', 'lint-all', 'deps', '.generate.stamp']) {
    const result = f.make(['-n', '-B', target])
    succeeded(result)
    const output = result.stdout
    assert.doesNotMatch(
      output,
      /command -v buf|(?:^|[;\n]|-- )\s*buf (?:lint|generate|breaking|--version)/m,
    )
    assert.ok(output.includes(f.dest), `${target} must resolve the pinned binary`)
    if (target === '.generate.stamp') {
      assert.doesNotMatch(output, /ensure-buf\.sh.*install|curl /)
      assert.match(output, /ensure-buf\.sh.*check/)
      assert.match(output, /3\).*committed gen used/s)
      assert.match(output, /touch .*generate\.stamp/)
      const firstSync = output.indexOf('sync-generated-tree.mjs')
      assert.ok(firstSync >= 0)
      assert.ok(output.indexOf('exit 0 ;;') < firstSync)
      assert.ok(output.indexOf('*) exit $result ;;') < firstSync)
      assert.ok(output.indexOf('generate -o') >= 0)
      assert.ok(output.indexOf('generate -o') < firstSync)
      assert.ok(
        output.lastIndexOf('sync-generated-tree.mjs') < output.indexOf('touch .generate.stamp'),
      )
      assert.doesNotMatch(
        output,
        /find (?:lib\/bossalib\/gen|services\/web\/src\/gen)[^;\n]*-delete/,
      )
    }
  }
})
