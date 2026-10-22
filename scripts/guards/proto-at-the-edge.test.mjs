import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import test from 'node:test'
import { checkProtoAtTheEdge } from './proto-at-the-edge.mjs'
import { splitBaselineEntry } from './baseline.mjs'

const PROTO = 'github.com/recurser/bossalib/gen/bossanova/v1'
const source = (pkg) => `package x\nimport _ "${pkg}"\n`
function write(root, file, text) {
  fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
  fs.writeFileSync(path.join(root, file), text)
  execFileSync('git', ['add', file], { cwd: root })
}
const check = (repoRoot, write = false) =>
  checkProtoAtTheEdge({ repoRoot, baselinePath: 'baseline.json', write })
function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'proto-at-the-edge-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  execFileSync('git', ['init', '-q'], { cwd: root })
  write(root, '.golangci.yml', fs.readFileSync('.golangci.yml', 'utf8'))
  write(root, 'baseline.json', '[]\n')
  assert.deepEqual(check(root, true).failures, [])
  return root
}
test('proto-at-the-edge rejects root and subpackage imports across current and future packages', (t) => {
  const root = fixture(t)
  for (const [file, pkg] of [
    ['services/bosso/internal/domain/x.go', PROTO],
    ['services/bosso/internal/newfeature/x.go', `${PROTO}/bossanovav1connect`],
    ['services/bosso/future/x.go', PROTO],
    ['services/bossd/internal/session/x.go', PROTO],
    ['services/bossd/internal/adapter/git/x.go', `${PROTO}/bossanovav1connect`],
    ['services/bossd/future/x.go', PROTO],
  ])
    write(root, file, source(pkg))
  assert.equal(check(root).offenders.length, 6)
  assert.equal(check(root).failures.length, 6)
  assert.match(check(root).failures.join('\n'), /owned domain type/)
  for (const file of check(root).offenders) write(root, file, 'package x\n')
  assert.deepEqual(check(root).failures, [])
})
test('proto-at-the-edge allows API, cmd, tests and neighboring package names', (t) => {
  const root = fixture(t)
  for (const [file, pkg] of [
    ['services/bosso/api/future/x.go', PROTO],
    ['services/bosso/cmd/x.go', `${PROTO}/bossanovav1connect`],
    ['services/bossd/cmd/x.go', `${PROTO}/bossanovav1connect`],
    ['services/bossd/internal/feature/x_test.go', PROTO],
    ['services/bossd/internal/feature/x.go', `${PROTO}extra`],
    ['services/bosso/internal/feature/x_test.go', PROTO],
    ['services/bosso/internal/feature/x.go', `${PROTO}extra`],
    ['services/bossd/api/daemon/x.go', PROTO],
  ])
    write(root, file, source(pkg))
  write(
    root,
    'services/bosso/internal/feature/comment.go',
    `package x\n// import "${PROTO}"\nvar _ = \`import "${PROTO}"\`\n`,
  )
  assert.deepEqual(check(root).failures, [])
})
test('proto-at-the-edge baseline is uncapped, reason-aware, shrinking and idempotent', (t) => {
  const root = fixture(t)
  for (let i = 0; i < 151; i++)
    write(root, `services/bosso/internal/feature/x${i}.go`, source(PROTO))
  assert.deepEqual(check(root, true).failures, [])
  const baselinePath = path.join(root, 'baseline.json')
  const entries = JSON.parse(fs.readFileSync(baselinePath, 'utf8'))
  assert.equal(entries.length, 151)
  assert.ok(entries.every((entry) => splitBaselineEntry(entry).reason))
  const config = fs.readFileSync(path.join(root, '.golangci.yml'), 'utf8')
  assert.deepEqual(check(root, true).failures, [])
  assert.equal(fs.readFileSync(path.join(root, '.golangci.yml'), 'utf8'), config)
  write(
    root,
    'baseline.json',
    JSON.stringify(entries.map((entry) => `${splitBaselineEntry(entry).key} — revised reason`)),
  )
  assert.deepEqual(check(root).failures, [])
  write(
    root,
    '.golangci.yml',
    config.replace('"!**/services/bosso/internal/feature/x0.go"', '"!**/wrong.go"'),
  )
  assert.match(check(root).failures.join('\n'), /generated.*stale/)
  assert.deepEqual(check(root, true).failures, [])
  write(root, 'services/bosso/internal/feature/x0.go', 'package x\n')
  assert.match(check(root).failures.join('\n'), /no longer occurs/)
  assert.deepEqual(check(root, true).failures, [])
  assert.equal(JSON.parse(fs.readFileSync(baselinePath, 'utf8')).length, 150)
})
test('proto-at-the-edge rejects missing deny entries and drifted exemptions', (t) => {
  const root = fixture(t)
  write(
    root,
    '.golangci.yml',
    fs
      .readFileSync(path.join(root, '.golangci.yml'), 'utf8')
      .replaceAll(`pkg: ${PROTO}$`, 'pkg: wrong/pkg$')
      .replace('!**/services/bosso/cmd/**', '!**/wrong/cmd/**'),
  )
  assert.match(check(root).failures.join('\n'), /missing deny/)
  assert.match(check(root).failures.join('\n'), /files must match/)
})
test('proto-at-the-edge real tree passes', () =>
  assert.deepEqual(checkProtoAtTheEdge().failures, []))
