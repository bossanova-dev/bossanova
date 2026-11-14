import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  importSpecifiers,
  resolvesTo,
  testCoverage,
  readUiTestCoverage,
  uiProof,
  MAX_COVER_READS,
  MAX_CONFIG_READS,
  MAX_PATH_PATTERNS,
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

const ui = { path: 'app/src/components/Button.tsx', status: 'modified' }
const spec = { path: 'app/src/components/__tests__/Button.test.tsx', status: 'added' }
const aliasCoverage = (contents, changed = [ui, spec]) =>
  testCoverage({
    files: changed,
    readContent: (path) => contents[path] ?? null,
    listFiles: () => [...Object.keys(contents), ui.path],
  })
const aliasConfig = JSON.stringify({
  compilerOptions: { baseUrl: '.', paths: { '@components/*': ['src/components/*'] } },
})

test('a test importing a changed UI file through a tsconfig path alias covers it', () => {
  for (const source of [
    "import Button from '@components/Button'",
    "import { Button } from '@components/Button.tsx'",
    "const { Button } = require('@components/Button')",
  ])
    assert.equal(
      aliasCoverage({ 'app/tsconfig.json': aliasConfig, [spec.path]: source }).covered,
      true,
      source,
    )
  for (const source of [
    "import Other from '@components/Other'",
    "import Button from 'components/Button'",
    "import Button from '@components'",
  ])
    assert.equal(
      aliasCoverage({ 'app/tsconfig.json': aliasConfig, [spec.path]: source }).covered,
      false,
      source,
    )
  assert.equal(
    aliasCoverage({ [spec.path]: "import Button from '@components/Button'" }).covered,
    false,
  )
  assert.equal(
    aliasCoverage(
      { 'app/tsconfig.json': aliasConfig, [spec.path]: "import Button from '@components/Button'" },
      [ui, { ...spec, status: 'removed' }],
    ).covered,
    false,
  )
})

test('path aliases follow wildcard, exact, longest-prefix and baseUrl resolution', () => {
  const paths = {
    '@/*': ['src/*'],
    '@ui/*': ['src/components/*', 'vendor/*'],
    '@button': ['src/components/Button.tsx'],
    '@shadow/*': ['lib/*'],
    '@shadow/components/*': ['src/components/*'],
    '@legacy/*': ['vendor/*', 'src/components/*'],
  }
  const read = (compilerOptions, source) =>
    aliasCoverage({
      'app/jsconfig.json': JSON.stringify({ compilerOptions }),
      [spec.path]: source,
    }).covered
  for (const imported of [
    '@/components/Button',
    '@ui/Button',
    '@button',
    '@shadow/components/Button',
  ])
    assert.equal(read({ paths }, `import '${imported}'`), true, imported)
  assert.equal(read({ paths }, "import '@legacy/Button'"), false)
  assert.equal(read({ paths }, "import 'src/components/Button'"), false)
  assert.equal(read({ baseUrl: './src' }, "import 'components/Button'"), true)
  assert.equal(read({ baseUrl: './src', paths }, "import 'components/Button'"), true)
  assert.equal(read({ baseUrl: 'src', paths }, "import '@/Button'"), false)
  assert.equal(
    read({ baseUrl: 'src', paths: { '#/*': ['./*'] } }, "import '#/components/Button'"),
    true,
  )
})

test('path aliases inherit through relative extends chains', () => {
  const contents = {
    'app/tsconfig.json': JSON.stringify({ extends: ['some-package/tsconfig', './tsconfig.paths'] }),
    'app/tsconfig.paths.json': JSON.stringify({
      extends: '../config/tsconfig.base.json',
      compilerOptions: { strict: true },
    }),
    'config/tsconfig.base.json': JSON.stringify({
      compilerOptions: { baseUrl: '../app/src', paths: { '@app/*': ['*'] } },
    }),
    [spec.path]: "import Button from '@app/components/Button'",
  }
  assert.equal(aliasCoverage(contents).covered, true)
  assert.equal(
    aliasCoverage({
      ...contents,
      'app/tsconfig.json': JSON.stringify({
        extends: './tsconfig.paths.json',
        compilerOptions: { paths: { '@app/*': ['lib/*'] } },
      }),
    }).covered,
    false,
  )
  assert.equal(
    aliasCoverage({
      ...contents,
      'config/tsconfig.base.json': JSON.stringify({ extends: '../app/tsconfig.json' }),
    }).covered,
    false,
  )
})

