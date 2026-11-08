import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  importSpecifiers,
  resolvesTo,
  testCoverage,
  readUiTestCoverage,
  uiProof,
  MAX_COVER_READS,
} from './verify-ui-proof.mjs'

const files = [
  { path: 'src/Button.tsx', status: 'modified' },
  { path: 'src/Button.test.tsx', status: 'added' },
]
const coverage = (source, changed = files) =>
  testCoverage({ files: changed, readContent: () => source })

test('changed relative component imports prove every UI path only with green CI', () => {
  const covered = coverage("import Button from './Button'")
  assert.deepEqual(covered, {
    uiPaths: ['src/Button.tsx'],
    tests: ['src/Button.test.tsx'],
    uncovered: [],
    covered: true,
  })
  assert.equal(uiProof({ ciGreen: true, coverage: covered }), 'tests')
  assert.equal(uiProof({ ciGreen: false, coverage: covered }), 'none')
  assert.equal(
    coverage("import Button from './Button'", [...files, { path: 'src/Other.tsx' }]).covered,
    false,
  )
  assert.equal(coverage("import Button from './Button'", []).covered, false)
})

test('only real static module references count and mocked modules are excluded', () => {
  assert.deepEqual(
    importSpecifiers(
      `import X from './a'; import './b'; export {X} from './c'; import('./d'); require('./e')`,
    ),
    ['./a', './b', './c', './d', './e'],
  )
  for (const source of [
    "import Button from '@/Button'",
    "// import './Button'",
    "/* import './Button' */",
    null,
    `const text = "import './Button'"`,
    "const text = `import './Button'`",
    `const pattern = /import('..\/Button')/`,
    ...['vi.mock', 'jest.mock', 'vi.doMock', 'jest.doMock'].map(
      (mock) => `import './Button'; ${mock}('./Button')`,
    ),
  ])
    assert.equal(coverage(source).covered, false, String(source))
  assert.equal(
    coverage(
      "import './Button'",
      files.map((f) => ({ ...f, status: 'removed' })),
    ).covered,
    false,
  )
  assert.equal(
    coverage("import './Button'", [
      { path: 'src/Button.tsx' },
      { path: 'src/Button.test.py', status: 'added' },
    ]).covered,
    false,
  )
})

test('relative module resolution handles source extensions and directory indexes', () => {
  for (const spec of [
    './Button',
    './Button.tsx',
    './Button.js',
    './Button.jsx',
    './Button.mjs',
    './Button.cjs',
  ])
    assert.equal(resolvesTo('src/Button.test.ts', spec, 'src/Button.tsx'), true)
  assert.equal(resolvesTo('tests/ui.spec.ts', '../src/dir', 'src/dir/index.tsx'), true)
  assert.equal(resolvesTo('src/Button.test.ts', '@/Button', 'src/Button.tsx'), false)
  assert.equal(resolvesTo('src/Button.test.ts', './Other', 'src/Button.tsx'), false)
})

test('coverage caps candidate reads and tolerates unreadable head contents', () => {
  let reads = 0
  const changed = [
    files[0],
    ...Array.from({ length: MAX_COVER_READS + 1 }, (_, i) => ({
      path: `tests/${i}.test.ts`,
      status: 'modified',
    })),
  ]
  assert.equal(
    testCoverage({
      files: changed,
      readContent: () => {
        reads++
        return "import '../src/Button'"
      },
    }).covered,
    false,
  )
  assert.equal(reads, 0)
  assert.equal(
    testCoverage({
      files,
      readContent: () => {
        throw Error('offline')
      },
    }).covered,
    false,
  )
  const read = (_command, args) => {
    reads++
    assert.equal(args[1], 'repos/o/r/contents/src/Button.test.tsx?ref=head')
    return {
      ok: true,
      payload: {
        type: 'file',
        encoding: 'base64',
        content: Buffer.from("import './Button'").toString('base64'),
      },
    }
  }
  assert.equal(readUiTestCoverage(read, { repo: 'o/r', headSha: 'head', files }).covered, true)
  for (const payload of [
    { type: 'dir' },
    { type: 'file', encoding: 'none' },
    { type: 'file', encoding: 'base64', content: null },
  ])
    assert.equal(
      readUiTestCoverage(() => ({ ok: true, payload }), { repo: 'o/r', headSha: 'head', files })
        .covered,
      false,
    )
  assert.equal(
    readUiTestCoverage(() => ({ ok: false }), { repo: 'o/r', headSha: 'head', files }).covered,
    false,
  )
  assert.equal(
    readUiTestCoverage(
      () => {
        throw Error('read')
      },
      { repo: 'o/r', headSha: 'head', files },
    ).covered,
    false,
  )
})

