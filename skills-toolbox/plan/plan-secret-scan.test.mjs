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

import {
  HOSTNAME_LINE,
  INTERNAL_HOSTNAME_SHAPES,
  ROTATION_LINE,
  SHAPES as CREDENTIAL_SHAPES,
  scanFiles,
  scanText,
} from './plan-secret-scan.mjs'

const HELPER = fileURLToPath(new URL('./plan-secret-scan.mjs', import.meta.url))
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'plan-secret-scan-'))
after(() => fs.rmSync(dir, { recursive: true, force: true }))

const rep = (s, n) => s.repeat(n)
const alnum = (n) => rep('A1b2C3d4E5', Math.ceil(n / 10)).slice(0, n)
const b64 = (n) => rep('QUJDRA', Math.ceil(n / 6)).slice(0, n)
const host = (...parts) => parts.join('.')

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

function findUnboundedQuantifier(source) {
  let inClass = false
  for (let i = 0; i < source.length; i += 1) {
    const ch = source[i]
    if (ch === '\\') {
      i += 1
      continue
    }
    if (inClass) {
      if (ch === ']') inClass = false
      continue
    }
    if (ch === '[') {
      inClass = true
      continue
    }
    if (ch === '*' || ch === '+') return ch
    if (ch === '{') {
      const match = source.slice(i).match(/^\{\d+,\}/)
      if (match) return match[0]
    }
  }
  return null
}

test('every exported scan shape uses only bounded quantifiers', () => {
  for (const shape of [...CREDENTIAL_SHAPES, ...INTERNAL_HOSTNAME_SHAPES]) {
    assert.equal(
      findUnboundedQuantifier(shape.re.source),
      null,
      `${shape.kind} regex must bound every quantifier`,
    )
  }
})

test(
  'a 1 MB file with no newlines scans cleanly with hostnames on (bounded quantifiers)',
  { timeout: 30_000 },
  () => {
    const big = rep('sk-aaaa?token=:// eyJ-----BEGIN ', 40000).slice(0, 1024 * 1024)
    assert.deepEqual(scanText(big, { hostnames: true }), [])
  },
)

test(
  'a 1 MB hostname-heavy line with no suffix scans cleanly (bounded quantifiers)',
  { timeout: 30_000 },
  () => {
    const big = (
      rep('aa1.bb2.cc3.dd4.', 40000).slice(0, 512 * 1024) +
      ' ' +
      rep('alpha1.beta2.gamma3.delta4.epsilon5.zeta6.eta7.theta8 ', 12000)
    ).slice(0, 1024 * 1024)
    assert.deepEqual(scanText(big, { hostnames: true }), [])
  },
)

// Fictional labels assembled at runtime so no non-exempt hostname literal is committed.
const INTERNAL_HOST_CASES = {
  'two-label .lan in prose': [host('nas01', 'lan'), (h) => `backups target ${h} nightly`],
  'two-label .local': [host('printer01', 'local'), (h) => `reach ${h} on the office subnet`],
  'two-label .internal': [host('collector', 'internal'), (h) => `metrics push to ${h}`],
  'two-label .localdomain': [host('mx01', 'localdomain'), (h) => `mail relay ${h}`],
  'two-label .home.arpa': [host('router01', 'home', 'arpa'), (h) => `gateway ${h} resolves dns`],
  'multi-label .internal': [host('api', 'staging', 'eu', 'internal'), (h) => `deploy to ${h}`],
  'URL authority with port and path': [
    host('buildbox', 'lan'),
    (h) => `webhook https://${h}:8080/hooks/run`,
  ],
  'user@ host': [host('worker02', 'internal'), (h) => `ssh deploy@${h}`],
  'sentence-ending period': [host('nas02', 'lan'), (h) => `Primary box is ${h}.`],
  'substring of placeholder word (server)': [host('webserver', 'lan'), (h) => `route via ${h}`],
  'substring of placeholder word (foo)': [host('foobox', 'local'), (h) => `sync with ${h}`],
  'substring of placeholder word (host)': [host('my-host-01', 'internal'), (h) => `ssh ${h}`],
}

for (const [name, [hostname, format]] of Object.entries(INTERNAL_HOST_CASES)) {
  test(`flags ${name} as internal-hostname, masked, with hostname line only`, () => {
    const file = write(
      `host-${name.replace(/\W+/g, '-')}.md`,
      `# plan\n\nprose line\n${format(hostname)}\n`,
    )
    const { code, out } = cli(file)
    assert.equal(code, 1, out)
    assert.match(out, /:4:internal-hostname \[value masked\]/)
    assert.ok(!out.includes(hostname), `output must never echo the hostname (${name})`)
    assert.ok(out.includes(HOSTNAME_LINE), 'a hostname hit carries the hostname line')
    assert.ok(!out.includes(ROTATION_LINE), 'a hostname-only hit must not carry the rotation line')
  })
}

