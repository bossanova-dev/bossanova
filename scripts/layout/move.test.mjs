import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { applyMoves } from './move.mjs'
import { readBaseline } from '../guards/baseline.mjs'

function fixture(t, files) {
  const root = mkdtempSync(path.join(tmpdir(), 'script-move-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  for (const [file, body] of Object.entries({ 'scripts/layout/moves.json': '[]\n', ...files })) {
    mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    writeFileSync(path.join(root, file), body)
  }
  execFileSync('git', ['init', '-q', root])
  execFileSync('git', ['add', '.'], { cwd: root })
  return { root, read: (file) => readFileSync(path.join(root, file), 'utf8') }
}
const moves = [['scripts/task.mjs', 'scripts/dev/task.mjs']]

test('rewritten baseline entries remain valid when moving a path changes their sort order', (t) => {
  const f = fixture(t, {
    'scripts/task.mjs': 'export {}\n',
    'scripts/count-baseline.json': JSON.stringify({
      'scripts/task.mjs': 2,
      'scripts/zebra.mjs': 1,
    }),
    'scripts/other-baseline.json': JSON.stringify([
      'scripts/task.mjs — legacy exception',
      'scripts/zebra.mjs',
    ]),
  })
  applyMoves({ root: f.root, moves: [['scripts/task.mjs', 'scripts/zz/task.mjs']] })
  assert.deepEqual(Object.keys(JSON.parse(f.read('scripts/count-baseline.json'))), [
    'scripts/zebra.mjs',
    'scripts/zz/task.mjs',
  ])
  const baseline = readBaseline(f.root, 'scripts/other-baseline.json')
  assert.deepEqual(baseline.errors, [])
  assert.deepEqual(baseline.entries, [
    'scripts/zebra.mjs',
    'scripts/zz/task.mjs — legacy exception',
  ])
})

test('rewrites imports in moved files and importers outside the moved set, including exports and dynamic imports', (t) => {
  const f = fixture(t, {
    'scripts/task.mjs': "import './helper.mjs'\nexport const x = 1\n",
    'scripts/helper.mjs': 'export {}\n',
    'scripts/user.mjs':
      "import './task.mjs'\nexport { x } from './task.mjs'\nawait import('./task.mjs')\n",
    'scripts/task.test.mjs': "import './task.mjs'\n",
    'scripts/task-baseline.json': '{}\n',
  })
  applyMoves({ root: f.root, moves })
  assert.match(f.read('scripts/dev/task.mjs'), /import '\.\.\/helper.mjs'/)
  assert.equal(
    f.read('scripts/user.mjs'),
    "import './dev/task.mjs'\nexport { x } from './dev/task.mjs'\nawait import('./dev/task.mjs')\n",
  )
  assert.equal(f.read('scripts/dev/task.test.mjs'), "import './task.mjs'\n")
  assert.ok(existsSync(path.join(f.root, 'scripts/dev/task-baseline.json')))
  assert.equal(JSON.parse(f.read('scripts/layout/moves.json')).length, 3)
})

test('preserves depth-relative path targets and URL paths after a move', (t) => {
  const f = fixture(t, {
    'scripts/task.mjs':
      "const root = path.resolve(import.meta.dirname, '..')\nconst other = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'docs')\nconst url = new URL('../docs/', import.meta.url)\nconst dir = path.dirname(fileURLToPath(import.meta.url))\nconst aliasRoot = path.resolve(dir, '..')\nconst self = fileURLToPath(import.meta.url)\nconst identity = import.meta.filename\nconst selfRoot = path.resolve(path.dirname(self), '..')\nconst source = fs.readFileSync(fileURLToPath(import.meta.url), 'utf8')\nif (isMainModule(import.meta.url)) {}\n",
  })
  applyMoves({ root: f.root, moves })
  const body = f.read('scripts/dev/task.mjs')
  assert.match(body, /resolve\(import.meta.dirname, '\.\.\/\.\.'\)/)
  assert.match(body, /fileURLToPath\(import.meta.url\)\), '\.\.\/\.\.\/docs'\)/)
  assert.match(body, /resolve\(dir, '\.\.\/\.\.'\)/)
  assert.match(body, /resolve\(path.dirname\(self\), '\.\.\/\.\.'\)/)
  assert.match(body, /const identity = import\.meta\.filename/)
  assert.match(body, /new URL\('\.\.\/\.\.\/docs\/', import.meta.url\)/)
})

