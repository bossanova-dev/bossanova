import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const docPath = path.join(repoRoot, 'docs/testing/frontend-lint-gates.md')

test('frontend lint gate file:line references resolve to real code', () => {
  const doc = fs.readFileSync(docPath, 'utf8')
  const refs = [...doc.matchAll(/`([^`\s]+):(\d+)`/g)].map((match) => ({
    path: match[1],
    line: Number(match[2]),
  }))

  assert.ok(refs.length > 0, 'expected at least one file:line reference')

  for (const ref of refs) {
    const target = path.join(repoRoot, ref.path)
    assert.ok(fs.existsSync(target), `${ref.path} should exist`)

    const lines = fs.readFileSync(target, 'utf8').split('\n')
    assert.ok(ref.line >= 1, `${ref.path}:${ref.line} should use a one-based line number`)
    assert.ok(
      ref.line <= lines.length,
      `${ref.path}:${ref.line} should resolve within ${lines.length} lines`,
    )
    assert.notEqual(lines[ref.line - 1].trim(), '', `${ref.path}:${ref.line} should not be blank`)
  }
})
