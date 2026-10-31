import assert from 'node:assert/strict'
import { test } from 'node:test'

import { realDiff } from './verify-git-fixture.mjs'
import {
  importLines,
  isPureMoveFile,
  pureMoveSummary,
  renamePairs,
  stripModulePath,
} from './verify-moves.mjs'

const patch = (removed, added) =>
  ['@@ -1 +1 @@', ...removed.map((l) => `-${l}`), ...added.map((l) => `+${l}`)].join('\n')
const renamed = (previousPath, path, p = '') => ({
  path,
  previousPath,
  status: 'renamed',
  changes: p ? p.split('\n').length - 1 : 0,
  patch: p,
})
const modified = (path, p) => ({ path, status: 'modified', changes: 2, patch: p })
const pure = (file, files) => isPureMoveFile(file, renamePairs(files))

const lines = (...list) => list.join('\n') + '\n'
const FILLER = Array.from({ length: 12 }, (_, i) => `// line ${i}`)
const realPure = (files, file) =>
  pure(
    files.find((entry) => entry.path === file),
    files,
  )

test('stripModulePath drops the extension and a trailing index', () => {
  assert.equal(stripModulePath('src/a/index.tsx'), 'src/a')
  assert.equal(stripModulePath('pkg/mod/__init__.py'), 'pkg/mod')
  assert.equal(stripModulePath('src/a/b.test.ts'), 'src/a/b.test')
  assert.equal(stripModulePath('Makefile'), 'Makefile')
})

test('a JS/TS directory move whose importers change only relative specifiers is pure', () => {
  const files = [
    renamed('src/widgets/Button.tsx', 'src/ui/Button.tsx'),
    renamed('src/widgets/Card.tsx', 'src/ui/Card.tsx'),
    modified(
      'src/app/Page.tsx',
      patch(
        ["import { Button } from '../widgets/Button'", "import Card from '../widgets/Card.js';"],
        ["import { Button } from '../ui/Button'", "import Card from '../ui/Card.js';"],
      ),
    ),
    modified(
      'src/app/lazy.ts',
      patch(
        ["const m = await import('../widgets/Card')"],
        ["const m = await import('../ui/Card')"],
      ),
    ),
    modified('src/app/multi.ts', patch(["} from '../widgets/Button'"], ["} from '../ui/Button'"])),
  ]
  for (const file of files) assert.equal(pure(file, files), true, file.path)
  assert.deepEqual(pureMoveSummary(files), { pure: true, moved: 2, importOnly: 3, other: 0 })
})

test("a moved importer whose './x' becomes '../x' is pure", () => {
  const files = [
    renamed(
      'src/a/user.ts',
      'src/a/deep/user.ts',
      patch(["import { x } from './x'"], ["import { x } from '../x'"]),
    ),
  ]
  assert.equal(pure(files[0], files), true)
})

test('an alias-path module specifier is explained by a file-level rename', () => {
  const files = [
    renamed('src/components/Foo.tsx', 'src/ui/Foo.tsx'),
    modified(
      'src/page.tsx',
      patch(["import Foo from '@/components/Foo'"], ["import Foo from '@/ui/Foo'"]),
    ),
  ]
  assert.equal(pure(files[1], files), true)
})

test('a Go package move with import-path-only importers is pure', () => {
  const files = [
    renamed('internal/old/store/store.go', 'internal/data/store/store.go'),
    renamed('internal/old/store/log/log.go', 'internal/data/store/log/log.go'),
    modified(
      'cmd/main.go',
      patch(
        ['\t"example.com/m/internal/old/store"', '\tlog "example.com/m/internal/old/store/log"'],
        [
          '\tlog "example.com/m/internal/data/store/log"',
          '',
          '\t"example.com/m/internal/data/store"',
        ],
      ),
    ),
    modified(
      'cmd/single.go',
      patch(
        ['import "example.com/m/internal/old/store"'],
        ['import "example.com/m/internal/data/store"'],
      ),
    ),
  ]
  for (const file of files) assert.equal(pure(file, files), true, file.path)
})

test('a Python dotted-path move is pure', () => {
  const files = [
    renamed('app/utils/text.py', 'app/common/text.py'),
    modified(
      'app/main.py',
      patch(
        ['from app.utils.text import slug', 'import app.utils.text as t'],
        ['from app.common.text import slug', 'import app.common.text as t'],
      ),
    ),
  ]
  assert.equal(pure(files[1], files), true)
})