test('unclassifiable import.meta path fails with file and line before writing anything', (t) => {
  const source = 'export {}\nconst root = path.resolve(import.meta.dirname, computed)\n'
  const f = fixture(t, { 'scripts/task.mjs': source })
  assert.throws(() => applyMoves({ root: f.root, moves }), /scripts\/task.mjs:2:.*unclassifiable/)
  assert.equal(f.read('scripts/task.mjs'), source)
  assert.equal(f.read('scripts/layout/moves.json'), '[]\n')
})

test('rewrites tokens in build, workflow, registry globs, baseline entries and budget keys', (t) => {
  const files = {
    'scripts/task.mjs': 'export {}\n',
    Makefile: 'run:\n\tnode scripts/task.mjs\n',
    '.github/workflows/test.yml': 'run: node scripts/task.mjs\n',
    'scripts/guards/registry/example.mjs': "export default { scans: ['scripts/task*.mjs'] }\n",
    'scripts/guards/example-baseline.json': '["scripts/task.mjs"]\n',
    'scripts/guards/budgets.json': '{"scripts/task.mjs": 1000}\n',
    'docs/usage.md':
      '`scripts/task.mjs` and scripts/task.mjs.backup and ./scripts/task.mjs and ../../../scripts/task.mjs.\n',
  }
  const f = fixture(t, files)
  applyMoves({ root: f.root, moves })
  for (const name of Object.keys(files).filter((name) => name !== 'scripts/task.mjs')) {
    assert.ok(
      f
        .read(name)
        .includes(name.includes('/registry/') ? 'scripts/dev/task*.mjs' : 'scripts/dev/task.mjs'),
      name,
    )
  }
  assert.ok(f.read('docs/usage.md').includes('scripts/task.mjs.backup'))
  assert.ok(f.read('docs/usage.md').includes('./scripts/dev/task.mjs'))
  assert.ok(f.read('docs/usage.md').includes('../../../scripts/dev/task.mjs.'))
})

test('dry run writes no files, moves no paths and leaves the git index unchanged', (t) => {
  const f = fixture(t, { 'scripts/task.mjs': 'export {}\n', Makefile: 'node scripts/task.mjs\n' })
  const before = execFileSync('git', ['ls-files', '--stage'], { cwd: f.root }).toString()
  const result = applyMoves({ root: f.root, moves, dryRun: true })
  assert.equal(result.moves.length, 1)
  assert.ok(existsSync(path.join(f.root, 'scripts/task.mjs')))
  assert.equal(f.read('Makefile'), 'node scripts/task.mjs\n')
  assert.equal(f.read('scripts/layout/moves.json'), '[]\n')
  assert.equal(execFileSync('git', ['ls-files', '--stage'], { cwd: f.root }).toString(), before)
})

test('rejects basename changes, destination collisions and fixture moves', (t) => {
  const f = fixture(t, { 'scripts/task.mjs': 'export {}\n', 'scripts/dev/task.mjs': 'other\n' })
  for (const mapping of [
    moves,
    [['scripts/task.mjs', 'scripts/dev/renamed.mjs']],
    [['scripts/task.mjs', 'scripts/fixtures/task.mjs']],
  ]) {
    assert.throws(() => applyMoves({ root: f.root, moves: mapping }))
  }
})

test('leaves generated mirrors and excluded fixtures untouched', (t) => {
  const f = fixture(t, {
    'scripts/task.mjs': 'export {}\n',
    '.codex/skills/example/SKILL.md': 'scripts/task.mjs\n',
    'lib/bossalib/contract/plugin/skillpayload/skills/boss-build/toolbox/task.mjs':
      '// scripts/task.mjs\n',
    'scripts/fixtures/source.mjs': '// scripts/task.mjs\n',
  })
  applyMoves({ root: f.root, moves })
  assert.equal(f.read('.codex/skills/example/SKILL.md'), 'scripts/task.mjs\n')
  assert.equal(f.read('scripts/fixtures/source.mjs'), '// scripts/task.mjs\n')
  assert.equal(
    f.read('lib/bossalib/contract/plugin/skillpayload/skills/boss-build/toolbox/task.mjs'),
    '// scripts/task.mjs\n',
  )
})

test('dynamic dirname aliases are refused while import.meta text in comments and strings is ignored', (t) => {
  const f = fixture(t, {
    'scripts/task.mjs':
      "// import.meta.dirname + dynamic\nconst text = 'import.meta.url'\nconst dirname = path.dirname(fileURLToPath(import.meta.url))\nconst root = path.resolve(dirname, computed)\n",
  })
  assert.throws(() => applyMoves({ root: f.root, moves }), /scripts\/task.mjs:4:.*unclassifiable/)
})

