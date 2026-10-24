import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fixture, cli } from './docs-guard-fixture.mjs'

test('docs-paths rejects broken citations with the registered command and passes once baselined', (t) => {
  const root = fixture(t, {
    'services/a.go': '',
    'docs/guides/a.md': '`services/missing.go` [broken](missing.md)\n',
  })
  const red = cli('docs-paths', root)
  assert.equal(red.status, 1)
  assert.match(red.stderr, /services\/missing.go/)
  assert.match(red.stderr, /missing.md/)
  assert.equal(cli('docs-paths', root, ['--write-baseline']).status, 0)
  assert.equal(cli('docs-paths', root).status, 0)
  fs.writeFileSync(path.join(root, 'docs/guides/a.md'), '')
  assert.equal(cli('docs-paths', root).status, 1)
})
test('docs-paths resolves directories relative links ignored paths and line suffixes', (t) => {
  const root = fixture(t, {
    '.gitignore': 'services/ignored/\n',
    'services/a.go': '',
    'docs/guides/a.md': [
      '`services/a.go:2-3#anchor` `services/` [file](../../services/a.go#anchor) [root](/services/a.go)',
      '`services/ignored/a.go` `services/<name>/a.go` `services/*.go` `services/$name.go`',
      '`services/…/a.go` [anchor](#missing) [remote](https://example.com/nope)',
      '```go',
      '`services/missing.go` [bad](gone.md)',
      '```',
      '~~~',
      '`services/missing.go`',
      '~~~',
      '<!-- guard-exception(docs-paths): history; expires 2030-01-01 -->',
      '`services/retired.go`',
    ].join('\n'),
  })
  const result = cli('docs-paths', root)
  assert.equal(result.status, 0, result.stderr)
})
test('docs-paths scans nested AGENTS files and skips symlinks', (t) => {
  const root = fixture(t, {
    'services/a.go': '',
    'services/AGENTS.md': '`services/missing.go`',
    'docs/guides/a.md': '',
  })
  assert.equal(cli('docs-paths', root).status, 1)
  fs.unlinkSync(path.join(root, 'services/AGENTS.md'))
  fs.symlinkSync('../docs/guides/a.md', path.join(root, 'services/AGENTS.md'))
  assert.equal(cli('docs-paths', root).status, 0)
})

test('docs-paths accepts ignored directory citations before dependencies are installed', (t) => {
  const root = fixture(t, {
    '.gitignore': 'node_modules/\n',
    'services/docs/package.json': '{}',
    'docs/guides/a.md':
      '`services/docs/node_modules` [dependencies](../../services/docs/node_modules)\n',
  })
  assert.equal(fs.existsSync(path.join(root, 'services/docs/node_modules')), false)
  const result = cli('docs-paths', root)
  assert.equal(result.status, 0, result.stderr)
})
