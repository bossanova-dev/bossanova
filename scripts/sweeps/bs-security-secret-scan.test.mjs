import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { after, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { scanBlobText, groupHits, toFindingRecords } from './bs-security-secret-scan.mjs'

const cli = fileURLToPath(new URL('./bs-security-secret-scan.mjs', import.meta.url))
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'history-scan-test-'))
after(() => fs.rmSync(root, { recursive: true, force: true }))
const value = 'gh' + 'p_' + 'AbC123def456GHI789jkl012MNO345pqr678'
const hash = createHash('sha256').update(value).digest('hex').slice(0, 12)
let serial = 0
function git(repo, ...args) {
  const r = spawnSync('git', ['-C', repo, ...args], { encoding: 'utf8' })
  assert.equal(r.status, 0, 'fixture git operation succeeded')
  return r.stdout.trim()
}
function fixture(files = { 'sample.txt': value }, message = 'fixture') {
  const dir = path.join(root, String(++serial))
  fs.mkdirSync(dir)
  git(dir, 'init', '-q')
  git(dir, 'config', 'commit.gpgsign', 'false')
  for (const [name, text] of Object.entries(files)) fs.writeFileSync(path.join(dir, name), text)
  git(dir, 'add', '.')
  git(
    dir,
    '-c',
    'user.name=Test',
    '-c',
    'user.email=test@example.invalid',
    'commit',
    '-qm',
    message,
  )
  const repo = dir + '-clone'
  const r = spawnSync('git', ['clone', '-q', dir, repo])
  assert.equal(r.status, 0)
  git(repo, 'config', 'commit.gpgsign', 'false')
  return repo
}
function run(repo, extra = [], command = 'scan') {
  const out = path.join(root, 'out-' + ++serial + '.json')
  const result = spawnSync(
    process.execPath,
    [cli, command, '--repo', repo, '--out', out, ...extra],
    { encoding: 'utf8' },
  )
  return {
    ...result,
    output: fs.existsSync(out) ? fs.readFileSync(out, 'utf8') : '',
    data: fs.existsSync(out) ? JSON.parse(fs.readFileSync(out, 'utf8')) : null,
  }
}
function valueFree(result, secret = value) {
  const text = result.stdout + result.stderr + result.output
  for (let i = 0; i <= secret.length - 8; i++) {
    // gate-region-ok: Enumerate credential substrings to prove byte-value absence, not guidance wording.
    assert.equal(text.includes(secret.slice(i, i + 8)), false, 'output is value-free')
  }
}
test('a planted credential is reported by kind and hash and never by value', () => {
  const r = run(fixture())
  assert.equal(r.status, 0)
  assert.equal(r.data.length, 1)
  assert.equal(r.data[0].evidence.kind, 'github-token')
  assert.equal(r.data[0].evidence.valueHash, hash)
  valueFree(r)
})
test('an allowlisted hash is suppressed and counted', () => {
  const allow = path.join(root, 'allow.json')
  fs.writeFileSync(
    allow,
    JSON.stringify({
      entries: [
        { kind: 'github-token', valueHash: hash, samplePath: 'sample.txt', reason: 'fixture' },
      ],
    }),
  )
  const r = run(fixture(), ['--allowlist', allow])
  assert.equal(r.status, 0)
  assert.deepEqual(r.data, [])
  assert.equal(JSON.parse(r.stdout).suppressed, 1)
})
test('two occurrences of one value become one record with occurrences 2', () => {
  const r = run(fixture({ 'sample.txt': value + '\n' + value }))
  assert.equal(r.status, 0)
  assert.equal(r.data.length, 1)
  assert.equal(r.data[0].evidence.occurrences, 2)
})
test('the fingerprint input does not depend on the sample path', () => {
  const hits = scanBlobText(value)
  const records = (samplePath) =>
    toFindingRecords(groupHits(hits.map((h) => ({ ...h, samplePath, blob: 'abc' }))))
  assert.equal(records('a.txt')[0].fingerprint, records('z.txt')[0].fingerprint)
  assert.equal(records('a.txt')[0].locations[0].path, 'git-history')
})
test('a value only in an unpushed local branch is not reported by default but is with all-objects', () => {
  const repo = fixture({ 'clean.txt': 'clean' })
  git(repo, 'checkout', '-qb', 'local-only')
  fs.writeFileSync(path.join(repo, 'local.txt'), value)
  git(repo, 'add', '.')
  git(
    repo,
    '-c',
    'user.name=Test',
    '-c',
    'user.email=test@example.invalid',
    'commit',
    '-qm',
    'local',
  )
  assert.deepEqual(run(repo).data, [])
  const deep = run(repo, ['--refs', 'all-objects'])
  assert.equal(deep.status, 0)
  assert.equal(deep.data.length, 1)
})
test('a credential in a commit message is reported with sample path commit-message', () => {
  const r = run(fixture({ 'clean.txt': 'clean' }, value))
  assert.equal(r.status, 0)
  assert.equal(r.data[0].evidence.samplePath, 'commit-message')
  valueFree(r)
})
test('binary and oversized blobs are skipped', () => {
  const r = run(fixture({ binary: '\0' + value, large: 'x'.repeat(200) + value }), [
    '--max-blob-bytes',
    '100',
  ])
  assert.equal(r.status, 0)
  assert.deepEqual(r.data, [])
  assert.equal(JSON.parse(r.stdout).blobsScanned, 0)
})
test('a placeholder rejected by the shared accept filter is not reported', () => {
  const r = run(fixture({ sample: 'postgres://app:password@db.example.invalid/x' }))
  assert.equal(r.status, 0)
  assert.deepEqual(r.data, [])
})
test('a missing allowlist warns and scans; a malformed one exits 2', () => {
  const repo = fixture()
  const missing = run(repo, ['--allowlist', path.join(root, 'missing.json')])
  assert.equal(missing.status, 0)
  assert.match(missing.stderr, /allowlist.*missing/)
  const bad = path.join(root, 'bad.json')
  fs.writeFileSync(bad, '{bad')
  const malformed = run(repo, ['--allowlist', bad])
  assert.equal(malformed.status, 2)
  valueFree(malformed)
  fs.writeFileSync(
    bad,
    JSON.stringify({
      entries: [
        {
          kind: 'github-token',
          valueHash: 123456789012,
          samplePath: 'sample.txt',
          reason: 'fixture',
        },
      ],
    }),
  )
  assert.equal(run(repo, ['--allowlist', bad]).status, 2)
})
test('baseline writes value-free candidate entries', () => {
  const r = run(fixture(), [], 'baseline')
  assert.equal(r.status, 0)
  assert.deepEqual(r.data.entries, [
    { kind: 'github-token', valueHash: hash, samplePath: 'sample.txt', reason: '' },
  ])
  valueFree(r)
})

