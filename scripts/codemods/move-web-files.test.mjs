import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { applyMoves, verifyPure } from './move-web-files.mjs'

const src = 'services/web/src/'
const moves = [[`${src}components/Panel.tsx`, `${src}shared/ui/Panel.tsx`]]
const git = (root, ...args) => execFileSync('git', args, { cwd: root, encoding: 'utf8' })
function fixture(t, files) {
  const root = mkdtempSync(path.join(tmpdir(), 'web-move-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const write = (name, body) => {
    mkdirSync(path.dirname(path.join(root, name)), { recursive: true })
    writeFileSync(path.join(root, name), body)
  }
  for (const [name, body] of Object.entries({
    'scripts/codemods/web-moves.json': '[]\n',
    ...files,
  }))
    write(name, body)
  git(root, 'init', '-q')
  git(root, 'config', 'commit.gpgsign', 'false')
  git(root, 'config', 'user.name', 'Fixture')
  git(root, 'config', 'user.email', 'fixture@example.com')
  git(root, 'add', '.')
  git(root, 'commit', '-qm', 'fixture')
  return { root, write, read: (file) => readFileSync(path.join(root, file), 'utf8') }
}

test('moves components and co-tests, resolving every module literal and preserving unrelated text', (t) => {
  const f = fixture(t, {
    [`${src}components/Panel.tsx`]:
      "import './Panel.css'\nimport { helper } from '../lib/helper'\n// '~/components/Panel'\nexport default function Panel() { return <div>{helper}</div> }\n",
    [`${src}components/Panel.css`]: '.panel {}\n',
    [`${src}lib/helper.ts`]: 'export const helper = 1\n',
    [`${src}components/Panel.test.tsx`]:
      "vi.mock('~/components/Panel')\nvi.importActual('./Panel')\ntype T = import('./Panel').default\nimportOriginal<typeof import('./Panel')>()\n",
    'services/web/e2e/user.ts':
      "export { default } from '~/components/Panel'\nconst p = import('../src/components/Panel')\nimport { helper } from '../src/lib/helper.js'\nconst text = '~/components/Panel'\n",
  })
  applyMoves({
    root: f.root,
    moves: [...moves, [`${src}components/Panel.css`, `${src}shared/ui/Panel.css`]],
  })
  assert.equal(
    f.read(`${src}shared/ui/Panel.tsx`),
    "import './Panel.css'\nimport { helper } from '~/lib/helper'\n// '~/components/Panel'\nexport default function Panel() { return <div>{helper}</div> }\n",
  )
  assert.equal(
    f.read(`${src}shared/ui/Panel.test.tsx`),
    "vi.mock('./Panel')\nvi.importActual('./Panel')\ntype T = import('./Panel').default\nimportOriginal<typeof import('./Panel')>()\n",
  )
  assert.equal(
    f.read('services/web/e2e/user.ts'),
    "export { default } from '~/shared/ui/Panel'\nconst p = import('~/shared/ui/Panel')\nimport { helper } from '../src/lib/helper.js'\nconst text = '~/components/Panel'\n",
  )
  assert.equal(JSON.parse(f.read('scripts/codemods/web-moves.json')).length, 3)
})

test('moved co-tests keep dependencies outside src relative to their destination', (t) => {
  const f = fixture(t, {
    [`${src}components/Panel.tsx`]: 'export default 1\n',
    [`${src}components/Panel.test.tsx`]:
      "import { helper } from '../../tests/helper'\nconst data = import('../../tests/data.json?raw')\n",
    'services/web/tests/helper.ts': 'export const helper = 1\n',
    'services/web/tests/data.json': '{}\n',
  })
  applyMoves({ root: f.root, moves })
  assert.equal(
    f.read(`${src}shared/ui/Panel.test.tsx`),
    "import { helper } from '../../../tests/helper'\nconst data = import('../../../tests/data.json?raw')\n",
  )
  assert.equal(f.read('services/web/tests/helper.ts'), 'export const helper = 1\n')
})

test('rewrites complete repository and web-relative path tokens and sorts baselines', (t) => {
  const f = fixture(t, {
    [`${src}components/Panel.tsx`]: 'export default 1\n',
    'docs/guide.md': `${src}components/Panel.tsx and ${src}components/Panel.tsx.backup\n`,
    'scripts/example-baseline.json': JSON.stringify({
      [`${src}components/Panel.tsx`]: 2,
      [`${src}pages/Other.tsx`]: 3,
    }),
    'services/web/biome.json': '{"includes":["src/components/Panel.tsx"]}',
    'scripts/guards/web-filenames-baseline.json': '["src/components/Panel.tsx"]',
    [`${src}gen/untouched.ts`]: `// ${src}components/Panel.tsx\n`,
  })
  applyMoves({ root: f.root, moves })
  assert.equal(
    f.read('docs/guide.md'),
    `${src}shared/ui/Panel.tsx and ${src}components/Panel.tsx.backup\n`,
  )
  assert.deepEqual(Object.keys(JSON.parse(f.read('scripts/example-baseline.json'))), [
    `${src}pages/Other.tsx`,
    `${src}shared/ui/Panel.tsx`,
  ])
  assert.match(f.read('services/web/biome.json'), /src\/shared\/ui\/Panel.tsx/)
  assert.match(f.read('scripts/guards/web-filenames-baseline.json'), /src\/shared\/ui\/Panel.tsx/)
  assert.equal(f.read(`${src}gen/untouched.ts`), `// ${src}components/Panel.tsx\n`)
})

test('preflights unresolved imports in unmoved files before changing files or index', (t) => {
  const f = fixture(t, {
    [`${src}components/Panel.tsx`]: 'export default 1\n',
    [`${src}user.ts`]: "import '~/missing'\n",
  })
  const before = git(f.root, 'ls-files', '--stage')
  assert.throws(() => applyMoves({ root: f.root, moves }), /user.ts:1:.*unresolved/)
  assert.ok(existsSync(path.join(f.root, moves[0][0])))
  assert.equal(git(f.root, 'ls-files', '--stage'), before)
  assert.equal(f.read('scripts/codemods/web-moves.json'), '[]\n')
})

test('dry-run returns the plan without writing or moving', (t) => {
  const f = fixture(t, { [`${src}components/Panel.tsx`]: 'export default 1\n' })
  const before = git(f.root, 'status', '--porcelain')
  assert.equal(applyMoves({ root: f.root, moves, dryRun: true }).moves.length, 1)
  assert.equal(git(f.root, 'status', '--porcelain'), before)
  assert.equal(f.read('scripts/codemods/web-moves.json'), '[]\n')
})

test('pure verification accepts module-only renames and reports changed behavior', (t) => {
  const body =
    "import { helper } from '../lib/helper'\nexport default function Panel() { return <div>{helper}</div> }\n"
  const f = fixture(t, {
    [`${src}components/Panel.tsx`]: body,
    [`${src}lib/helper.ts`]: 'export const helper = 1\n',
  })
  const base = git(f.root, 'rev-parse', 'HEAD').trim()
  applyMoves({ root: f.root, moves })
  git(f.root, 'add', '.')
  git(f.root, 'commit', '-qm', 'move')
  assert.deepEqual(verifyPure({ root: f.root, base }).violations, [])
  f.write(moves[0][1], f.read(moves[0][1]).replace('{helper}', '{helper + 1}'))
  git(f.root, 'add', '.')
  git(f.root, 'commit', '-qm', 'change')
  assert.deepEqual(verifyPure({ root: f.root, base }).violations, [moves[0][1]])
})
