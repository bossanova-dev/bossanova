import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { docsIndex } from './docs-index.mjs'

function fixture(t) {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'docs-index-'))
  t.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 5 }))
  const files = {
    'AGENTS.md':
      '# Index\n<!-- BEGIN GENERATED docs-index -->\nstale\n<!-- END GENERATED docs-index -->\n',
    'docs/a.md': '---\nsummary: A reference.\n---\n# A\n',
    'docs/group/README.md': '---\nsummary: Group reference.\n---\n# Group\n',
    'docs/group/nested/hidden.md': '# Hidden without summary\n',
    'docs/solutions/a.md': '# Historical entry\n',
  }
  for (const [file, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(repoRoot, file)), { recursive: true })
    fs.writeFileSync(path.join(repoRoot, file), text)
  }
  execFileSync('git', ['init', '-q'], { cwd: repoRoot })
  execFileSync('git', ['add', '--', ...Object.keys(files)], { cwd: repoRoot })
  return repoRoot
}

test('docs index collapses README directories and solutions and ignores untracked docs', (t) => {
  const repoRoot = fixture(t)
  fs.writeFileSync(path.join(repoRoot, 'docs/scratch.md'), '# No summary\n')
  assert.equal(docsIndex({ repoRoot, mode: 'write' }).ok, true)
  const text = fs.readFileSync(path.join(repoRoot, 'AGENTS.md'), 'utf8')
  assert.ok(text.includes('[docs/a.md](docs/a.md) — A reference.'))
  assert.ok(text.includes('[docs/group/README.md](docs/group/README.md)'))
  assert.ok(text.includes('[docs/solutions/](docs/solutions/)'))
  assert.ok(!text.includes('hidden.md'))
  assert.ok(!text.includes('scratch.md'))
  assert.equal(docsIndex({ repoRoot, mode: 'check' }).ok, true)
  assert.equal(docsIndex({ repoRoot, mode: 'write' }).ok, true)
  assert.equal(fs.readFileSync(path.join(repoRoot, 'AGENTS.md'), 'utf8'), text)
})

test('docs-index check fails after a hand edit in the generated region', (t) => {
  const repoRoot = fixture(t)
  assert.equal(docsIndex({ repoRoot, mode: 'write' }).ok, true)
  const file = path.join(repoRoot, 'AGENTS.md')
  fs.writeFileSync(file, fs.readFileSync(file, 'utf8').replace('A reference.', 'Hand edit.'))
  const result = docsIndex({ repoRoot, mode: 'check' })
  assert.equal(result.ok, false)
  assert.match(result.messages.join('\n'), /Run: make docs-index-update/)
  assert.match(result.messages.join('\n'), /-.*Hand edit/)
})

for (const frontmatter of [
  '',
  '---\nsummary: \n---\n',
  '---\nsummary: |\n  Multiline\n---\n',
  '---\nsummary: Starts\n  continued\n---\n',
]) {
  test(`docs-index rejects missing or non-scalar summary ${JSON.stringify(frontmatter)}`, (t) => {
    const repoRoot = fixture(t)
    fs.writeFileSync(path.join(repoRoot, 'docs/a.md'), `${frontmatter}# A\n`)
    for (const mode of ['write', 'check']) {
      const result = docsIndex({ repoRoot, mode })
      assert.equal(result.ok, false)
      assert.match(result.messages.join('\n'), /docs\/a.md.*summary/)
    }
  })
}

test('docs index rejects missing duplicate and reversed markers', (t) => {
  const repoRoot = fixture(t)
  for (const text of [
    '# No markers\n',
    '<!-- BEGIN GENERATED docs-index -->\n<!-- BEGIN GENERATED docs-index -->\n<!-- END GENERATED docs-index -->',
    '<!-- END GENERATED docs-index -->\n<!-- BEGIN GENERATED docs-index -->',
  ]) {
    fs.writeFileSync(path.join(repoRoot, 'AGENTS.md'), text)
    assert.equal(docsIndex({ repoRoot, mode: 'write' }).ok, false)
  }
})
