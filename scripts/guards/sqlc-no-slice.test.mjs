import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import test from 'node:test'
import { checkSqlcNoSlice } from './sqlc-no-slice.mjs'

test('sqlc-no-slice rejects seeded slice and names ANY replacement', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'sqlc-no-slice-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const file = path.join(root, 'services/bosso/internal/db/queries/x.sql')
  fs.mkdirSync(path.dirname(file), { recursive: true })
  fs.writeFileSync(
    file,
    "SELECT id FROM x WHERE id IN (SQLC.SLICE('ids')); -- sqlc.slice('comment')\n",
  )
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  const result = checkSqlcNoSlice({ repoRoot: root })
  assert.equal(result.failures.length, 1)
  assert.match(result.failures[0], /= ANY\(/)
  fs.writeFileSync(file, "-- sqlc.slice('comment')\nSELECT '-- sqlc.slice(''ids'')' AS text;\n")
  assert.deepEqual(checkSqlcNoSlice({ repoRoot: root }).failures, [])
  assert.deepEqual(checkSqlcNoSlice().failures, [])
})
