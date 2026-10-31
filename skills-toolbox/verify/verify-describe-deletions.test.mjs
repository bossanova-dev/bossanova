import assert from 'node:assert/strict'
import { test } from 'node:test'

import { describeDeletions, exportedNames, MAX_SEARCHES } from './verify-describe-deletions.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'

const MODULE = new URL('./verify-describe-deletions.mjs', import.meta.url)

function change(t, before, after) {
  const repo = gitFixture(t)
  repo.write(before)
  const base = repo.commit('base')
  for (const [from, to] of Object.entries(after.moves ?? {})) repo.move(from, to)
  repo.write(after.files ?? {})
  return { repo, base, head: repo.commit('head') }
}
const describe = ({ repo, base, head }, area = 'other') =>
  runDescriberScript(MODULE, envelopeFor(repo.dir, base, head, area), repo.dir)

test('a deleted export still referenced in the head tree is breaking and names the file', (t) => {
  const result = describe(
    change(
      t,
      {
        'lib/util.js': 'export function slug(s) {\n  return s\n}\nexport const keep = 1\n',
        'app/main.js': "import { slug } from '../lib/util.js'\nslug('x')\n",
      },
      { files: { 'lib/util.js': 'export const keep = 1\n' } },
    ),
  )
  assert.equal(result.breaking, true)
  assert.ok(result.items.some((item) => item.includes('slug') && item.includes('app/main.js')))
})

test('a deleted export with no references is not breaking', (t) => {
  const result = describe(
    change(
      t,
      {
        'lib/util.go': 'package lib\n\nfunc Unused() {}\n\nfunc Keep() {}\n',
        'app/main.go': 'package main\n\nfunc main() { lib.Keep() }\n',
      },
      { files: { 'lib/util.go': 'package lib\n\nfunc Keep() {}\n' } },
    ),
  )
  assert.equal(result.breaking, false)
  assert.equal(result.items.length, 1)
})

test('an export moved to another file is not a deletion', (t) => {
  const result = describe(
    change(
      t,
      {
        'pkg/a.py': 'def helper():\n    return 1\n\ndef stay():\n    return 2\n',
        'pkg/use.py': 'from pkg.b import helper\n',
      },
      {
        files: {
          'pkg/a.py': 'def stay():\n    return 2\n',
          'pkg/b.py': 'def helper():\n    return 1\n',
        },
      },
    ),
  )
  assert.deepEqual(result.items, [])
  assert.equal(result.breaking, false)
})

test('an export moved to another module is breaking while an importer still names the old one', (t) => {
  const before = {
    'lib/old.js': 'export const value = 1\nexport const keep = 2\n',
    'app/main.js': "import { value } from '../lib/old.js'\nvalue\n",
  }
  const moved = describe(
    change(t, before, {
      files: { 'lib/old.js': 'export const keep = 2\n', 'lib/new.js': 'export const value = 1\n' },
    }),
  )
  assert.equal(moved.breaking, true)
  assert.ok(moved.items.some((item) => item.includes('value') && item.includes('app/main.js')))

  const unrelated = describe(
    change(t, before, {
      files: {
        'lib/old.js': 'export const keep = 2\n',
        'other/x.ts': 'export function value() {}\n',
      },
    }),
  )
  assert.equal(unrelated.breaking, true)
})

test('an export redefined in its own module or Go package is not a deletion', (t) => {
  const edited = describe(
    change(
      t,
      {
        'lib/util.js': 'export function slug(s) {\n  return s\n}\n',
        'app/main.js': "import { slug } from '../lib/util.js'\n",
      },
      { files: { 'lib/util.js': 'export function slug(s, sep) {\n  return s + sep\n}\n' } },
    ),
  )
  assert.deepEqual(edited.items, [])

  const samePackage = describe(
    change(
      t,
      {
        'lib/a.go': 'package lib\n\nfunc Run() {}\n',
        'lib/b.go': 'package lib\n',
        'app/main.go': 'package main\n\nfunc main() { lib.Run() }\n',
      },
      { files: { 'lib/a.go': 'package lib\n', 'lib/b.go': 'package lib\n\nfunc Run() {}\n' } },
    ),
  )
  assert.deepEqual(samePackage.items, [])
  assert.equal(samePackage.breaking, false)
})

test('a deleted file that is still imported is breaking; an unreferenced one is not', (t) => {
  const imported = describe(
    change(
      t,
      {
        'src/lib/format.ts': 'const x = 1\n',
        'src/app.ts': "import './lib/format'\n",
        'src/lib/keep.ts': 'const y = 2\n',
      },
      { files: { 'src/lib/format.ts': null } },
    ),
  )
  assert.equal(imported.breaking, true)
  assert.ok(imported.items.some((item) => item.includes('src/app.ts')))

  const unreferenced = describe(
    change(
      t,
      { 'src/lib/orphan.ts': 'const x = 1\n', 'src/lib/keep.ts': '' },
      { files: { 'src/lib/orphan.ts': null } },
    ),
  )
  assert.equal(unreferenced.breaking, false)
})

