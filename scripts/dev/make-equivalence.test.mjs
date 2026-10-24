import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const tool = fileURLToPath(new URL('./make-equivalence.mjs', import.meta.url))
function fixture(t, monolith, a, b = '', root = 'include mk/a.mk mk/b.mk\n') {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'make-equivalence-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const git = (...args) => {
    const r = spawnSync('git', args, { cwd: dir, encoding: 'utf8' })
    assert.equal(r.status, 0, r.stderr)
    return r.stdout.trim()
  }
  git('init', '-q')
  git('config', 'user.name', 'Fixture')
  git('config', 'user.email', 'fixture@example.com')
  fs.writeFileSync(path.join(dir, 'Makefile'), monolith)
  git('add', 'Makefile')
  git('-c', 'commit.gpgsign=false', 'commit', '-qm', 'monolith')
  const base = git('rev-parse', 'HEAD')
  fs.mkdirSync(path.join(dir, 'mk'))
  fs.writeFileSync(path.join(dir, 'Makefile'), root)
  fs.writeFileSync(path.join(dir, 'mk/a.mk'), a)
  fs.writeFileSync(path.join(dir, 'mk/b.mk'), b)
  git('add', 'Makefile', 'mk')
  git('-c', 'commit.gpgsign=false', 'commit', '-qm', 'split')
  const result = spawnSync(process.execPath, [tool, '--base', base, '--head', 'HEAD', '--json'], {
    cwd: dir,
    encoding: 'utf8',
  })
  assert.ok(result.stdout, result.stderr)
  return { result, report: JSON.parse(result.stdout), dir, git }
}
const a = '.PHONY: all other\nall: other\n\t@echo hello\n'
const b = 'other:\n\t@echo world\n'
test('pure split preserves the database and dry runs in both configurations', (t) => {
  const { result, report } = fixture(t, a + b, a, b)
  assert.equal(result.status, 0, JSON.stringify(report))
  assert.deepEqual(report.targets, ['all', 'other'])
  assert.equal(report.configurations.length, 2)
})
for (const [name, before, afterA, afterB, root, target] of [
  ['one-character recipe change', a + b, a.replace('hello', 'hallo'), b, undefined, 'all'],
  ['dropped target', a + b, a, '', undefined, 'other'],
  [
    'immediate variable moves after its rule',
    'NAME := chosen\n$(NAME):\n\t@echo yes\n',
    '$(NAME):\n\t@echo yes\n',
    '',
    'include mk/a.mk mk/b.mk\nNAME := chosen\n',
    'chosen',
  ],
  ['duplicate target recipes', a + b, a + b, b, undefined, 'other'],
]) {
  test(name + ' fails and names the affected target', (t) => {
    const { result, report } = fixture(t, before, afterA, afterB, root)
    assert.equal(result.status, 1, JSON.stringify(report))
    assert.match(JSON.stringify(report), new RegExp(target))
  })
}
test('recursive recipes and their dependents are reported without executing a sentinel', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'make-sentinel-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const sentinel = path.join(dir, 'executed')
  const recursive = `all: recursive\nrecursive:\n\t$(MAKE) -f /dev/null missing; touch ${sentinel}\n`
  const { result, report } = fixture(t, recursive, recursive)
  assert.equal(result.status, 0, JSON.stringify(report))
  assert.ok(
    report.configurations.every(
      (c) => c.databaseOnlyTargets.includes('recursive') && c.databaseOnlyTargets.includes('all'),
    ),
  )
  assert.equal(fs.existsSync(sentinel), false)
})

test('only the exact test-affected smoke-reader operand may follow the include list', (t) => {
  const before =
    "test-affected:\n\t@if grep -Eq '^test-smoke:' Makefile; then $(MAKE) test-smoke; fi\ntest-smoke:\n\t@echo smoke\n"
  const after = before.replace(
    "grep -Eq '^test-smoke:' Makefile",
    "grep -Eq '^test-smoke:' $(MAKEFILE_LIST)",
  )
  const good = fixture(t, before, after)
  assert.equal(good.result.status, 0, JSON.stringify(good.report))
  const bad = fixture(t, before, after.replace('echo smoke', 'echo broken'))
  assert.equal(bad.result.status, 1)
  assert.match(JSON.stringify(bad.report), /test-smoke/)
})

test('ordinary dotfile targets stay in the manifest with their defining fragments', (t) => {
  const text =
    'BIN := bin\nSTAMP := .stamp\nall: $(STAMP) bin/tool\n$(STAMP):\n\t@echo stamp\n$(BIN)/tool:\n\t@echo tool\n'
  const { result, report } = fixture(t, text, text)
  assert.equal(result.status, 0, JSON.stringify(report))
  assert.ok(report.targets.includes('.stamp'))
  assert.equal(report.configurations[0].definingFiles['.stamp'], 'mk/a.mk')
  assert.equal(report.configurations[0].definingFiles['bin/tool'], 'mk/a.mk')
})

test('plus-prefixed recipes are database-only without executing a sentinel', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'make-plus-sentinel-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const sentinel = path.join(dir, 'executed')
  const recipe = `all: forced\nforced:\n\t+touch ${sentinel}\n`
  const { result, report } = fixture(t, recipe, recipe)
  assert.equal(result.status, 0, JSON.stringify(report))
  assert.ok(report.configurations.every((c) => c.databaseOnlyTargets.includes('forced')))
  assert.equal(fs.existsSync(sentinel), false)
})