test('a module mocked through a path alias never counts as covered', () => {
  const contents = (source) => ({ 'app/tsconfig.json': aliasConfig, [spec.path]: source })
  for (const source of [
    "import Button from '@components/Button'; vi.mock('@components/Button')",
    "import Button from '@components/Button'; jest.mock('../Button')",
    "import Button from '../Button'; vi.mock('@components/Button.tsx', () => ({}))",
    "import Button from '../Button'; vi.mock(import('@components/Button'))",
  ])
    assert.equal(aliasCoverage(contents(source)).covered, false, source)
  assert.equal(
    aliasCoverage(contents("import Button from '@components/Button'; vi.mock('@components/Other')"))
      .covered,
    true,
  )
})

test('tsconfig parses leniently and a malformed one gives no alias coverage', () => {
  const lenient = `{
    // tsconfig allows comments
    "compilerOptions": {
      /* and block comments */
      "baseUrl": ".", "paths": { "@components/*": ["src/components/*",], },
    },
  }`
  const imported = "import Button from '@components/Button'"
  assert.equal(aliasCoverage({ 'app/tsconfig.json': lenient, [spec.path]: imported }).covered, true)
  for (const malformed of ['{ "compilerOptions": ', '[]', '"// not a comment"', 'null'])
    assert.equal(
      aliasCoverage({
        'app/src/tsconfig.json': malformed,
        'app/tsconfig.json': aliasConfig,
        [spec.path]: imported,
      }).covered,
      false,
      malformed,
    )
  assert.equal(
    aliasCoverage({
      'app/tsconfig.json': aliasConfig.replace('src/components/*', 'src//components/*'),
      [spec.path]: imported,
    }).covered,
    true,
  )
  assert.equal(
    aliasCoverage({
      'app/tsconfig.json': '{ "compilerOptions": ',
      [spec.path]: "import Button from '../Button'; vi.mock('@components/Button')",
    }).covered,
    false,
  )
  assert.equal(
    aliasCoverage({
      'app/tsconfig.json': JSON.stringify({ extends: './missing.json' }),
      [spec.path]: imported,
    }).covered,
    false,
  )
  assert.equal(
    testCoverage({
      files: [ui, spec],
      readContent: (path) => {
        if (path === spec.path) return imported
        throw Error('offline')
      },
    }).covered,
    false,
  )
})

test('config reads are bounded and come from the head file list through the contents API', () => {
  const configs = Array.from({ length: MAX_CONFIG_READS + 5 }, (_, i) => `p${i}/tsconfig.json`)
  const tests = configs.map((config) => ({
    path: config.replace('tsconfig.json', 'x.test.ts'),
    status: 'added',
  }))
  const read = []
  testCoverage({
    files: [ui, ...tests.slice(0, MAX_COVER_READS)],
    listFiles: () => [...configs, ...tests.map((test) => test.path)],
    readContent: (path) => {
      read.push(path)
      return path.endsWith('.test.ts') ? "import 'react'" : '{"extends": "./tsconfig.json"}'
    },
  })
  assert.equal(read.filter((path) => path.endsWith('config.json')).length, MAX_COVER_READS)
  const chain = Object.fromEntries(
    Array.from({ length: MAX_CONFIG_READS + 1 }, (_, i) => [
      `app/c${i}.json`,
      JSON.stringify({ extends: `./c${i + 1}.json` }),
    ]),
  )
  const reads = []
  assert.equal(
    testCoverage({
      files: [ui, spec],
      listFiles: () => [
        'app/tsconfig.json',
        ...Object.keys(chain),
        `app/c${MAX_CONFIG_READS + 1}.json`,
      ],
      readContent: (path) => {
        reads.push(path)
        return path === spec.path
          ? "import '@components/Button'"
          : path === 'app/tsconfig.json'
            ? JSON.stringify({ extends: './c0.json' })
            : (chain[path] ?? aliasConfig)
      },
    }).covered,
    false,
  )
  assert.equal(reads.length, MAX_CONFIG_READS + 1)
  const contents = {
    'app/tsconfig.json': aliasConfig,
    [spec.path]: "import Button from '@components/Button'",
  }
  const gh = (tree) => (_command, args) => {
    if (args[1] === 'repos/o/r/git/trees/head?recursive=1')
      return tree ? { ok: true, payload: tree } : { ok: false }
    const [, path, ref] = /^repos\/o\/r\/contents\/(.+)\?ref=(.+)$/.exec(args[1]) ?? []
    const content = ref === 'head' ? contents[decodeURIComponent(path)] : undefined
    requested.push(decodeURIComponent(path))
    return content === undefined
      ? { ok: false }
      : {
          ok: true,
          payload: {
            type: 'file',
            encoding: 'base64',
            content: Buffer.from(content).toString('base64'),
          },
        }
  }
  const files = [ui, spec].map(({ path, status }) => ({ filename: path, status }))
  /** @type {string[]} */
  let requested = []
  const listing = (truncated) => ({
    truncated,
    tree: [
      { path: 'app', type: 'tree' },
      { path: 'app/src/components/__tests__/tsconfig.json', type: 'tree' },
      ...Object.keys(contents).map((path) => ({ path, type: 'blob' })),
    ],
  })
  for (const [tree, covered] of [
    [null, false],
    [listing(true), false],
    [listing(false), true],
  ]) {
    requested = []
    assert.equal(
      readUiTestCoverage(gh(tree), { repo: 'o/r', headSha: 'head', files }).covered,
      covered,
      JSON.stringify(tree),
    )
    assert.deepEqual(requested, covered ? [spec.path, 'app/tsconfig.json'] : [spec.path])
  }
})

