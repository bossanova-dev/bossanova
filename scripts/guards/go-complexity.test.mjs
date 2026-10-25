import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { spawnSync } from 'node:child_process'
import {
  renderConfig,
  parseReport,
  subjectKey,
  checkFindings,
  measureModule,
  inactiveSubjectsFor,
} from './go-complexity.mjs'

const budgets = {
  'go-cognitive-complexity': { default: 30, subjects: {} },
  'go-interface-width': { default: 10, subjects: {} },
}
function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'go-complexity-proof-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  return root
}

test('subjects come from declarations and retain receiver identity across file splits', () => {
  assert.equal(
    subjectKey('services/bossd/x.go', 'func (s *Store[T]) Read() {}', 1, 'ireturn'),
    'services/bossd:Store.Read',
  )
  assert.equal(
    subjectKey('services/bossd/split.go', 'func Read() {}', 1, 'gocognit'),
    'services/bossd:Read',
  )
  assert.equal(
    subjectKey(
      'services/bossd/x.go',
      'type (\n Wide interface {\n Read()\n }\n)',
      2,
      'interfacebloat',
    ),
    'services/bossd:Wide',
  )
  assert.throws(() => subjectKey('x.go', '// func Fake() {}', 1, 'ireturn'))
})

test('checks reject growth, stale entries and budgets no longer above the default within this module', () => {
  const input = {
    moduleDir: 'services/bossd',
    budgets: {
      ...budgets,
      'go-cognitive-complexity': {
        default: 30,
        subjects: { 'services/bossd:Read': 31, 'services/boss:Other': 99 },
      },
    },
    entries: ['services/bossd:New', 'services/boss:Other'],
    findings: [],
  }
  assert.equal(checkFindings(input).length, 2)
  input.findings = [
    { guardId: 'go-cognitive-complexity', subject: 'services/bossd:Read', count: 32 },
  ]
  assert.match(checkFindings(input).join('\n'), /32 is over its budget of 31/)
  input.findings[0].count = 30
  assert.match(checkFindings(input).join('\n'), /no longer needs/)
})

test('malformed JSON reports fail closed', () => {
  for (const data of ['{}', '{"Issues":[{}]}', 'broken'])
    assert.throws(() => parseReport(data, '.', 'services/bossd'))
})

function proveSeed(t, name, source, guard) {
  const probe = spawnSync('golangci-lint', ['version'], { encoding: 'utf8' })
  if (probe.error?.code === 'ENOENT') {
    assert.notEqual(process.env.GUARD_PROOF_REQUIRE_TOOLS, '1', 'golangci-lint is required')
    t.skip('golangci-lint unavailable')
    return
  }
  const root = fixture(t)
  fs.writeFileSync(path.join(root, 'go.mod'), 'module example.com/proof\n\ngo 1.26\n')
  fs.writeFileSync(path.join(root, 'seed.go'), `package proof\n${source}\n`)
  const findings = measureModule({
    repoRoot: root,
    moduleDir: '.',
    budgets,
    env: { ...process.env, GOWORK: 'off' },
  })
  assert.equal(findings.length, 1)
  assert.equal(findings[0].guardId, guard)
  assert.equal(checkFindings({ moduleDir: '.', budgets, entries: [], findings }).length, 1)
  if (name === 'gocognit') {
    assert.equal(findings[0].count, 31)
    const budgeted = {
      ...budgets,
      [guard]: { default: 30, subjects: { [findings[0].subject]: 31 } },
    }
    assert.deepEqual(
      checkFindings({ moduleDir: '.', budgets: budgeted, entries: [], findings }),
      [],
    )
    fs.writeFileSync(
      path.join(root, 'seed.go'),
      `package proof\n${source.replace('return n', 'if n == 99 { n++ }; return n')}\n`,
    )
    const grown = measureModule({
      repoRoot: root,
      moduleDir: '.',
      budgets,
      env: { ...process.env, GOWORK: 'off' },
    })
    assert.match(
      checkFindings({ moduleDir: '.', budgets: budgeted, entries: [], findings: grown }).join('\n'),
      /32 is over its budget of 31/,
    )
  }
  fs.writeFileSync(path.join(root, 'seed.go'), 'package proof\nfunc Read() int { return 1 }\n')
  assert.deepEqual(
    measureModule({
      repoRoot: root,
      moduleDir: '.',
      budgets,
      env: { ...process.env, GOWORK: 'off' },
    }),
    [],
  )
}

test('seeded gocognit violation fails the Go complexity guard', (t) => {
  proveSeed(
    t,
    'gocognit',
    `func Read(n int) int {\n${Array.from({ length: 31 }, (_, i) => `if n == ${i} { n++ }`).join('\n')}\nreturn n\n}`,
    'go-cognitive-complexity',
  )
})
test('seeded interfacebloat violation fails the Go complexity guard', (t) => {
  proveSeed(
    t,
    'interfacebloat',
    `type Wide interface {\n${Array.from({ length: 11 }, (_, i) => `Read${i}()`).join('\n')}\n}`,
    'go-interface-width',
  )
})
test('seeded ireturn violation fails the Go complexity guard', (t) => {
  proveSeed(
    t,
    'ireturn',
    'type Reader interface { Read() }\nfunc NewReader() Reader { return nil }',
    'go-returned-interfaces',
  )
})

