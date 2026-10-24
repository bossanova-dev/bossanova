import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fixture, cli } from './docs-guard-fixture.mjs'

test('docs-layout rejects a seeded directory with the registered command', (t) => {
  const root = fixture(t, { 'docs/scratch/new.md': '' })
  const result = cli('docs-layout', root)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /scripts\/layout\/docs-allowlist\.json/)
})
test('docs-layout rejects stale references and stale legacy entries', (t) => {
  const root = fixture(t, {
    'docs/guides/a.md': '`docs/retired.md`\n',
    'scripts/layout/docs-moves.json': '[["docs/retired.md", "docs/guides/a.md"]]\n',
    'scripts/guards/docs-layout-baseline.json': '["docs/old.md — BOS-1764 legacy"]\n',
  })
  const result = cli('docs-layout', root)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /moved to/)
  assert.match(result.stderr, /no longer occurs/)
})
test('docs-layout accepts allowlisted absent directories, retained legacy and history exceptions', (t) => {
  const root = fixture(t, {
    'docs/old.md': '',
    'docs/guides/a.md':
      '<!-- guard-exception(docs-layout): history; expires 2030-01-01 -->\n`docs/retired.md`\n`prefix/docs/retired.md`\n`docs/retired.md-extra`\n',
    'scripts/layout/docs-allowlist.json': '["docs/guides — guides", "docs/sweeps — future"]\n',
    'scripts/layout/docs-moves.json': '[["docs/retired.md", "docs/guides/a.md"]]\n',
    'scripts/guards/docs-layout-baseline.json': '["docs/old.md — BOS-1764 legacy"]\n',
  })
  const result = cli('docs-layout', root)
  assert.equal(result.status, 0, result.stderr)
  fs.writeFileSync(path.join(root, 'scripts/layout/docs-moves.json'), '{}')
  assert.equal(cli('docs-layout', root).status, 1)
})
