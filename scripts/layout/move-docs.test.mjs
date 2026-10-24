import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { moveDocs } from './move-docs.mjs'

function fixture(t, files = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'move-docs-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const contents = {
    'scripts/layout/docs-moves.json': '[]\n',
    'scripts/layout/docs-allowlist.json': '["docs/structure — contracts"]\n',
    'docs/old.md': '[source](../lib/source.go#part)\n',
    'lib/source.go': 'package lib\n',
    'docs/other.md': '[old](old.md#part)\n',
    'README.md': '`docs/old.md` docs/old.md.backup https://host/docs/old.md\n',
    '.codex/skills/example/SKILL.md': 'docs/old.md\n',
    ...files,
  }
  for (const [file, body] of Object.entries(contents)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), body)
  }
  execFileSync('git', ['init', '-q', root])
  execFileSync('git', ['add', '.'], { cwd: root })
  return { root, read: (file) => fs.readFileSync(path.join(root, file), 'utf8') }
}
const moves = [['docs/old.md', 'docs/structure/contracts.md']]

test('moves docs and repairs absolute citations and relative links, recording an idempotent ledger', (t) => {
  const f = fixture(t)
  const result = moveDocs({ root: f.root, moves })
  assert.equal(fs.existsSync(path.join(f.root, 'docs/old.md')), false)
  assert.equal(f.read('docs/structure/contracts.md'), '[source](../../lib/source.go#part)\n')
  assert.equal(f.read('docs/other.md'), '[old](structure/contracts.md#part)\n')
  assert.equal(
    f.read('README.md'),
    '`docs/structure/contracts.md` docs/old.md.backup https://host/docs/old.md\n',
  )
  assert.deepEqual(JSON.parse(f.read('scripts/layout/docs-moves.json')), moves)
  assert.equal(f.read('.codex/skills/example/SKILL.md'), 'docs/old.md\n')
  assert.deepEqual(result.generators, ['make codex-skills'])
  assert.deepEqual(moveDocs({ root: f.root, moves }).moves, [])
})

test('dry-run leaves the files and index unchanged', (t) => {
  const f = fixture(t)
  const before = execFileSync('git', ['ls-files', '--stage'], { cwd: f.root }).toString()
  const result = moveDocs({ root: f.root, moves, dryRun: true })
  assert.deepEqual(result.moves, moves)
  assert.equal(f.read('scripts/layout/docs-moves.json'), '[]\n')
  assert.equal(f.read('docs/old.md'), '[source](../lib/source.go#part)\n')
  assert.equal(execFileSync('git', ['ls-files', '--stage'], { cwd: f.root }).toString(), before)
})

test('rejects untracked sources, taken destinations and disallowed destination directories before moving', (t) => {
  const f = fixture(t)
  for (const pair of [
    ['docs/missing.md', 'docs/structure/new.md'],
    ['docs/old.md', 'docs/other.md'],
    ['docs/old.md', 'docs/scratch/new.md'],
  ])
    assert.throws(() => moveDocs({ root: f.root, moves: [pair] }))
  assert.equal(f.read('docs/old.md'), '[source](../lib/source.go#part)\n')
})
