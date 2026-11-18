import test from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { discoverExtensions } from '../skill-extensions.mjs'
import { boundDescription } from './describer-kit.mjs'
import { refineDescription, DESCRIBE_TIMEOUT_MS } from './verify-describe.mjs'
import { gitFixture, envelopeFor, runDescriberScript } from './verify-git-fixture.mjs'
import { describeTests, testCases, testSubjects, checksSignal } from './verify-describe-tests.mjs'

test('lexical case names follow Go, JavaScript and Python declarations', () => {
  assert.deepEqual(
    testCases(
      'x_test.go',
      'func TestSave(t *testing.T) {}\nfunc BenchmarkSave(b *testing.B) {}\nfunc FuzzSave(f *testing.F) {}\nfunc ExampleSave() {}',
    ),
    ['TestSave', 'BenchmarkSave', 'FuzzSave', 'ExampleSave'],
  )
  assert.deepEqual(
    testCases(
      'x.test.ts',
      'test("saves", () => {})\nit.only(\'loads\', () => {})\n describe.skip(`empty`, () => {})\ntest.todo("later")\ntest.each([])("table")\ntest(`dynamic ${name}`, () => {})\nconst s = "test(\\"fake\\")"',
    ),
    ['saves', 'loads', 'empty', 'later'],
  )
  assert.deepEqual(
    testCases('test_x.py', 'def test_save(): pass\nclass Suite:\n    async def test_load(): pass'),
    ['test_save', 'test_load'],
  )
  assert.deepEqual(testCases('x.test.rs', 'fn test_save() {}'), [])
})

test('maximum-size blank-line sources finish inside the describer deadline', () => {
  const moduleUrl = new URL('./verify-describe-tests.mjs', import.meta.url).href
  const result = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `import { testCases } from ${JSON.stringify(moduleUrl)};
       const blank = '\\n'.repeat(1024 * 1024 - 100);
       console.log(JSON.stringify([
         testCases('x.test.js', blank + '\\ttest("works", () => {})'),
         testCases('test_x.py', blank + '\\tdef test_works(): pass'),
       ]));`,
    ],
    { encoding: 'utf8', timeout: DESCRIBE_TIMEOUT_MS },
  )
  assert.ifError(result.error)
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), [['works'], ['test_works']])
})

test('subjects map to changed package or matching stems', () => {
  const changes = [
    'pkg/a.go',
    'pkg/b.go',
    'pkg/a_test.go',
    'other/c.go',
    'src/Button.tsx',
    'src/store.ts',
    'parse.py',
  ].map((path) => ({ path, status: 'M' }))
  assert.deepEqual(testSubjects('pkg/a_test.go', changes), ['pkg/a.go', 'pkg/b.go'])
  assert.deepEqual(testSubjects('src/Button.test.tsx', changes), ['src/Button.tsx'])
  assert.deepEqual(testSubjects('src/__tests__/store.test.ts', changes), ['src/store.ts'])
  assert.deepEqual(testSubjects('tests/test_parse.py', changes), ['parse.py'])
  assert.deepEqual(testSubjects('unrelated.test.js', changes), [])
})

function fixture(t) {
  const repo = gitFixture(t)
  repo.write({ README: 'base' })
  const base = repo.commit()
  repo.write({
    'src/components/Button.tsx': 'export const Button = 1',
    'src/components/Button.test.tsx':
      'test("renders empty", () => {})\nit("rejects blank", () => {})',
    'pkg/testdata/x_test.go': 'func TestFixture() {}',
  })
  const head = repo.commit()
  return { repo, base, head }
}

test('test items follow subjects; housekeeping alone sends PR-wide signals', (t) => {
  const { repo, base, head } = fixture(t)
  const ui = describeTests(envelopeFor(repo.dir, base, head, 'ui'), { cwd: repo.dir })
  assert.match(ui.items.join(' '), /Button.test.tsx.*added.*renders empty.*rejects blank/)
  assert.equal(ui.signals, undefined)
  const envelope = {
    ...envelopeFor(repo.dir, base, head, 'housekeeping'),
    checks: ['build', 'unit', 'unit'],
  }
  const facts = describeTests(envelope, { cwd: repo.dir })
  assert.equal(facts.breaking, false)
  assert.equal(facts.signals.find((s) => s.key === 'tests').state, 'additive')
  assert.match(facts.signals[0].summary, /1 test file.*2 new cases/)
  assert.equal(
    facts.signals.some((s) => s.summary.includes('x_test.go')),
    false,
  )
  assert.deepEqual(
    facts.signals.find((s) => s.key === 'checks'),
    {
      key: 'checks',
      label: 'Checks',
      state: 'proven',
      summary: '2 passed on this head: build, unit',
    },
  )
  for (const checks of [undefined, []]) {
    const unknown = describeTests({ ...envelope, checks }, { cwd: repo.dir })
    assert.equal(
      unknown.signals.some((s) => s.key === 'checks'),
      false,
    )
  }
  const result = runDescriberScript(
    new URL('./verify-describe-tests.mjs', import.meta.url).href,
    envelope,
    repo.dir,
  )
  assert.deepEqual(refineDescription(result), [])
})

