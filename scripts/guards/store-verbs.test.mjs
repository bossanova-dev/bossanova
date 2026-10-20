import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import test from 'node:test'
import { checkStoreVerbs } from './store-verbs.mjs'

function fixture(t, files, baseline = []) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'store-verbs-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  for (const [file, source] of Object.entries({
    ...files,
    'baseline.json': JSON.stringify(baseline),
  })) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), source)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  return root
}
const go = 'services/bossd/internal/db/x.go'
const sql = 'services/bosso/internal/db/queries/x.sql'
const check = (repoRoot) => checkStoreVerbs({ repoRoot, baselinePath: 'baseline.json' })
test('store-verbs rejects seeded FindRepo and InsertRepo and names allowed replacements', (t) => {
  const root = fixture(t, {
    [go]: 'package db\nfunc (s *SQLiteRepoStore) FindRepo() {}',
    [sql]: '-- name: InsertRepo :exec\nSELECT 1;',
  })
  assert.match(check(root).failures.join('\n'), /FindRepo.*Get/)
  assert.match(check(root).failures.join('\n'), /InsertRepo.*Create/)
})
test('store-verbs detects CamelCase entity repeats and permits Unscoped and unrelated words', (t) => {
  const root = fixture(t, {
    [go]: 'package db\nfunc (s SQLiteRepoStore) CreateRepo() {}\nfunc (s *SQLSessionWebhookStore) CreateSessionWebhook() {}\nfunc (s *SQLiteAccountStore) MarkAccountSuspended() {}\nfunc (s *SQLiteSessionStore) ListTmuxSessionNames() {}\nfunc (s *SQLiteRepoStore) ListUnscoped() {}\nfunc (s *SQLiteRepoStore) GetRepository() {}\nfunc (s *localStore) FindRepo() {}\nfunc FindRepo() {}',
  })
  assert.equal(check(root).failures.length, 4)
})
test('store-verbs enforces uppercase verb boundaries and excludes tests and literal declarations', (t) => {
  const root = fixture(t, {
    [go]: 'package db\n/*\nfunc (s *SQLiteRepoStore) FindFake() {}\n*/\nvar x = `\nfunc (s *SQLiteRepoStore) FindRaw() {}\n`\nfunc (s *SQLiteRepoStore) Getter() {}\nfunc (s *SQLiteRepoStore) Update() {}',
    [go.replace('.go', '_test.go')]: 'package db\nfunc (s *SQLiteRepoStore) FindTest() {}',
  })
  assert.equal(check(root).failures.length, 1)
  assert.match(check(root).failures[0], /Getter/)
})
test('store-verbs baseline shrinks and real tree passes', (t) => {
  const root = fixture(t, { [go]: 'package db' }, [
    'services/bossd/internal/db:SQLiteRepoStore.Archive',
  ])
  assert.match(check(root).failures.join('\n'), /no longer occurs/)
  assert.deepEqual(checkStoreVerbs().failures, [])
})
