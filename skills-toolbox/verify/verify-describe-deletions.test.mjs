import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { test } from 'node:test'

import {
  describeDeletions,
  exportedNames,
  MAX_SEARCHES,
  staleImporter,
} from './verify-describe-deletions.mjs'
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

test('moved JS exports distinguish reachable imports from incidental mentions', (t) => {
  const cases = [
    [
      "import { value } from '../lib/new.js'\nimport { keep } from '../lib/old.js'\nvalue + keep\n",
      false,
    ],
    ["const old = 'old'\nconst value = 1\n", false],
    ["import * as old from '../lib/old.js'\nold.value\n", true],
    ["import {\n value,\n} from '../lib/old.js'\n", true],
    ["const value = require('../lib/old.js').value\nvalue()\n", true],
    ["require('../lib/old.js').value()\n", true],
    ["const keep = require('../lib/old.js').keep\nvalue()\n", false],
    ["import {\n value } from '../lib/old.js'\n", true],
    ["const {\n value } = require('../lib/old.js')\n", true],
    ["import * as ns\n from '../lib/old.js'\nns.value()\n", true],
    ["import\n * as ns from '../lib/old.js'\nns.value()\n", true],
    ["const { value = 1 } = require('../lib/old.js')\n", true],
    ["const {\n value = 1 } = require('../lib/old.js')\n", true],
    ["const { keep = value } = require('../lib/old.js')\n", false],
  ]
  for (const declaration of [
    "import * as ns from '../lib/old.js'",
    "const ns = require('../lib/old.js')",
  ]) {
    for (const usage of [
      'ns?.value',
      "ns['value']",
      'ns?.["value"]',
      'const { value } = ns',
      'const { value: local = 0 } = ns',
      '({ value } = ns)',
      'const { value } = ns;',
      'const { value } = ns, other = 0',
    ])
      cases.push([`${declaration}\n${usage}\n`, true])
    for (const usage of [
      "ns['keep']; value",
      'ns?.keep; value',
      'const { keep = value } = ns',
      'const { value } = nsOther',
      'const { value } = ns.sub',
      'const { value } = ns.default',
      'const { value } = ns . sub',
      "const { value } = ns['sub']",
      'const { value } = ns ["sub"]',
      'const { value } = ns?.sub',
      "const { value } = ns?.['sub']",
      "otherns['value']",
    ])
      cases.push([`${declaration}\n${usage}\n`, false])
  }
  for (const [source, breaking] of cases) {
    const result = describe(
      change(
        t,
        {
          'lib/old.js': 'export const value = 1\nexport const keep = 2\n',
          'app/main.js': source,
        },
        {
          files: {
            'lib/old.js': 'export const keep = 2\n',
            'lib/new.js': '// moved from old.js\nexport const value = 1\n',
          },
        },
      ),
    )
    assert.equal(result.breaking, breaking, source)
    if (breaking) assert.ok(result.items.some((item) => item.includes('app/main.js')))
    assert.ok(result.items.every((item) => !item.includes('lib/new.js')))
  }
})

test('embedded JS importers retain stale references without treating prose as imports', (t) => {
  for (const extension of ['mdx', 'vue', 'svelte', 'html', 'unknown']) {
    for (const [source, breaking] of [
      ["import { value } from '../lib/old.js'\nvalue\n", true],
      ["import value from '../lib/old.js'\nvalue()\n", true],
      ["import value from '../lib/new.js'\n// old\nvalue()\n", false],
      ["import { value } from '../lib/new.js'\nimport { keep } from '../lib/old.js'\n", false],
      ['The old value is documented here.\n', false],
    ]) {
      const file = `app/main.${extension}`
      const result = describe(
        change(
          t,
          {
            'lib/old.js': 'export const value = 1\nexport const keep = 2\n',
            [file]: source,
          },
          {
            files: {
              'lib/old.js': 'export const keep = 2\n',
              'lib/new.js': 'export const value = 1\n',
            },
          },
        ),
      )
      assert.equal(result.breaking, breaking, `${extension}: ${source}`)
      if (breaking) assert.ok(result.items.some((item) => item.includes(file)))
    }
  }
})

test('Python from-package imports can bind the old module', (t) => {
  for (const [source, breaking] of [
    ['from pkg import old\nold.helper()\n', true],
    ['from pkg import old as legacy\nlegacy.helper()\n', true],
    ['from . import old\nold.helper()\n', true],
    ['from . import old as legacy\nlegacy.helper()\n', true],
    ['from pkg import old\nold.stay()\nhelper()\n', false],
    ['from other import old\nold.helper()\n', true],
    ['from ..other import old\nold.helper()\n', false],
  ]) {
    const result = describe(
      change(
        t,
        {
          'pkg/old.py': 'def helper():\n    return 1\ndef stay():\n    return 2\n',
          'pkg/main.py': source,
        },
        {
          files: {
            'pkg/old.py': 'def stay():\n    return 2\n',
            'pkg/new.py': 'def helper():\n    return 1\n',
          },
        },
      ),
    )
    assert.equal(result.breaking, breaking, source)
    if (breaking) assert.ok(result.items.some((item) => item.includes('pkg/main.py')))
  }
})

