import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import test from 'node:test'
import { checkWarehouseModelNames } from './warehouse-model-names.mjs'

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

function check(t, names, baseline = []) {
  return checkWarehouseModelNames({
    repoRoot: fixture(
      t,
      Object.fromEntries([
        ...names.map((name) => [`services/warehouse/models/${name}.sql`, 'select 1']),
        ['scripts/guards/warehouse-model-names-baseline.json', JSON.stringify(baseline)],
      ]),
    ),
  })
}
test('warehouse-model-names fails a seeded staging source mismatch', (t) => {
  assert.match(check(t, ['staging/bosso/stg_wrong__x']).failures.join('\n'), /stg_wrong__x/)
})
test('warehouse-model-names accepts layer conventions and root intermediate models', (t) => {
  assert.deepEqual(
    check(t, [
      'staging/bosso/stg_bosso__events',
      'intermediate/identity/int_identity__people',
      'intermediate/int_newsletter__subscription_signals',
      'marts/ga4/mart_ga4__events_daily',
      'marts/monetary/mart_monetary_receipts',
      'growth/measurement_health',
    ]).failures,
    [],
  )
})
test('warehouse-model-names rejects invalid layers prefixes and case', (t) => {
  assert.equal(
    check(t, [
      'other/x',
      'growth/stg_x__y',
      'marts/x/mart_y__z',
      'intermediate/x/int_y__z',
      'growth/Bad_name',
    ]).failures.length,
    5,
  )
})
test('warehouse-model-names keeps a shrinking baseline for the CI probe', (t) => {
  const file = 'services/warehouse/models/staging/ci/ci_probe_duplicate_grain.sql'
  assert.deepEqual(check(t, ['staging/ci/ci_probe_duplicate_grain'], [file]).failures, [])
  assert.match(check(t, ['growth/valid'], [file]).failures.join('\n'), /no longer occurs/)
})