test('numeric-only hashes retain distinct per-value fingerprints', () => {
  const groups = ['123456789012', '234567890123'].map((valueHash) => ({
    kind: 'github-token',
    valueHash,
    samplePath: 'sample.txt',
    line: 1,
    blob: 'abc',
    occurrences: 1,
  }))
  const records = toFindingRecords(groups)
  assert.notEqual(records[0].fingerprint, records[1].fingerprint)
})

test('credential-bearing sample paths are masked', () => {
  const r = run(fixture({ [value]: value }))
  assert.equal(r.status, 0)
  assert.equal(r.data[0].evidence.samplePath, 'redacted-path')
  valueFree(r)
})

const extraCases = [
  ['aws-temporary-access-key-id', 'AS' + 'IA' + 'ABCDEF1234567890'],
  ['google-api-key', 'AI' + 'za' + 'A1b2C3d'.repeat(5)],
  ['google-oauth-client-secret', 'GO' + 'CSPX-' + 'A1b2C3d4'.repeat(4)],
  ['stripe-webhook-secret', 'wh' + 'sec_' + 'A1b2C3d4'.repeat(4)],
  ['sentry-user-token', 'sn' + 'tryu_' + 'A1b2C3d4'.repeat(4)],
  ['terraform-cloud-token', 'A1b2C3d4' + '.atlas' + 'v1.' + 'A1b2C3d4'.repeat(4)],
  ['kubeconfig-client-key', 'client-key-' + 'data: ' + 'QUJD'.repeat(20)],
  [
    'private-key',
    '-----BEGIN ' + 'PRIVATE KEY-----\n' + 'QUJD'.repeat(20) + '\n-----END PRIVATE KEY-----',
  ],
  [
    'service-account-private-key',
    JSON.stringify({
      type: 'service_' + 'account',
      private_key:
        '-----BEGIN ' + 'PRIVATE KEY-----\n' + 'QUJD'.repeat(20) + '\n-----END PRIVATE KEY-----',
    }),
  ],
]
for (const [kind, text] of extraCases)
  test(`reports extra shape ${kind}`, () => {
    const r = run(fixture({ 'sample.txt': text }))
    assert.equal(r.status, 0)
    assert.equal(
      r.data.some(
        (rec) => rec.evidence.kind === kind && rec.locationStatus === 'location-verified',
      ),
      true,
    )
  })

test('URL and signature hashes identify credentials rather than metadata captures', () => {
  const secret = 'aVery' + 'SpecificPassword123'
  const text = 'postgres://user:' + secret + '@example.invalid/x?signature=' + secret
  const r = run(fixture({ 'sample.txt': text }))
  assert.equal(r.status, 0)
  const expected = createHash('sha256').update(secret).digest('hex').slice(0, 12)
  assert.deepEqual(
    r.data.map((rec) => rec.evidence.valueHash),
    [expected, expected],
  )
  valueFree(r, secret)
})

test('CLI errors never quote secret-bearing arguments or paths', () => {
  const r = run(fixture(), ['--' + value, value])
  assert.equal(r.status, 2)
  valueFree(r)
})