test('not pure: unexplained targets, alias or name changes, other lines, unsupported languages', () => {
  const move = renamed('src/a/x.ts', 'src/b/x.ts')
  const cases = {
    unmoved: modified(
      'src/m.ts',
      patch(["import { x } from './a/y'"], ["import { x } from './b/y'"]),
    ),
    otherModule: modified(
      'src/m.ts',
      patch(["import { x } from 'left-pad'"], ["import { x } from 'right-pad'"]),
    ),
    name: modified('src/m.ts', patch(["import { x } from './a/x'"], ["import { z } from './b/x'"])),
    alias: modified('main.go', patch(['\ta "example.com/m/src/a"'], ['\tb "example.com/m/src/b"'])),
    unmovedSubpackage: modified(
      'main.go',
      patch(['\t"example.com/m/src/a/sub"'], ['\t"example.com/m/src/b/sub"']),
    ),
    behaviour: modified(
      'src/m.ts',
      patch(["import { x } from './a/x'"], ["import { x } from './b/x'", 'x()']),
    ),
    goReturn: modified('main.go', patch(['\treturn "src/a"'], ['\treturn "src/b"'])),
    language: modified('src/m.rb', patch(["require 'a/x'"], ["require 'b/x'"])),
    added: {
      path: 'src/new.ts',
      status: 'added',
      changes: 1,
      patch: patch([], ["import './b/x'"]),
    },
    noPatch: { path: 'src/m.ts', status: 'modified', changes: 3 },
  }
  for (const [name, file] of Object.entries(cases))
    assert.equal(pure(file, [move, file]), false, name)
  assert.deepEqual(pureMoveSummary([move, cases.behaviour]), {
    pure: false,
    moved: 1,
    importOnly: 0,
    other: 1,
  })
})

test('importLines is null for any non-import line and ignores blank lines', () => {
  assert.equal(importLines('a.ts', patch(['const a = 1'], [])), null)
  assert.equal(importLines('a.txt', patch(["import 'x'"], [])), null)
  assert.deepEqual(importLines('a.ts', patch(['   '], [''])), { removed: [], added: [], hunks: [] })
})

test('shuffled inputs give identical output', () => {
  const files = [
    renamed('src/a/x.ts', 'src/b/x.ts'),
    renamed('src/a/y.ts', 'src/b/y.ts'),
    renamed('lib/z.go', 'pkg/z.go'),
    modified(
      'src/m.ts',
      patch(
        ["import { y } from './a/y'", "import { x } from './a/x'"],
        ["import { x } from './b/x'", "import { y } from './b/y'"],
      ),
    ),
  ]
  const shuffled = [files[3], files[1], files[2], files[0]]
  assert.deepEqual(renamePairs(shuffled), renamePairs(files))
  assert.deepEqual(pureMoveSummary(shuffled), pureMoveSummary(files))
  assert.equal(pure(files[3], shuffled), true)
})

test('a modified file is pure only when a rename translates one of its specifiers', () => {
  const move = renamed('src/a/x.ts', 'src/b/x.ts')
  const cases = {
    reorderOnly: modified(
      'src/m.tsx',
      patch(
        ["import { y } from './y'", "import { z } from './z'"],
        ["import { z } from './z'", "import { y } from './y'"],
      ),
    ),
    blankOnly: modified('src/m.tsx', patch([''], ['   '])),
    goReorderOnly: modified('cmd/sorted.go', patch(['\t"fmt"', '\t"os"'], ['\t"os"', '\t"fmt"'])),
  }
  for (const [name, file] of Object.entries(cases))
    assert.equal(pure(file, [move, file]), false, name)
})

test('a relative specifier that changes target without a rename is not pure', () => {
  const retarget = modified(
    'src/m.ts',
    patch(["import { a } from './a.js'"], ["import { a } from './a/index.js'"]),
  )
  assert.equal(pure(retarget, [retarget]), false, 'no rename, ./a.js -> ./a/index.js')
  const unrelated = renamed('src/z/q.ts', 'src/y/q.ts')
  assert.equal(pure(retarget, [unrelated, retarget]), false, 'unrelated rename elsewhere')
})

test('real git: an unchanged side-effect import a binding import moves across is not pure', (t) => {
  const files = realDiff(
    t,
    {
      'src/a/x.ts': 'export const x = 1\n',
      'src/m.tsx': lines(
        "import './polyfill'",
        "import { x } from './a/x'",
        '',
        'export const y = x',
      ),
    },
    [['src/a/x.ts', 'src/b/x.ts']],
    {
      'src/m.tsx': lines(
        "import { x } from './b/x'",
        "import './polyfill'",
        '',
        'export const y = x',
      ),
    },
  )
  assert.equal(realPure(files, 'src/b/x.ts'), true, 'the zero-change rename itself')
  assert.equal(realPure(files, 'src/m.tsx'), false)
})

