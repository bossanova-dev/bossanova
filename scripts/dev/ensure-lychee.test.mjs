import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const helper = fileURLToPath(new URL('./ensure-lychee.sh', import.meta.url))
const version = '0.24.2'
const output = 'lychee 0.24.2'

function fixture(t, { builder = true } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ensure-lychee-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const bin = path.join(dir, 'bin')
  fs.mkdirSync(bin)
  const write = (file, body) => fs.writeFileSync(file, `#!/bin/sh\n${body}\n`, { mode: 0o755 })
  // Isolate PATH so the absent-builder case cannot find a host installation.
  for (const name of [
    'sh',
    'dirname',
    'uname',
    'mkdir',
    'mktemp',
    'rm',
    'mv',
    'chmod',
    'awk',
    'tar',
    'gzip',
    'shasum',
    'cp',
    'touch',
    'find',
  ]) {
    const resolved = spawnSync('/bin/sh', ['-c', `command -v ${name}`], {
      encoding: 'utf8',
    }).stdout.trim()
    fs.symlinkSync(resolved, path.join(bin, name))
  }
  write(path.join(bin, 'sleep'), 'exit 0')
  const source = path.join(dir, 'source')
  fs.mkdirSync(source)
  const nested = 'lychee-platform'
  fs.mkdirSync(path.join(source, nested), { recursive: true })
  write(path.join(source, nested, 'lychee'), `echo "${output}"`)
  const archive = path.join(dir, 'release.tar.gz')
  const pack = spawnSync('tar', ['-czf', archive, '-C', source, nested || 'lychee'], {
    encoding: 'utf8',
  })
  assert.equal(pack.status, 0, pack.stderr)
  const asset = `lychee-${os.arch() === 'arm64' ? 'aarch64' : 'x86_64'}-${os.type() === 'Darwin' ? 'apple-darwin' : 'unknown-linux-gnu'}.tar.gz`
  const sums = path.join(dir, 'checksums.txt')
  function checksum(hash = createHash('sha256').update(fs.readFileSync(archive)).digest('hex')) {
    fs.writeFileSync(sums, `${hash}  ${asset}\n`)
  }
  checksum()
  const calls = path.join(dir, 'calls')
  const env = { PATH: bin, ARCHIVE: archive, SUMS: sums, CALLS: calls }
  write(
    path.join(bin, 'curl'),
    `echo "curl $*" >> "$CALLS"
[ -z "$CURL_FAIL" ] || exit 22
case "$2" in
  *.sha256) [ -z "$SUM_FAIL" ] || exit 22; cp "$SUMS" "$4" ;;
  *) cp "$ARCHIVE" "$4" ;;
esac`,
  )
  if (builder)
    write(
      path.join(bin, 'cargo'),
      `echo "cargo $*" >> "$CALLS"
[ -z "$BUILD_FAIL" ] || exit 9
[ "$1 $2 $3 $4 $5 $6" = 'install --locked lychee --version 0.24.2 --root' ] || exit 8
mkdir -p "$7/bin"
if [ -n "$WRONG_VERSION" ]; then text='lychee 0.24.1'; else text='lychee 0.24.2'; fi
printf '#!/bin/sh\necho "%s"\n' "$text" > "$7/bin/lychee"
chmod +x "$7/bin/lychee"`,
    )
  const dest = path.join(dir, 'cache', 'lychee')
  return {
    dir,
    dest,
    env,
    checksum,
    calls: () => (fs.existsSync(calls) ? fs.readFileSync(calls, 'utf8') : ''),
    seed: (text = output) => {
      fs.mkdirSync(path.dirname(dest), { recursive: true })
      write(dest, `echo '${text}'`)
    },
    run: (operation = 'install') =>
      spawnSync('/bin/sh', [helper, operation, version, dest], { env, encoding: 'utf8' }),
  }
}

function success(result) {
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout, '')
}

test('release installs a verified binary and a repeated install is offline', (t) => {
  const f = fixture(t)
  success(f.run())
  success(f.run('check'))
  assert.doesNotMatch(f.calls(), /cargo /)
  const first = f.calls()
  success(f.run())
  assert.equal(f.calls(), first)
  assert.deepEqual(fs.readdirSync(path.dirname(f.dest)), ['lychee'])
})

test('exhausted downloads build the pinned tool from source', (t) => {
  const f = fixture(t)
  f.env.CURL_FAIL = '1'
  success(f.run())
  success(f.run('check'))
  assert.match(f.calls(), /cargo install --locked lychee --version 0\.24\.2 --root /)
  assert.deepEqual(fs.readdirSync(path.dirname(f.dest)), ['lychee'])
})

test('a partial release download uses only the completed source build', (t) => {
  const f = fixture(t)
  f.env.SUM_FAIL = '1'
  success(f.run())
  success(f.run('check'))
  assert.match(f.calls(), /cargo install /)
})

for (const existing of [false, true]) {
  test(`checksum mismatch never builds and preserves ${existing ? 'the existing binary' : 'an absent destination'}`, (t) => {
    const f = fixture(t)
    if (existing) f.seed('old tool')
    const before = existing ? fs.readFileSync(f.dest) : null
    f.checksum('0'.repeat(64))
    const result = f.run()
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /checksum/)
    assert.doesNotMatch(f.calls(), /cargo /)
    assert.equal(fs.existsSync(f.dest), existing)
    if (existing) assert.deepEqual(fs.readFileSync(f.dest), before)
    assert.deepEqual(fs.readdirSync(path.dirname(f.dest)), existing ? ['lychee'] : [])
  })
}

test('wrong source version leaves an existing destination untouched', (t) => {
  const f = fixture(t)
  f.seed('old tool')
  const before = fs.readFileSync(f.dest)
  f.env.CURL_FAIL = '1'
  f.env.WRONG_VERSION = '1'
  assert.notEqual(f.run().status, 0)
  assert.deepEqual(fs.readFileSync(f.dest), before)
  assert.deepEqual(fs.readdirSync(path.dirname(f.dest)), ['lychee'])
})

test('failure of both sources is nonzero and names both', (t) => {
  const f = fixture(t)
  f.env.CURL_FAIL = '1'
  f.env.BUILD_FAIL = '1'
  const result = f.run()
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /both release.*cargo/s)
  assert.equal(fs.existsSync(f.dest), false)
})

test('a missing cargo reports the unavailable fallback', (t) => {
  const f = fixture(t, { builder: false })
  f.env.CURL_FAIL = '1'
  const result = f.run()
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /fallback unavailable: cargo not on PATH/)
  assert.equal(fs.existsSync(f.dest), false)
})
