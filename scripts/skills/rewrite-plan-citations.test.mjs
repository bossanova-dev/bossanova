import assert from 'node:assert/strict'
import { test } from 'node:test'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import { rewriteCitations, exclusionFor } from './rewrite-plan-citations.mjs'

for (const [name, source, expected] of [
  ['Go comment with id', '// See `docs/plans/BOS-123-example.md:12-14`.', '// See BOS-123.'],
  ['comment-only parenthetical', '// (docs/plans/old.md). It is a sibling.', '// It is a sibling.'],
  ['lowercase id', 'docs/plans/2026-07-31-bos-600-example.md', 'BOS-600'],
  ['glob and relative prefix', '../../docs/plans/BOS-27-*.md', 'BOS-27'],
  ['absolute prefix', '/docs/plans/BOS-28-example.md', 'BOS-28'],
  ['parenthetical', 'Text (see `docs/plans/2026-example.md`).', 'Text.'],
  ['parenthetical prefix', 'Text (Spec lives in docs/plans/old.md).', 'Text.'],
  ['parenthetical remainder', 'Text (docs/plans/old.md, decision #14).', 'Text (decision #14).'],
  ['standalone sentence', '// See docs/plans/old.md for the design.', '// '],
  ['prepositional phrase', 'decision D6 in docs/plans/old.md, the rule', 'decision D6, the rule'],
  ['markdown bullet', '- `docs/plans/old.md` — design\nNext\n', 'Next\n'],
  [
    'YAML module list',
    '---\nmodule: boss, docs/plans, web\n---\n',
    '---\nmodule: boss, web\n---\n',
  ],
  [
    'wrapped residue',
    '// see docs/plans/old.md,\n// decision #2',
    '// see docs/plans/old.md,\n// decision #2',
  ],
  [
    'free prose residue',
    'Built from docs/plans/old.md yesterday.',
    'Built from docs/plans/old.md yesterday.',
  ],
]) {
  test(name, () => {
    const actual = rewriteCitations(source)
    assert.equal(actual, expected)
    assert.equal(rewriteCitations(actual), actual, 'rewriting is idempotent')
  })
}

test('generated and intentional paths are excluded, live source is included', () => {
  for (const file of [
    'lib/bossalib/gen/x.go',
    'services/web/src/gen/x.ts',
    'services/docs/openapi/x.yaml',
    '.codex/skills/x.md',
    'lib/bossalib/contract/plugin/skillpayload/skills/x.md',
  ])
    assert.equal(exclusionFor(file).kind, 'GENERATED')
  for (const file of [
    'experiments/x.md',
    'services/bossd/internal/db/db_test.go',
    'scripts/skills/rewrite-plan-citations.test.mjs',
  ])
    assert.equal(exclusionFor(file).kind, 'INTENTIONAL')
  assert.equal(exclusionFor('services/bosso/main.go'), null)
  assert.equal(exclusionFor('docs/plans/old.md').kind, 'DELETED')
})

test('--check reports unresolved references, --write is idempotent and ignored files remain', () => {
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), 'plan-citations-'))
  const cli = new URL('./rewrite-plan-citations.mjs', import.meta.url).pathname
  try {
    execFileSync('git', ['init', '-q', repo])
    fs.writeFileSync(path.join(repo, 'live.md'), 'See docs/plans/BOS-123-example.md.\n')
    fs.mkdirSync(path.join(repo, 'experiments'))
    fs.writeFileSync(path.join(repo, 'experiments', 'notes.md'), 'docs/plans/opaque.md\n')
    execFileSync('git', ['-C', repo, 'add', 'live.md', 'experiments/notes.md'])
    const run = (...args) =>
      spawnSync(process.execPath, [cli, ...args], { cwd: repo, encoding: 'utf8' })
    assert.equal(run('--check').status, 1)
    assert.equal(run().status, 0)
    assert.match(fs.readFileSync(path.join(repo, 'live.md'), 'utf8'), /docs\/plans/)
    assert.equal(run('--write').status, 0)
    const first = fs.readFileSync(path.join(repo, 'live.md'), 'utf8')
    assert.equal(first, 'See BOS-123.\n')
    assert.equal(run('--write').status, 0)
    assert.equal(fs.readFileSync(path.join(repo, 'live.md'), 'utf8'), first)
    assert.equal(run('--check').status, 0)
    assert.match(run('--check').stdout, /INTENTIONAL experiments\/notes.md/)
    fs.writeFileSync(path.join(repo, 'live.md'), 'Unresolved docs/plans/old.md here.\n')
    const result = run('--check')
    assert.equal(result.status, 1)
    assert.match(result.stderr, /live.md:1/)
  } finally {
    fs.rmSync(repo, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})