test('mixed credential and hostname file reports both kinds and prints both guidance lines', () => {
  const hostname = host('nas01', 'lan')
  const file = write('mixed.md', `# plan\n${SHAPES.linear[0]}\nconnect to ${hostname}\n`)
  const { code, out } = cli(file)
  assert.equal(code, 1, out)
  assert.match(out, /:2:linear-api-key \[value masked\]/)
  assert.match(out, /:3:internal-hostname \[value masked\]/)
  assert.ok(!out.includes(hostname), 'output must never echo the hostname')
  assert.ok(out.includes(ROTATION_LINE), 'mixed file carries the rotation line')
  assert.ok(out.includes(HOSTNAME_LINE), 'mixed file carries the hostname line')
})

test('exempt and non-hostname forms pass when hostname scanning is enabled', () => {
  const text = [
    'Bare suffix mentions: (for example .lan/.local/.internal suffixes), .lan, *.local, \\*.internal.',
    'More bare suffixes: .localdomain and .home.arpa.',
    'Files and calls: settings.local.json, .env.local, config.local.ts, foo.local(), e.local.',
    `Non-placeholder call and paths: ${host('bravo', 'local')}(), /path/foo.local, /path/${host('bravo', 'local')}.`,
    `Disallowed left boundaries: $${host('bravo', 'lan')}, -${host('bravo', 'lan')}, prefix_${host('bravo', 'local')}.`,
    'Templates and redactions: <name>.lan, ${HOST}.local, [REDACTED: internal hostname].',
    'Placeholders: example.local, examples.lan, host.lan, hostname.local, myhost.internal, your-host.lan.',
    'More placeholders: server.local, machine.internal, foo.lan, bar.local, baz.internal, placeholder.localdomain, redacted.home.arpa.',
    'Case-insensitive and middle-label placeholders: Example.local, MYHOST.internal, app.example.local, db.placeholder.internal.',
    'Well-known: host.docker.internal, gateway.docker.internal, metadata.google.internal.',
    'Kubernetes service DNS: my-svc.default.svc.cluster.local, svc.cluster.local.',
    'Loopback: localhost, 127.0.0.1.',
    'Go qualified identifiers: codes.Internal, time.Local, element.Name.Local.',
  ].join('\n')
  assert.deepEqual(scanText(text, { hostnames: true }), [])
  assert.equal(cli(write('exempt-hostnames.md', text)).code, 0)
})

test('exempt hostname on the same line does not mask a real internal hostname', () => {
  const real = host('nas03', 'lan')
  assert.deepEqual(scanText(`replace example.local with ${real}`, { hostnames: true }), [
    { line: 1, kind: 'internal-hostname' },
  ])
})

test('scanText ignores hostnames by default and scanFiles allows opting out via deps', () => {
  const line = `connect to ${host('nas01', 'lan')}`
  assert.deepEqual(scanText(line), [])
  assert.deepEqual(scanText(line, { hostnames: false }), [])
  assert.deepEqual(scanFiles(['draft.md'], { read: () => line, hostnames: false }), {
    code: 0,
    lines: [],
  })
})

test('SHAPES contains no internal-hostname entry (preserves credential history-sweep boundary)', () => {
  assert.equal(
    CREDENTIAL_SHAPES.some((shape) => shape.kind === 'internal-hostname'),
    false,
  )
  assert.equal(
    INTERNAL_HOSTNAME_SHAPES.some((shape) => shape.kind === 'internal-hostname'),
    true,
  )
})

test('CLI ends with exactly one positive verdict for clean, redact and fail-closed exits', () => {
  for (const [files, code, verdict] of [
    [[write('verdict-clean.md', 'safe plan')], 0, 'clean'],
    [[write('verdict-hit.md', SHAPES.linear[0])], 1, 'redact'],
    [[write('verdict-hostname.md', `connect to ${host('nas01', 'lan')}`)], 1, 'redact'],
    [[path.join(dir, 'verdict-missing.md')], 2, 'fail-closed'],
    [[], 2, 'fail-closed'],
  ]) {
    const result = cli(...files)
    assert.equal(result.code, code)
    const lines = result.out.trimEnd().split('\n')
    assert.equal(lines.at(-1), `plan-secret-scan: verdict=${verdict}`)
    assert.equal(lines.filter((line) => line.startsWith('plan-secret-scan: verdict=')).length, 1)
  }
})