test('a deleted file imported by a relative sibling, child or default-only importer is breaking', (t) => {
  // No named exports, so only the file-reference scan can find the importer.
  const button = 'const x = 1\n'
  const sibling = describe(
    change(
      t,
      {
        'src/components/Button.tsx': button,
        'src/components/Form.tsx': "import { Button } from './Button'\n",
        'src/keep.ts': '',
      },
      { files: { 'src/components/Button.tsx': null } },
    ),
    'ui',
  )
  assert.equal(sibling.breaking, true)
  assert.ok(sibling.items.some((item) => item.includes('src/components/Form.tsx')))

  const child = describe(
    change(
      t,
      {
        'src/components/Button.tsx': button,
        'src/components/sub/X.tsx': "import { Button } from '../Button.js'\n",
      },
      { files: { 'src/components/Button.tsx': null } },
    ),
    'ui',
  )
  assert.equal(child.breaking, true)
  assert.ok(child.items.some((item) => item.includes('src/components/sub/X.tsx')))

  const defaultOnly = describe(
    change(
      t,
      {
        'src/components/Logo.tsx': 'export default function Logo() {\n  return null\n}\n',
        'src/components/Header.tsx': "import Logo from './Logo'\n",
      },
      { files: { 'src/components/Logo.tsx': null } },
    ),
    'ui',
  )
  assert.equal(defaultOnly.breaking, true)

  const unrelated = describe(
    change(
      t,
      {
        'src/components/Button.tsx': button,
        'src/components/Form.tsx':
          "import { Button } from './other/Button'\nconst Button2 = 'Button'\n",
      },
      { files: { 'src/components/Button.tsx': null } },
    ),
    'ui',
  )
  assert.equal(unrelated.breaking, false)
})

test('a deleted directory imported by a relative sibling is breaking', (t) => {
  const result = describe(
    change(
      t,
      {
        'src/components/legacy/a.ts': 'const a = 1\n',
        'src/components/Form.ts': "import { a } from './legacy/a'\n",
      },
      { files: { 'src/components/legacy': null } },
    ),
    'ui',
  )
  assert.equal(result.breaking, true)
  assert.ok(result.items.some((item) => item.includes('src/components/Form.ts')))
})

test('a deleted directory is searched by its path', (t) => {
  const result = describe(
    change(
      t,
      {
        'internal/legacy/a.go': 'package legacy\n',
        'internal/legacy/b.go': 'package legacy\n',
        'cmd/main.go': 'package main\n\nimport _ "example.com/m/internal/legacy"\n',
      },
      { files: { 'internal/legacy': null } },
    ),
  )
  assert.equal(result.breaking, true)
  assert.ok(result.items.some((item) => item.includes('cmd/main.go')))
})

test('a rename is not a deletion', (t) => {
  const fixture = change(
    t,
    {
      'lib/old.js': 'export const value = 1\n',
      'app.js': "import { value } from './lib/old.js'\n",
    },
    {
      moves: { 'lib/old.js': 'lib/new.js' },
      files: { 'app.js': "import { value } from './lib/new.js'\n" },
    },
  )
  for (const area of ['housekeeping', 'other']) {
    const result = describe(fixture, area)
    assert.deepEqual(result.items, [], area)
    assert.equal(result.breaking, false)
  }
})

test('searches beyond the cap count as unchecked, which is breaking', (t) => {
  const before = Object.fromEntries(
    Array.from({ length: MAX_SEARCHES + 2 }, (_, i) => [`gone/d${i}/f.txt`, 'x\n']),
  )
  before['keep/k.txt'] = 'k\n'
  const deletions = Object.fromEntries(
    Object.keys(before)
      .filter((f) => f.startsWith('gone/'))
      .map((f) => [f, null]),
  )
  // A parent that survives keeps each directory its own search target.
  before['gone/stay.txt'] = 's\n'
  const fixture = change(t, before, { files: deletions })
  const result = describeDeletions(
    envelopeFor(fixture.repo.dir, fixture.base, fixture.head, 'other'),
    {
      cwd: fixture.repo.dir,
    },
  )
  assert.equal(result.breaking, true)
})

test('exportedNames reads JS/TS, Go and Python export forms', () => {
  assert.deepEqual(exportedNames('js', 'export async function load() {'), ['load'])
  assert.deepEqual(exportedNames('js', 'export { a, b as c, default } from "./x"'), ['a', 'c'])
  assert.deepEqual(exportedNames('js', 'export default function main() {}'), [])
  assert.deepEqual(exportedNames('js', 'const local = 1'), [])
  assert.deepEqual(exportedNames('go', 'func (s *Server) Run() {'), [])
  assert.deepEqual(exportedNames('go', 'func Run() {'), ['Run'])
  assert.deepEqual(exportedNames('go', 'type config struct{}'), [])
  assert.deepEqual(exportedNames('py', 'def _private():'), [])
  assert.deepEqual(exportedNames('py', '    def method(self):'), [])
  assert.deepEqual(exportedNames('py', 'class Widget:'), ['Widget'])
})