test('real git: a renamed side-effect import that also changes position is not pure', (t) => {
  const files = realDiff(
    t,
    {
      'src/x.css': '.x { color: red }\n',
      'src/m.tsx': lines("import './p.css'", "import './x.css'", '', 'export const y = 1'),
    },
    [['src/x.css', 'src/x2.css']],
    { 'src/m.tsx': lines("import './x2.css'", "import './p.css'", '', 'export const y = 1') },
  )
  assert.equal(realPure(files, 'src/m.tsx'), false)
})

test('real git: swapping unchanged side-effect imports beside a rename rewrite is not pure', (t) => {
  const before = {
    'src/a/x.ts': 'export const x = 1\n',
    'src/m.tsx': lines(
      "import './reset.css'",
      "import './theme.css'",
      "import { x } from './a/x'",
      '',
      'export const y = x',
    ),
  }
  const swapped = realDiff(t, before, [['src/a/x.ts', 'src/b/x.ts']], {
    'src/m.tsx': lines(
      "import { x } from './b/x'",
      "import './theme.css'",
      "import './reset.css'",
      '',
      'export const y = x',
    ),
  })
  assert.equal(realPure(swapped, 'src/m.tsx'), false, 'swapped CSS imports')
  const crossed = realDiff(t, before, [['src/a/x.ts', 'src/b/x.ts']], {
    'src/m.tsx': lines(
      "import './reset.css'",
      "import { x } from './b/x'",
      "import './theme.css'",
      '',
      'export const y = x',
    ),
  })
  assert.equal(realPure(crossed, 'src/m.tsx'), false, 'binding import crossed a CSS import')
  const goBlank = realDiff(
    t,
    {
      'src/a/x/x.go': 'package x\n',
      'main.go': lines(
        'package main',
        '',
        'import (',
        '\t_ "example.com/m/a"',
        '\t_ "example.com/m/b"',
        '\t"example.com/m/src/a/x"',
        ')',
      ),
    },
    [['src/a/x/x.go', 'src/b/x/x.go']],
    {
      'main.go': lines(
        'package main',
        '',
        'import (',
        '\t"example.com/m/src/b/x"',
        '\t_ "example.com/m/b"',
        '\t_ "example.com/m/a"',
        ')',
      ),
    },
  )
  assert.equal(realPure(goBlank, 'main.go'), false, 'swapped Go blank imports')
})

test('real git: in-place rewrites and re-sorting within one changed block stay pure', (t) => {
  const inPlace = realDiff(
    t,
    {
      'src/a/x.ts': 'export const x = 1\n',
      'src/m.tsx': lines(
        "import './p.css'",
        "import { x } from './a/x'",
        "import './q.css'",
        '',
        'export const y = x',
      ),
    },
    [['src/a/x.ts', 'src/b/x.ts']],
    {
      'src/m.tsx': lines(
        "import './p.css'",
        "import { x } from './b/x'",
        "import './q.css'",
        '',
        'export const y = x',
      ),
    },
  )
  assert.equal(realPure(inPlace, 'src/m.tsx'), true, 'in-place rewrite between CSS imports')
  const resorted = realDiff(
    t,
    {
      'src/a/x.ts': 'export const x = 1\n',
      'src/a/y.ts': 'export const y = 2\n',
      'src/m.ts': lines("import { y } from './a/y'", "import { x } from './a/x'", '', 'x(y)'),
    },
    [
      ['src/a/x.ts', 'src/b/x.ts'],
      ['src/a/y.ts', 'src/b/y.ts'],
    ],
    { 'src/m.ts': lines("import { x } from './b/x'", "import { y } from './b/y'", '', 'x(y)') },
  )
  assert.equal(realPure(resorted, 'src/m.ts'), true, 'adjacent rewritten imports re-sorted')
  const cssMove = realDiff(
    t,
    {
      'src/a/x.css': '.x { color: red }\n',
      'src/m.tsx': lines("import './p.css'", "import './a/x.css'", '', 'export const y = 1'),
    },
    [['src/a/x.css', 'src/b/x.css']],
    { 'src/m.tsx': lines("import './p.css'", "import './b/x.css'", '', 'export const y = 1') },
  )
  assert.equal(realPure(cssMove, 'src/m.tsx'), true, 'side-effect import renamed in place')
})

