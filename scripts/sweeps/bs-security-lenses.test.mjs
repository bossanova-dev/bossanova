import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const ROOT = fileURLToPath(new URL('../../', import.meta.url))
const SCRIPT = fileURLToPath(new URL('./bs-security-lenses.mjs', import.meta.url))
const LENSES = path.join(ROOT, '.claude/skills/bs-security/references/lenses.json')

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-security-lenses-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const out = path.join(dir, 'selected.json')
  return {
    out,
    select: (...args) =>
      spawnSync(process.execPath, [SCRIPT, 'select', '--lenses', LENSES, '--out', out, ...args], {
        encoding: 'utf8',
      }),
  }
}

test('real lens selection preserves entries and families, including repeated --lens', (t) => {
  const f = fixture(t)
  const all = JSON.parse(fs.readFileSync(LENSES, 'utf8')).lenses
  const ai = all.find((lens) => lens.id === 'ai-agent')
  assert.ok(ai)
  assert.equal(ai.family, 'ai-agent')
  let result = f.select('--lens', 'ai-agent')
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(fs.readFileSync(f.out, 'utf8')), [ai])
  result = f.select('--lens', 'ai-agent', '--lens', all[0].id)
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(fs.readFileSync(f.out, 'utf8')), [all[0], ai])
  result = f.select()
  assert.equal(result.status, 0, result.stderr)
  assert.equal(all.length, 8)
  assert.deepEqual(JSON.parse(fs.readFileSync(f.out, 'utf8')), all)
})

test('unknown lens IDs and strict CLI usage fail before writing output', (t) => {
  const f = fixture(t)
  const unknown = f.select('--lens', 'missing-lens')
  assert.equal(unknown.status, 2, unknown.stderr)
  assert.match(unknown.stderr, /missing-lens/)
  assert.equal(fs.existsSync(f.out), false)
  for (const args of [['--unknown'], ['extra'], ['--lens']]) {
    const result = f.select(...args)
    assert.equal(result.status, 2, result.stderr)
    assert.equal(fs.existsSync(f.out), false)
  }
})

test('each configured AI-agent glob matches tracked repository files', () => {
  const ai = JSON.parse(fs.readFileSync(LENSES, 'utf8')).lenses.find(
    (lens) => lens.id === 'ai-agent',
  )
  assert.ok(ai)
  assert.ok(ai.globs.length > 0)
  for (const glob of ai.globs) {
    const result = spawnSync('/usr/bin/git', ['ls-files', `:(glob)${glob}`], {
      cwd: ROOT,
      encoding: 'utf8',
    })
    assert.equal(result.status, 0, result.stderr)
    assert.ok(result.stdout.trim(), `no tracked files match ${glob}`)
  }
})