test('moves baselines owned through registry commands even when the check- prefix differs', (t) => {
  const f = fixture(t, {
    'scripts/check-task.mjs': 'export {}\n',
    'scripts/task-baseline.json': '[]\n',
    'scripts/guards/registry/task.mjs':
      "export default { command: 'node scripts/check-task.mjs --check', baseline: 'scripts/task-baseline.json' }\n",
  })
  applyMoves({ root: f.root, moves: [['scripts/check-task.mjs', 'scripts/guards/check-task.mjs']] })
  assert.ok(existsSync(path.join(f.root, 'scripts/guards/task-baseline.json')))
  assert.match(f.read('scripts/guards/registry/task.mjs'), /scripts\/guards\/task-baseline.json/)
})

test('rewrites published toolbox citations and preserves generated rules regions', (t) => {
  const f = fixture(t, {
    'skills-toolbox/task.mjs': 'export const answer = 42\n',
    'consumer.mjs':
      'const {answer} = await import(`${process.env.BOSS_PLAN_TOOLBOX}/task.mjs`); console.log(answer)\n',
    'scripts/skills/vendor-toolbox.mjs':
      "export const VENDOR_MAP = { 'boss-build': ['task.mjs'] }\n",
    '.claude/skills/example/SKILL.md':
      'node "$BOSS_BUILD_TOOLBOX/task.mjs"\nnode "${BOSS_PLAN_TOOLBOX}/task.mjs"\nnode "$HOME/.claude/skills/boss-plan/toolbox/task.mjs"\n',
    'CLAUDE.md':
      'scripts/task.mjs\n<!-- BEGIN GENERATED guard-registry: run make -->\nscripts/task.mjs\n<!-- END GENERATED guard-registry -->\n',
    'scripts/task.mjs': 'export {}\n',
  })
  applyMoves({
    root: f.root,
    moves: [...moves, ['skills-toolbox/task.mjs', 'skills-toolbox/build/task.mjs']],
  })
  assert.equal(
    f.read('.claude/skills/example/SKILL.md'),
    'node "$BOSS_BUILD_TOOLBOX/build/task.mjs"\nnode "${BOSS_PLAN_TOOLBOX}/build/task.mjs"\nnode "$HOME/.claude/skills/boss-plan/toolbox/build/task.mjs"\n',
  )
  assert.match(f.read('scripts/skills/vendor-toolbox.mjs'), /'build\/task.mjs'/)
  assert.equal(
    execFileSync(process.execPath, [path.join(f.root, 'consumer.mjs')], {
      env: { ...process.env, BOSS_PLAN_TOOLBOX: path.join(f.root, 'skills-toolbox') },
      encoding: 'utf8',
    }).trim(),
    '42',
  )
  assert.ok(f.read('CLAUDE.md').startsWith('scripts/dev/task.mjs\n'))
  assert.ok(f.read('CLAUDE.md').includes('\nscripts/task.mjs\n<!-- END'))
})

test('broad registry globs retain unmoved subjects and include moved subjects', (t) => {
  const f = fixture(t, {
    'scripts/task.mjs': 'export {}\n',
    'scripts/helper.mjs': 'export {}\n',
    'scripts/guards/registry/task.mjs': "export default { scans: ['scripts/*.mjs'] }\n",
    'scripts/guards/registry/recursive.mjs': "export default { scans: ['scripts/**'] }\n",
  })
  applyMoves({ root: f.root, moves })
  const record = f.read('scripts/guards/registry/task.mjs')
  assert.ok(record.includes('scripts/*.mjs'))
  assert.ok(record.includes('scripts/dev/task.mjs'))
  assert.equal(
    f.read('scripts/guards/registry/recursive.mjs'),
    "export default { scans: ['scripts/**'] }\n",
  )
})

test('moves explicit parity baseline snapshots without rewriting their captured contents', (t) => {
  const from = 'scripts/skill-parity/baseline/example.snapshot.json'
  const to = 'scripts/skills/skill-parity/baseline/example.snapshot.json'
  const body = JSON.stringify({ capturedPath: 'scripts/task.mjs' }) + '\n'
  const f = fixture(t, { [from]: body, 'scripts/task.mjs': 'export {}\n' })
  applyMoves({ root: f.root, moves: [...moves, [from, to]] })
  assert.equal(f.read(to), body)
  assert.equal(existsSync(path.join(f.root, from)), false)
  assert.ok(
    JSON.parse(f.read('scripts/layout/moves.json')).some(
      ([source, target]) => source === from && target === to,
    ),
  )
})