test('removed cases and files request review; rename compares against old path', (t) => {
  const repo = gitFixture(t)
  repo.write({
    'a.test.ts': 'test("keep", () => {})\ntest("remove", () => {})\n' + '// padding\n'.repeat(20),
    'gone.spec.ts': 'it("gone", () => {})',
  })
  const base = repo.commit()
  repo.move('a.test.ts', 'b.test.ts')
  repo.write({
    'b.test.ts': 'test("keep", () => {})\ntest("new", () => {})\n' + '// padding\n'.repeat(20),
    'gone.spec.ts': null,
  })
  const head = repo.commit()
  const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), { cwd: repo.dir })
  assert.match(facts.items.join(' '), /renamed from `a.test.ts`.*new `new`.*removed `remove`/)
  assert.match(facts.items.join(' '), /gone.spec.ts.*removed/)
  assert.equal(facts.signals[0].state, 'review')
  assert.equal(facts.breaking, false)
})

for (const [file, before, after] of [
  [
    'a.test.js',
    'test("works", () => { assert.ok(value) })',
    'test.skip("works", () => { assert.ok(value) })',
  ],
  ['a.test.js', 'test("works", () => { assert.ok(value) })', 'test("works", () => {})'],
  [
    'a_test.go',
    'func TestWorks(t *testing.T) { check(t) }',
    'func TestWorks(t *testing.T) { t.Skip("disabled"); check(t) }',
  ],
  ['a_test.go', 'func TestWorks(t *testing.T) { check(t) }', 'func TestWorks(t *testing.T) {}'],
  ['test_a.py', 'def test_works():\n    assert value', 'def test_works():\n    pass'],
]) {
  for (const rename of [false, true]) {
    test(`existing test edits require review even with unchanged names: ${file}, rename=${rename}, ${after}`, (t) => {
      const repo = gitFixture(t)
      const padding = '\n' + (file.endsWith('.py') ? '# padding\n' : '// padding\n').repeat(30)
      repo.write({ [file]: before + padding })
      const base = repo.commit()
      const next = rename ? `renamed/${file}` : file
      if (rename) repo.move(file, next)
      repo.write({ [next]: after + padding })
      const head = repo.commit()
      const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), {
        cwd: repo.dir,
      })
      assert.equal(facts.signals[0].state, 'review')
    })
  }
}

test('byte-identical test renames retain the safe signal', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'a.test.js': 'test("works", () => { assert.ok(value) })' })
  const base = repo.commit()
  repo.move('a.test.js', 'b.test.js')
  const head = repo.commit()
  const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), { cwd: repo.dir })
  assert.equal(facts.signals[0].state, 'safe')
})

for (const [from, to, source, name] of [
  ['test_x.py', 'x_helpers.py', 'def test_save(): pass', 'test_save'],
  ['x.test.js', 'x_helpers.js', 'test("save", () => {})', 'save'],
  ['x_test.go', 'x_helpers.go', 'func TestSave() {}', 'TestSave'],
  ['empty.test.js', 'empty.js', '// no lexical cases', null],
  ['pkg/x_test.go', 'pkg/testdata/x_test.go', 'func TestSave() {}', 'TestSave'],
  ['pkg/empty_test.go', 'pkg/testdata/empty_test.go', '// no lexical cases', null],
]) {
  test(`rename out of test discovery requires review: ${from}`, (t) => {
    const repo = gitFixture(t)
    repo.write({ [from]: source })
    const base = repo.commit()
    repo.move(from, to)
    const head = repo.commit()
    const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), {
      cwd: repo.dir,
    })
    assert.equal(facts.signals[0].state, 'review')
    if (name) assert.ok(facts.items[0].includes(`removed \`${name}\``))
  })
}

for (const [from, to, source, name] of [
  ['x_helpers.py', 'test_x.py', 'def test_save(): pass', 'test_save'],
  ['pkg/testdata/x_test.go', 'pkg/x_test.go', 'func TestSave() {}', 'TestSave'],
]) {
  test(`rename into test discovery reports newly discoverable cases: ${from}`, (t) => {
    const repo = gitFixture(t)
    repo.write({ [from]: source })
    const base = repo.commit()
    repo.move(from, to)
    const head = repo.commit()
    const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), {
      cwd: repo.dir,
    })
    assert.equal(facts.signals[0].state, 'additive')
    assert.ok(facts.items[0].includes(`new \`${name}\``))
  })
}