test('measuring never reads or writes the shared golangci cache', (t) => {
  const probe = spawnSync('golangci-lint', ['version'], { encoding: 'utf8' })
  if (probe.error?.code === 'ENOENT') {
    assert.notEqual(process.env.GUARD_PROOF_REQUIRE_TOOLS, '1', 'golangci-lint is required')
    t.skip('golangci-lint unavailable')
    return
  }
  const root = fixture(t)
  const shared = fixture(t)
  fs.writeFileSync(path.join(root, 'go.mod'), 'module example.com/cacheproof\n\ngo 1.26\n')
  fs.writeFileSync(
    path.join(root, 'seed.go'),
    'package proof\ntype Reader interface { Read() }\nfunc NewReader() Reader { return nil }\n',
  )
  const findings = measureModule({
    repoRoot: root,
    moduleDir: '.',
    budgets,
    env: { ...process.env, GOWORK: 'off', GOLANGCI_LINT_CACHE: shared },
  })
  assert.equal(findings.length, 1)
  assert.deepEqual(fs.readdirSync(shared), [], 'golangci wrote to the shared cache')
})

test('platform exclusions preserve active and deleted symbol stale checks', (t) => {
  const root = fixture(t)
  fs.writeFileSync(path.join(root, 'go.mod'), 'module example.com/platformproof\n\ngo 1.26\n')
  fs.mkdirSync(path.join(root, 'platform'))
  fs.writeFileSync(
    path.join(root, 'platform', 'only_darwin.go'),
    `package platform
func DarwinOnly() {}
func Shared() {}
type Wide interface { Read() }
`,
  )
  fs.writeFileSync(
    path.join(root, 'platform', 'only_linux.go'),
    `package platform
func Shared() {}
func LinuxOnly() {}
func ActiveOnly() {}
`,
  )
  const subjects = [
    'platform:DarwinOnly',
    'platform:Shared',
    'platform:ActiveOnly',
    'platform:Wide',
    'platform:Deleted',
    'gone:Deleted',
  ]
  for (const goos of ['linux', 'darwin']) {
    const inactiveSubjects = inactiveSubjectsFor({
      repoRoot: root,
      moduleDir: '.',
      subjects,
      env: { ...process.env, GOWORK: 'off', GOOS: goos, CGO_ENABLED: '0' },
    })
    assert.equal(inactiveSubjects.has('platform:DarwinOnly'), goos === 'linux')
    assert.equal(inactiveSubjects.has('platform:Wide'), goos === 'linux')
    assert.equal(inactiveSubjects.has('platform:Shared'), true)
    assert.equal(inactiveSubjects.has('platform:ActiveOnly'), goos === 'darwin')
    assert.equal(inactiveSubjects.has('platform:Deleted'), false)
    assert.equal(inactiveSubjects.has('gone:Deleted'), false)
    const failures = checkFindings({
      moduleDir: '.',
      inactiveSubjects,
      findings: [],
      budgets: {
        'go-cognitive-complexity': {
          default: 30,
          subjects: {
            'platform:DarwinOnly': 33,
            'platform:Shared': 33,
            'platform:ActiveOnly': 33,
            'platform:Deleted': 33,
          },
        },
        'go-interface-width': { default: 10, subjects: { 'platform:Wide': 11 } },
      },
      entries: [
        'platform:DarwinOnly',
        'platform:Shared',
        'platform:ActiveOnly',
        'platform:Deleted',
        'gone:Deleted',
      ],
    })
    assert.equal(
      failures.filter((f) => f.includes('platform:DarwinOnly')).length,
      goos === 'linux' ? 0 : 2,
    )
    assert.equal(
      failures.filter((f) => f.includes('platform:Wide')).length,
      goos === 'linux' ? 0 : 1,
    )
    assert.equal(failures.filter((f) => f.includes('platform:Shared')).length, 0)
    assert.equal(
      failures.filter((f) => f.includes('platform:ActiveOnly')).length,
      goos === 'linux' ? 2 : 0,
    )
    const grown = checkFindings({
      moduleDir: '.',
      inactiveSubjects,
      budgets: {
        'go-cognitive-complexity': { default: 30, subjects: { 'platform:Shared': 33 } },
        'go-interface-width': { default: 10, subjects: {} },
      },
      entries: ['platform:Shared'],
      findings: [
        { guardId: 'go-cognitive-complexity', subject: 'platform:Shared', count: 34 },
        { guardId: 'go-returned-interfaces', subject: 'platform:Shared', count: 1 },
      ],
    })
    assert.equal(grown.length, 1)
    assert.match(grown[0], /34 is over its budget of 33/)
    assert.equal(failures.filter((f) => f.includes('platform:Deleted')).length, 2)
    assert.equal(failures.filter((f) => f.includes('gone:Deleted')).length, 1)
  }
  // A whole package may be excluded by build constraints, and ./... would omit it.
  fs.unlinkSync(path.join(root, 'platform', 'only_linux.go'))
  assert.equal(
    inactiveSubjectsFor({
      repoRoot: root,
      moduleDir: '.',
      subjects,
      env: { ...process.env, GOWORK: 'off', GOOS: 'linux', CGO_ENABLED: '0' },
    }).has('platform:DarwinOnly'),
    true,
  )
})