test('a listed config that cannot be read at the head gives no alias coverage', () => {
  for (const imported of [
    "import '@components/Button'",
    "import '../Button'; vi.mock('@components/Button')",
  ])
    for (const unreadable of [
      () => null,
      () => {
        throw Error('offline')
      },
    ])
      assert.equal(
        testCoverage({
          files: [ui, spec],
          listFiles: () => ['app/src/tsconfig.json', 'app/tsconfig.json'],
          readContent: (path) =>
            path === spec.path
              ? imported
              : path === 'app/tsconfig.json'
                ? aliasConfig
                : unreadable(),
        }).covered,
        false,
        imported,
      )
})

test('dot and dot-dot specifiers are relative, so they never resolve through baseUrl', () => {
  const index = { path: 'app/src/Button/index.tsx', status: 'modified' }
  const nested = { path: 'app/src/Button/__tests__/a.test.tsx', status: 'added' }
  const run = (source) =>
    aliasCoverage(
      {
        'app/tsconfig.json': JSON.stringify({ compilerOptions: { baseUrl: 'src/lib' } }),
        [nested.path]: source,
      },
      [index, nested],
    ).covered
  assert.equal(run("import { Button } from '..'"), true)
  assert.equal(run("import { Button } from '../index'; vi.mock('..')"), false)
  assert.equal(run("import { Button } from '../index'; vi.mock('../')"), false)
})

test('a mock is checked against every path substitution and an unusable one hides nothing', () => {
  const run = (paths, source) =>
    aliasCoverage({
      'app/tsconfig.json': JSON.stringify({ compilerOptions: { paths } }),
      [spec.path]: source,
    }).covered
  const fallback = { '@x/*': ['generated/*', 'src/components/*'] }
  assert.equal(run(fallback, "import Button from '../Button'"), true)
  assert.equal(run(fallback, "import Button from '../Button'; vi.mock('@x/Button')"), false)
  for (const paths of [{ '@x/*': 'src/components/*' }, { '@x/*': ['/abs/*'] }, { '@x/*': [] }])
    assert.equal(
      run(paths, "import Button from '../Button'; vi.mock('@x/Button')"),
      false,
      JSON.stringify(paths),
    )
})

test('an absolute baseUrl or extends, or too many path patterns, gives no alias coverage', () => {
  const crowded = Object.fromEntries(
    Array.from({ length: MAX_PATH_PATTERNS }, (_, i) => [`@p${i}/*`, ['lib/*']]),
  )
  for (const config of [
    { compilerOptions: { baseUrl: '.', paths: { ...crowded, '@components/*': ['src/*'] } } },
    { compilerOptions: { baseUrl: '/src' } },
    { extends: '/etc/tsconfig.json', compilerOptions: { baseUrl: 'src' } },
  ])
    assert.equal(
      aliasCoverage({
        'app/tsconfig.json': JSON.stringify(config),
        [spec.path]: "import 'components/Button'; import '@components/components/Button'",
      }).covered,
      false,
      JSON.stringify(config).slice(0, 80),
    )
})

const typedTests = [
  'src/Button.test.ts',
  'src/Button.test.tsx',
  'src/Button.test.mts',
  'src/Button.test.cts',
]
const typedCoverage = (source, test) =>
  coverage(source, [files[0], { path: test, status: 'added' }]).covered

test('TypeScript type arguments lex as code in every TypeScript test file', () => {
  for (const typed of [
    'const patient = (overrides: Partial<PatientFragment>) => ({ ...overrides })',
    'const [value] = useState<string>()',
    'const rows = list as Array<Foo>',
    'const accept = vi.fn<(message: string) => Promise<Result>>()',
    'const handlers = new Set<RouteHandler>()',
    'const ok = (value: unknown): value is Record<string, unknown> => true',
    'async function sign(page: Page): Promise<void> {}',
    "screen.getByRole<HTMLInputElement>('textbox', { name: /^Name/ })",
    'const nested: Promise<() => Promise<Array<Map<string, number>>>> = run()',
    'const lt = a<b && c > d',
  ])
    for (const test of typedTests) {
      assert.equal(typedCoverage(`import './Button'\n${typed}`, test), true, `${test}: ${typed}`)
      assert.equal(
        typedCoverage(`import './Button'\n${typed}\nvi.mock('./Button')`, test),
        false,
        `${test}: ${typed} then a mock`,
      )
    }
})

