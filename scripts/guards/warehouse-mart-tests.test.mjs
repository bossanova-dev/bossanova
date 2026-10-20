import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import test from 'node:test'
import { checkWarehouseMartTests } from './warehouse-mart-tests.mjs'

function fixture(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'warehouse-guard-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  for (const [file, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '--', ...Object.keys(files)], { cwd: root })
  return root
}

const model = 'services/warehouse/models/marts/x/mart_x.sql'
test('warehouse-mart-tests fails a seeded mart without a dbt test', (t) => {
  const result = checkWarehouseMartTests({ repoRoot: fixture(t, { [model]: 'select 1' }) })
  assert.match(result.failures.join('\n'), /mart_x.*no dbt test/)
})
test('warehouse-mart-tests accepts model column unit and singular tests', (t) => {
  for (const yaml of [
    'models:\n  - name: mart_x\n    data_tests: [unique]\n',
    'models:\n  - name: mart_x\n    columns:\n      - name: id\n        tests: [unique]\n',
    'unit_tests:\n  - name: unit_x\n    model: mart_x\n',
  ]) {
    assert.deepEqual(
      checkWarehouseMartTests({
        repoRoot: fixture(t, { [model]: 'select 1', 'services/warehouse/models/tests.yml': yaml }),
      }).failures,
      [],
    )
  }
  assert.deepEqual(
    checkWarehouseMartTests({
      repoRoot: fixture(t, {
        [model]: 'select 1',
        'services/warehouse/data-tests/x.sql': "select * from {{ ref('mart_x') }}",
      }),
    }).failures,
    [],
  )
})
test('warehouse-mart-tests includes growth but excludes intermediate and generic definitions', (t) => {
  const result = checkWarehouseMartTests({
    repoRoot: fixture(t, {
      'services/warehouse/models/growth/x.sql': 'select 1',
      'services/warehouse/models/intermediate/int_x__y.sql': 'select 1',
      'services/warehouse/data-tests/generic/x.sql': "{{ ref('x') }}",
      'services/warehouse/models/empty.yml': 'models:\n  - name: x\n    tests: []\n',
    }),
  })
  assert.equal(result.failures.length, 1)
  assert.match(result.failures[0], /growth\/x.sql/)
})