test('moved Go exports require a use through the old package', (t) => {
  for (const qualifier of ['newpkg', 'old']) {
    const result = describe(
      change(
        t,
        {
          'old/a.go': 'package old\nfunc Value() {}\nfunc Keep() {}\n',
          'app/main.go': `package main\nimport (\n "example.com/m/old"\n "example.com/m/newpkg"\n)\nfunc main() { ${qualifier}.Value(); old.Keep() }\n`,
        },
        {
          files: {
            'old/a.go': 'package old\nfunc Keep() {}\n',
            'newpkg/a.go': 'package newpkg\nfunc Value() {}\n',
          },
        },
      ),
    )
    assert.equal(result.breaking, qualifier === 'old')
    if (qualifier === 'old') assert.ok(result.items.some((item) => item.includes('app/main.go')))
  }
})

test('moved Python exports distinguish migrated named imports', (t) => {
  for (const [source, breaking] of [
    ['from pkg.a import helper\nfrom pkg.a import stay\n', true],
    ['from pkg.b import helper\nfrom pkg.a import stay\n', false],
    ['from pkg.a import helper; helper()\n', true],
    ['from pkg.a import stay, \\\n helper\n', true],
    ['from pkg.b import stay, \\\n helper\n', false],
  ]) {
    const result = describe(
      change(
        t,
        {
          'pkg/a.py': 'def helper():\n    return 1\ndef stay():\n    return 2\n',
          'app/main.py': source,
        },
        {
          files: {
            'pkg/a.py': 'def stay():\n    return 2\n',
            'pkg/b.py': 'def helper():\n    return 1\n',
          },
        },
      ),
    )
    assert.equal(result.breaking, breaking, source)
    if (breaking) assert.ok(result.items.some((item) => item.includes('app/main.py')))
  }
})

test('each moved export spends exactly one search', (t) => {
  const names = ['one', 'two', 'three']
  const definitions = names.map((name) => `export const ${name} = 1\n`).join('')
  const fixture = change(
    t,
    {
      'lib/old.js': definitions + 'export const keep = 1\n',
      'app/main.js': "import { one, two, three } from '../lib/old.js'\n",
    },
    { files: { 'lib/old.js': 'export const keep = 1\n', 'lib/new.js': definitions } },
  )
  let searches = 0
  const result = describeDeletions(
    envelopeFor(fixture.repo.dir, fixture.base, fixture.head, 'other'),
    {
      cwd: fixture.repo.dir,
      run(command, args, options) {
        if (command === 'git' && args[0] === 'grep') searches += 1
        return spawnSync(command, args, options)
      },
    },
  )
  assert.equal(result.breaking, true)
  assert.equal(searches, names.length)
})

test('stale importer recognizers cover named, qualified and ambiguous bindings', () => {
  const cases = [
    ['js', "import type { value as local } from '../lib/old.js'", true],
    ['js', "export { value as renamed } from '../lib/old.js'", true],
    ['js', "const { value: local } = require('../lib/old.js')", true],
    ['js', "const ns = require('../lib/old.js'); ns.value", true],
    ['js', "const value = require('../lib/old.js').value; value()", true],
    ['js', "require('../lib/old.js').value()", true],
    ['js', "value } from '../lib/old.js'", true],
    ['js', "value } = require('../lib/old.js')", true],
    ['js', "import ns from '../lib/old.js'; ns.value", true],
    ['js', "import ns from '../lib/old.js'; ns.keep; value", false],
    ['js', "import { keep } from '../lib/old.js'; value", false],
    ['js', "import '../lib/old.js'; value", false],
    ['js', "export * from '../lib/old.js'; value", true],
    ['js', "const lazy = import('../lib/old.js'); value", true],
    ['js', "} = require('../lib/old.js'); value", true],
    ['js', "import { value } from '../else/old.js'", false],
    ['js', "import { value } from '@alias/old'", true],
    ['js', "import $old from '../lib/old.js'; $old.value", true],
    ['go', 'import legacy "example.com/m/old"; legacy.value', true],
    ['go', 'import legacy "example.com/m/old"; old.value', false],
    ['go', 'import . "example.com/m/old"; value', true],
    ['go', 'import _ "example.com/m/old"; value', false],
    ['py', 'from pkg.old import value as local', true],
    ['py', 'from pkg.old import value; value()', true],
    ['py', 'from pkg.old import keep, \\', true],
    ['py', 'from pkg.new import keep, \\', false],
    ['py', 'from pkg.old import keep; value', false],
    ['py', 'from pkg.old import (', true],
    ['py', 'from pkg.old import *', true],
    ['py', 'import pkg.old as legacy; legacy.value', true],
    ['py', 'import pkg.old; pkg.old.value', true],
    ['py', 'from ..lib.old import value', true],
    ['py', 'from .old import value', false],
  ]
  for (const [lang, line, expected] of cases) {
    assert.equal(
      staleImporter(lang, 'app/main.js', [line], { key: 'value', movedFrom: 'lib/old' }),
      expected,
      line,
    )
  }
  assert.equal(
    staleImporter('go', 'lib/old/sibling.go', ['value()'], { key: 'value', movedFrom: 'lib/old' }),
    true,
  )
})