test('proof precedence is tests, valid recorded pass, none', () => {
  const outcomes = [{ outcome: 'ok', verdict: 'pass', recorded: true }]
  assert.equal(uiProof({ outcomes }), 'recording')
  assert.equal(uiProof({ ciGreen: true, coverage: { covered: true }, outcomes }), 'tests')
  for (const outcome of [
    { outcome: 'failed', verdict: 'pass', recorded: true },
    { outcome: 'ok', verdict: 'human', recorded: true },
    { outcome: 'ok', verdict: 'pass' },
  ])
    assert.equal(uiProof({ outcomes: [outcome] }), 'none')
})

test('template text is opaque but mocked references inside interpolation still invalidate proof', () => {
  assert.equal(coverage("import './Button'; const text = `${vi.mock('./Button')}`").covered, false)
  assert.equal(coverage("import './Button'; const text = `ordinary ${1 + 2} text`").covered, true)
})

test('division and regex literals cannot hide mocks or manufacture imports', () => {
  for (const source of [
    "import './Button'; const n = 10 / 2; vi.mock('./Button')",
    "import './Button'; render(<div>text</div>); vi.mock('./Button')",
    "import './Button'; const n = `${10 / 2}`; vi.mock('./Button')",
    "const pattern = /import('.\\/Button')/",
  ])
    assert.equal(coverage(source).covered, false, source)
  assert.equal(coverage("import './Button'; expect('Button').toMatch(/Button/)").covered, true)
})

test('paired and nested JSX preserve imports without hiding subsequent mocks', () => {
  for (const markup of [
    '<Button>Merge</Button>',
    '<section><Button>Merge</Button></section>',
    '<><Button>Merge</Button><span /></>',
  ]) {
    assert.equal(
      coverage(`import Button from './Button'; render(${markup});`).covered,
      true,
      markup,
    )
    assert.equal(
      coverage(`import Button from './Button'; render(${markup}); vi.mock('./Button')`).covered,
      false,
      markup,
    )
  }
})

test('dynamic module arguments to mocks disqualify their imported module', () => {
  for (const mock of ['vi.mock', 'vi.doMock', 'jest.mock', 'jest.doMock']) {
    assert.equal(
      coverage(`${mock}(import('./Button'), () => ({default: () => null}))`).covered,
      false,
      mock,
    )
    assert.equal(
      coverage(
        `import Button from './Button'; ${mock}(import('./Button'), () => ({default: () => null}))`,
      ).covered,
      false,
      mock,
    )
  }
})

test('script inference never substitutes for an explicit stylesheet import', () => {
  for (const [spec, path] of [
    ['./Button', 'src/Button.css'],
    ['./styles', 'src/styles/index.css'],
    ['./styles/index', 'src/styles/index.css'],
  ]) {
    assert.equal(resolvesTo('src/Button.test.tsx', spec, path), false)
  }
  assert.equal(resolvesTo('src/Button.test.tsx', './Button.css', 'src/Button.css'), true)
  const changed = [{ path: 'src/Button.css', status: 'modified' }, files[1]]
  assert.equal(coverage("import Button from './Button'", changed).covered, false)
  assert.equal(coverage("import './Button.css'", changed).covered, true)
})

test('equivalent relative mock specifiers disqualify coverage of the resolved UI source', () => {
  for (const [imported, mocked] of [
    ['./Button', '././Button'],
    ['./Button', './Button.tsx'],
    ['./Button.tsx', './Button'],
    ['./Button.js', './Button.tsx'],
  ]) {
    assert.equal(
      coverage(`import '${imported}'; vi.mock('${mocked}', () => ({}))`).covered,
      false,
      `${imported} mocked as ${mocked}`,
    )
    assert.equal(
      coverage(`import '${imported}'; vi.mock(import('${mocked}'), () => ({}))`).covered,
      false,
    )
  }
  const changed = [{ path: 'src/dir/index.tsx', status: 'modified' }, files[1]]
  assert.equal(coverage("import './dir'; jest.mock('./dir/index.tsx')", changed).covered, false)
  assert.equal(coverage("import './Button'; vi.mock('./Other')").covered, true)
  assert.equal(coverage("import './Button'; vi.mock('./Button.css')").covered, true)
})