test('renames within testdata remain outside test discovery', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'pkg/testdata/x_test.go': 'func TestSave() {}' })
  const base = repo.commit()
  repo.move('pkg/testdata/x_test.go', 'pkg/testdata/y_test.go')
  const head = repo.commit()
  const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), { cwd: repo.dir })
  assert.deepEqual(facts.items, [])
  assert.equal(facts.signals, undefined)
})

test('bounds preserve listing while omitting unread or oversized case names', (t) => {
  const repo = gitFixture(t)
  repo.write({ README: 'base' })
  const base = repo.commit()
  repo.write(
    Object.fromEntries(
      Array.from({ length: 205 }, (_, i) => [
        `tests/t${String(i).padStart(3, '0')}.test.js`,
        i === 0
          ? 'test("huge", () => {})\n' + ' '.repeat(1024 * 1024)
          : `test("case${i}", () => {})`,
      ]),
    ),
  )
  const head = repo.commit()
  const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), { cwd: repo.dir })
  assert.equal(facts.items.length, 7)
  assert.match(facts.items.at(-1), /199 more files changed/)
  assert.doesNotMatch(facts.items[0], /huge/)
  assert.equal(facts.signals[0].state, 'additive')
  assert.match(facts.signals[0].summary, /6 not inspected/)
  assert.deepEqual(refineDescription(boundDescription('boss-verify-tests', facts)), [])
})

for (const rename of [false, true]) {
  test(`oversized ${rename ? 'renamed' : 'modified'} tests require review`, (t) => {
    const repo = gitFixture(t)
    const padding = '// padding\n'.repeat(120_000)
    repo.write({ 'a.test.js': `test("removed", () => {})\n${padding}` })
    const base = repo.commit()
    if (rename) repo.move('a.test.js', 'b.test.js')
    repo.write({ [rename ? 'b.test.js' : 'a.test.js']: padding })
    const head = repo.commit()
    const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), {
      cwd: repo.dir,
    })
    assert.equal(facts.signals[0].state, 'review')
    assert.match(facts.signals[0].summary, /1 not inspected/)
    assert.match(facts.items[0], /case names not inspected/)
  })
}

test('modified tests beyond the file read bound require review', (t) => {
  const repo = gitFixture(t)
  const files = Array.from({ length: 201 }, (_, i) => `t${String(i).padStart(3, '0')}.test.js`)
  repo.write(Object.fromEntries(files.map((file) => [file, 'test("removed", () => {})'])))
  const base = repo.commit()
  repo.write(
    Object.fromEntries(
      files.map((file, i) => [file, i === 200 ? '// removed' : 'test("removed", () => {})\n']),
    ),
  )
  const head = repo.commit()
  const facts = describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), {
    cwd: repo.dir,
  })
  assert.equal(facts.signals[0].state, 'review')
  assert.match(facts.signals[0].summary, /1 not inspected/)
})

test('no test changes sends no tests signal and checks helper omits unknowns', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'README.md': 'base' })
  const base = repo.commit()
  repo.write({ 'README.md': 'head' })
  const head = repo.commit()
  assert.equal(
    describeTests(envelopeFor(repo.dir, base, head, 'housekeeping'), { cwd: repo.dir }).signals,
    undefined,
  )
  assert.equal(checksSignal(undefined), null)
  assert.equal(checksSignal([]), null)
})

test('payload discovers and executes the built-in with its vendored closure', (t) => {
  const { repo, base, head } = fixture(t)
  const builtinDir = fileURLToPath(
    new URL(
      '../../lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/extensions',
      import.meta.url,
    ),
  )
  const found = discoverExtensions({
    core: 'boss-verify',
    role: 'proof-describe',
    builtinDir,
    root: repo.dir,
  }).extensions.find((e) => e.name === 'boss-verify-tests')
  assert.equal(found?.builtin, true)
  assert.ok(found.describeAreas.includes('ui'))
  assert.ok(found.describeAreas.includes('no-sign-off'))
  assert.equal(found.describeAreas.includes('dependencies'), false)
  const envelope = {
    ...envelopeFor(repo.dir, base, head, 'housekeeping', found.name),
    checks: ['build', 'unit'],
  }
  const result = runDescriberScript(
    new URL(`file://${found.dir}/describe.mjs`).href,
    envelope,
    repo.dir,
  )
  assert.deepEqual(
    result.signals.map((s) => s.key),
    ['tests', 'checks'],
  )
})