test('proto-at-the-edge real depguard rejects seeded protobuf imports and accepts the repair', (t) => {
  const available = spawnSync('golangci-lint', ['version'], { encoding: 'utf8' })
  if (available.error?.code === 'ENOENT') {
    if (process.env.GUARD_PROOF_REQUIRE_TOOLS === '1')
      assert.fail('golangci-lint required by GUARD_PROOF_REQUIRE_TOOLS=1')
    return t.skip('golangci-lint not on PATH')
  }
  assert.equal(available.status, 0, available.stderr)
  for (const service of ['bosso', 'bossd']) {
    const root = fixture(t)
    write(
      root,
      `services/${service}/go.mod`,
      `module github.com/recurser/${service}\n\ngo 1.26.0\n\nrequire github.com/recurser/bossalib v0.0.0\n\nreplace github.com/recurser/bossalib => ../../lib/bossalib\n`,
    )
    write(root, 'lib/bossalib/go.mod', 'module github.com/recurser/bossalib\n\ngo 1.26.0\n')
    for (const pkg of ['v1', 'v1/bossanovav1connect', 'v1extra'])
      write(root, `lib/bossalib/gen/bossanova/${pkg}/x.go`, 'package x\n')
    const bad = [
      [`services/${service}/internal/domain/x.go`, PROTO],
      [`services/${service}/internal/future/x.go`, `${PROTO}/bossanovav1connect`],
    ]
    for (const [file, pkg] of bad) write(root, file, source(pkg))
    for (const [file, pkg] of [
      [`services/${service}/api/future/x.go`, PROTO],
      [`services/${service}/cmd/x.go`, PROTO],
      [`services/${service}/internal/feature/x.go`, `${PROTO}extra`],
      [`services/${service}/internal/feature/x_test.go`, PROTO],
    ])
      write(root, file, source(pkg))
    const run = () =>
      spawnSync(
        'golangci-lint',
        [
          'run',
          '--allow-parallel-runners',
          '--config',
          path.join(root, '.golangci.yml'),
          '--enable-only',
          'depguard',
          './...',
        ],
        {
          cwd: path.join(root, `services/${service}`),
          env: { ...process.env, GOWORK: 'off' },
          encoding: 'utf8',
          timeout: 120000,
          maxBuffer: 4 * 1024 * 1024,
        },
      )
    const red = run()
    assert.equal(red.error, undefined)
    assert.equal(red.status, 1, red.stdout + red.stderr)
    assert.match(red.stdout, /domain\/x.go.*proto-at-the-edge/)
    assert.match(red.stdout, /future\/x.go.*proto-at-the-edge/)
    assert.doesNotMatch(
      red.stdout,
      /(?:api\/future\/x.go|cmd\/x.go|feature\/x(?:_test)?\.go).*proto-at-the-edge/,
    )
    for (const [file] of bad) write(root, file, 'package x\n')
    const green = run()
    assert.equal(green.error, undefined)
    assert.equal(green.status, 0, green.stdout + green.stderr)
  }
})