test('real git: a moved importer rewrites its relative imports, re-sorted or not', (t) => {
  const files = realDiff(
    t,
    {
      'src/a/m.tsx': lines(
        "import { y } from './y'",
        "import { x } from './x'",
        "import './x.css'",
        ...FILLER,
      ),
    },
    [['src/a/m.tsx', 'src/a/deep/m.tsx']],
    {
      'src/a/deep/m.tsx': lines(
        "import { x } from '../x'",
        "import { y } from '../y'",
        "import '../x.css'",
        ...FILLER,
      ),
    },
  )
  assert.equal(files.length, 1)
  assert.equal(realPure(files, 'src/a/deep/m.tsx'), true)
  const swapped = realDiff(
    t,
    { 'src/a/m.ts': lines("import { p } from 'p'", "import { q } from 'q'", ...FILLER) },
    [['src/a/m.ts', 'src/b/m.ts']],
    { 'src/b/m.ts': lines("import { q } from 'q'", "import { p } from 'p'", ...FILLER) },
  )
  assert.equal(realPure(swapped, 'src/b/m.ts'), false, 'an unchanged import moved past another')
})

test('real git: JS, Go and Python package moves with import-only importers are pure', (t) => {
  const files = realDiff(
    t,
    {
      'src/widgets/Button.tsx': 'export const Button = () => null\n',
      'src/widgets/Card.tsx': 'export const Card = () => null\n',
      'src/app/Page.tsx': lines(
        "import { Button } from '../widgets/Button'",
        "import Card from '../widgets/Card.js';",
        '',
        'export const Page = () => [Button, Card]',
      ),
      'src/app/lazy.ts': lines("const m = await import('../widgets/Card')", 'm.Card()'),
      'src/app/multi.ts': lines('import {', '  Button,', "} from '../widgets/Button'", 'Button()'),
      'internal/old/store/store.go': 'package store\n',
      'internal/old/store/log/log.go': 'package log\n',
      'cmd/main.go': lines(
        'package main',
        '',
        'import (',
        '\t"fmt"',
        '',
        '\t"example.com/m/internal/old/store"',
        '\tlog "example.com/m/internal/old/store/log"',
        ')',
      ),
      'cmd/single.go': lines('package main', '', 'import "example.com/m/internal/old/store"'),
      'app/utils/text.py': 'def slug(s):\n    return s\n',
      'app/main.py': lines(
        'from app.utils.text import slug',
        'import app.utils.text as t',
        '',
        'print(slug(t))',
      ),
    },
    [
      ['src/widgets/Button.tsx', 'src/ui/Button.tsx'],
      ['src/widgets/Card.tsx', 'src/ui/Card.tsx'],
      ['internal/old/store/store.go', 'internal/data/store/store.go'],
      ['internal/old/store/log/log.go', 'internal/data/store/log/log.go'],
      ['app/utils/text.py', 'app/common/text.py'],
    ],
    {
      'src/app/Page.tsx': lines(
        "import { Button } from '../ui/Button'",
        "import Card from '../ui/Card.js';",
        '',
        'export const Page = () => [Button, Card]',
      ),
      'src/app/lazy.ts': lines("const m = await import('../ui/Card')", 'm.Card()'),
      'src/app/multi.ts': lines('import {', '  Button,', "} from '../ui/Button'", 'Button()'),
      'cmd/main.go': lines(
        'package main',
        '',
        'import (',
        '\t"fmt"',
        '',
        '\tlog "example.com/m/internal/data/store/log"',
        '',
        '\t"example.com/m/internal/data/store"',
        ')',
      ),
      'cmd/single.go': lines('package main', '', 'import "example.com/m/internal/data/store"'),
      'app/main.py': lines(
        'from app.common.text import slug',
        'import app.common.text as t',
        '',
        'print(slug(t))',
      ),
    },
  )
  assert.equal(files.length, 11)
  for (const file of files) assert.equal(pure(file, files), true, file.path)
  assert.deepEqual(pureMoveSummary(files), { pure: true, moved: 5, importOnly: 6, other: 0 })
})

test('real git: blank changed lines never make an unbalanced hunk pure', (t) => {
  const files = realDiff(
    t,
    {
      'src/a/x.ts': 'export const x = 1\n',
      'src/m.ts': lines("import './polyfill'", "import { x } from './a/x'", 'x()'),
    },
    [['src/a/x.ts', 'src/b/x.ts']],
    { 'src/m.ts': lines("import { x } from './b/x'", '', "import './polyfill'", '', 'x()') },
  )
  assert.equal(realPure(files, 'src/m.ts'), false)
})

test('real git: rewritten imports that swap a binding past a side-effect import are not pure', (t) => {
  const files = realDiff(
    t,
    {
      'src/a/x.css': '.x { color: red }\n',
      'src/a/y.ts': 'export const y = 1\n',
      'src/m.ts': lines("import './a/x.css'", "import { y } from './a/y'", '', 'y()'),
    },
    [
      ['src/a/x.css', 'src/b/x.css'],
      ['src/a/y.ts', 'src/b/y.ts'],
    ],
    { 'src/m.ts': lines("import { y } from './b/y'", "import './b/x.css'", '', 'y()') },
  )
  assert.equal(realPure(files, 'src/m.ts'), false)
})