test('a .ts file holds no JSX, so angle-bracket generics and assertions are code', () => {
  for (const typed of [
    'const id = <T>(x: T) => x',
    'const n = <Foo>bar',
    'type F = <T>(x: T) => T',
    'const label = `${<Foo>bar}`',
  ])
    for (const test of ['src/Button.test.ts', 'src/Button.test.mts', 'src/Button.test.cts']) {
      assert.equal(typedCoverage(`import './Button'\n${typed}`, test), true, `${test}: ${typed}`)
      assert.equal(
        typedCoverage(`import './Button'\n${typed}\nvi.mock('./Button')`, test),
        false,
        `${test}: ${typed} then a mock`,
      )
    }
})

test('JSX still lexes beside type arguments and after expression keywords', () => {
  for (const source of [
    'const view = (props: Partial<Props>) => <Button {...props}>Merge</Button>',
    'const id = <T,>(x: T) => x; render(<Button />)',
    "function View(): JSX.Element { return <p>Don't merge</p> }",
    "const views = [<p key='a'>Don't</p>, <Button key='b' />]",
    "export default <p>Don't merge</p>",
  ]) {
    assert.equal(coverage(`import './Button'\n${source}`).covered, true, source)
    assert.equal(
      coverage(`import './Button'\n${source}\nvi.mock('./Button')`).covered,
      false,
      `${source} then a mock`,
    )
  }
})

test('JSX after a statement that ends in a word on an earlier line still lexes as JSX', () => {
  for (const ending of [
    'let x\n',
    'let rendered: HTMLElement\n',
    'type Label = string\n',
    'let x /* a\ncomment */ ',
  ])
    for (const text of ["Don't", 'see //docs']) {
      const markup = `${ending}<p>${text}</p>`
      assert.equal(coverage(`import './Button'\n${markup}`).covered, true, markup)
      assert.equal(
        coverage(`import './Button'\n${markup}; vi.mock('./Button')`).covered,
        false,
        `${markup} then a mock`,
      )
    }
  assert.equal(coverage("let x\n<p>import './Button' isn't real</p>\nconst y = 'z'").covered, false)
})

test('a TSX angle bracket that is neither an operand comparison nor closed JSX still rejects', () => {
  for (const source of [
    "import './Button'\nconst id = <T>(x: T) => x",
    "import './Button'\nconst id = <T extends object>(x: T) => x",
    "import './Button'\nrender(<Select<Option> />)",
  ])
    assert.equal(coverage(source).covered, false, source)
})

test('a mock that carries type arguments still disqualifies its module', () => {
  for (const mock of ['vi.mock', 'vi.doMock', 'jest.mock', 'jest.doMock'])
    for (const test of typedTests) {
      for (const source of [
        `import './Button'\n${mock}<typeof import('./Button')>('./Button', () => ({}))`,
        `import './Button'\n${mock}<Record<string, () => void>>(import('./Button'))`,
        `import './Button'\n${mock}<string('./Button')`,
      ])
        assert.equal(typedCoverage(source, test), false, `${test}: ${source}`)
      assert.equal(
        typedCoverage(`import './Button'\n${mock}<typeof import('./Other')>('./Other')`, test),
        true,
        `${test}: ${mock} of another module`,
      )
    }
})

test('lexer gaps that typed files now reach cannot hide a mock', () => {
  const typed = "import './Button'\nconst [v] = useState<string>('a')\n"
  for (const terminator of ['\r', ' ', ' '])
    assert.equal(coverage(`${typed}// note${terminator}vi.mock('./Button')`).covered, false)
  assert.equal(coverage(`${typed}vi.mock('.\\/Button')`).covered, false)
  for (const regex of ["for (const c of /'/.source) {}", "if ('a' in /'/) {}", "x instanceof /'/"])
    assert.equal(
      coverage(`${typed}${regex}\nvi.mock('./Button')\nconst q = "'"`).covered,
      false,
      regex,
    )
  assert.equal(
    coverage(`${typed}const of = 4\nconst n = of / 2; vi.mock('./Button')`).covered,
    false,
  )
  assert.equal(coverage(`${typed}const n = x.in / 2`).covered, true)
  assert.equal(coverage(`${typed}const n = x.in / 2; vi.mock('./Button')`).covered, false)
})
