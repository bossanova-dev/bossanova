// Scenario coverage for plan-secret-scan.mjs.
//
// Every secret-shaped fixture is ASSEMBLED AT RUNTIME by concatenation, so no secret-shaped literal
// is ever committed: the repo's own secret scanners — and this scanner run over the repo — would
// otherwise flag this file.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

import { ROTATION_LINE, scanFiles, scanText } from './plan-secret-scan.mjs'

const HELPER = fileURLToPath(new URL('./plan-secret-scan.mjs', import.meta.url))
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'plan-secret-scan-'))
after(() => fs.rmSync(dir, { recursive: true, force: true }))

const rep = (s, n) => s.repeat(n)
const alnum = (n) => rep('A1b2C3d4E5', Math.ceil(n / 10)).slice(0, n)
const b64 = (n) => rep('QUJDRA', Math.ceil(n / 6)).slice(0, n)

// name → [text, expected kind]
const SHAPES = {
  'github fine-grained': ['github' + '_pat_' + alnum(40), 'github-token'],
  'github classic': ['token g' + 'hp_' + alnum(36) + ' leaked', 'github-token'],
  jwt: ['e' + 'yJ' + alnum(20) + '.e' + 'yJ' + alnum(20) + '.' + alnum(20), 'jwt'],
  'provider sk key': ['KEY=s' + 'k-proj-' + alnum(40), 'provider-sk-key'],
  slack: ['xo' + 'xb-' + '123456789012-' + alnum(20), 'slack-token'],
  'aws access key id': ['id AK' + 'IA' + 'ABCDEFGHIJ234567', 'aws-access-key-id'],
  linear: ['li' + 'n_api_' + alnum(40), 'linear-api-key'],
  'stripe live': ['s' + 'k_live_' + alnum(24), 'stripe-key'],
  'stripe restricted': ['r' + 'k_live_' + alnum(24), 'stripe-key'],
  'labelled aws secret': ['aws_secret' + '_access_key = ' + alnum(40), 'aws-secret-access-key'],
  bearer: ['Authorization: ' + 'Bearer ' + alnum(32), 'bearer-token'],
  'url with literal password': [
    'postgres://app:' + 'hunter2' + 'Secret@db.example.invalid/x',
    'url-password',
  ],
  'signed upload url': [
    'https://uploads.example.invalid/a/b.png?' + 'signature=' + alnum(40),
    'signed-url-query',
  ],
  'amz signature': [
    'https://s3.example.invalid/k?X-Amz-' + 'Signature=' + alnum(64),
    'signed-url-query',
  ],
  'pem private key': [
    '-----BEGIN ' +
      'RSA PRIVATE KEY-----\n' +
      b64(64) +
      '\n' +
      b64(40) +
      '\n-----END RSA PRIVATE KEY-----',
    'private-key',
  ],
}

function write(name, text) {
  const file = path.join(dir, name)
  fs.writeFileSync(file, text)
  return file
}

function cli(...files) {
  const res = spawnSync(process.execPath, [HELPER, ...files], { encoding: 'utf8' })
  return { code: res.status, out: res.stdout }
}

for (const [name, [secret, kind]] of Object.entries(SHAPES)) {
  test(`flags ${name} as ${kind} on the right line, value masked, with the rotation line`, () => {
    const file = write(
      `${kind}-${name.replace(/\W+/g, '-')}.md`,
      `# plan\n\nprose line\n${secret}\n`,
    )
    const { code, out } = cli(file)
    assert.equal(code, 1, out)
    assert.match(out, new RegExp(`:4:${kind} \\[value masked\\]`))
    for (const token of secret.split(/[\s=:/?@.\n]+/).filter((t) => t.length >= 16)) {
      assert.ok(!out.includes(token), `output must never echo the matched value (${name})`)
    }
    assert.ok(out.includes(ROTATION_LINE), 'a hit carries the rotation line')
  })
}

test('bare words and documented redaction forms pass', () => {
  const text = [
    'Rotate the token, keep the secret out of logs, never store a password.',
    'Use token=REDACTED or token=[REDACTED] or token=[REDACTED:%20vault] on image URLs.',
    'Env lives in [REDACTED: repo-root .env].',
    '![shot](https://uploads.example.invalid/abc/def.png)',
    'https://uploads.example.invalid/abc/def.png?token=REDACTED',
    'Authorization: Bearer $TOKEN and Authorization: Bearer ${API_KEY}',
    'a kebab slug ask-for-the-long-running-migration-plan and disk-cleanup-for-everything',
  ].join('\n')
  assert.deepEqual(scanText(text), [])
  assert.equal(cli(write('clean.md', text)).code, 0)
})

test("the repo's real placeholder shapes pass", () => {
  const text = [
    'postgres://app:' + 'REPLACE_ME' + '@localhost:5432/db',
    'postgres://${' + 'var.db_user}:${' + 'var.db_password}@${' + 'var.db_host}/app',
    'redis://user:' + 'pass' + '@localhost:6379',
    'amqp://guest:' + 'password' + '@rabbit',
    'mysql://root:' + '<password>' + '@db',
    '-----BEGIN ' + 'PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----',
  ].join('\n')
  assert.deepEqual(scanText(text), [])
})

test('an unsigned upload URL passes while the same URL with a signature is a hit', () => {
  const unsigned = 'https://uploads.example.invalid/a/b.png'
  assert.deepEqual(scanText(unsigned), [])
  assert.deepEqual(
    scanText(unsigned + '?' + 'signature=' + alnum(48)).map((h) => h.kind),
    ['signed-url-query'],
  )
})

test('an unlabelled 40-char string is not an AWS secret; the labelled form is', () => {
  assert.deepEqual(scanText('value ' + alnum(40)), [])
  assert.deepEqual(
    scanText('AWS_SECRET' + '_ACCESS_KEY: "' + alnum(40) + '"').map((h) => h.kind),
    ['aws-secret-access-key'],
  )
})

test('a missing file and no arguments both exit 2 (the gate fails closed)', () => {
  assert.equal(cli(path.join(dir, 'does-not-exist.md')).code, 2)
  assert.equal(cli().code, 2)
  assert.equal(scanFiles([]).code, 2)
})

test('an unreadable file fails closed even when another file has a hit', () => {
  const hit = write('hit-with-missing.md', SHAPES.linear[0])
  assert.equal(cli(hit, path.join(dir, 'missing.md')).code, 2)
})

test('two files where only the second has a hit exit 1 naming the second', () => {
  const clean = write('first.md', 'nothing here\n')
  const second = write('second.md', 'ok\n' + SHAPES['stripe live'][0] + '\n')
  const { code, out } = cli(clean, second)
  assert.equal(code, 1)
  assert.ok(out.includes(`${second}:2:stripe-key`))
  assert.ok(!out.includes(`${clean}:`))
})

test('a 1 MB file with no newlines scans quickly (bounded quantifiers)', () => {
  const big = rep('sk-aaaa?token=:// eyJ-----BEGIN ', 40000).slice(0, 1024 * 1024)
  const started = process.hrtime.bigint()
  scanText(big)
  const ms = Number(process.hrtime.bigint() - started) / 1e6
  assert.ok(ms < 2000, `scan took ${ms}ms`)
})
