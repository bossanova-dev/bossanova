// A reviewed non-UI area (such as Migrations) whose section shows its diff or excerpt on the page
// must count as shown in the banner rather than landing under Not proven.
import test from 'node:test'
import assert from 'node:assert/strict'
import { buildProofRecord } from './verify-proof-build.mjs'
import { DIFF_SHOWN } from './verify-proof-page-model.mjs'
import { proofPageModel, renderProofHtml } from './verify-proof-page.mjs'

const HEAD = 'c'.repeat(40)
const BASE = 'd'.repeat(40)
const MIGRATIONS = {
  area: 'migrations',
  label: 'Migrations',
  paths: ['services/api/internal/migration/sql/0310_documents.sql'],
  defaultAsk: 'always',
  source: 'builtin',
}

function record() {
  const built = buildProofRecord({
    judged: {
      verdict: 'human',
      reason: 'human-review:migrations',
      headSha: HEAD,
      baseSha: BASE,
      confidence: {
        level: 'moderate',
        asks: [{ area: 'migrations', label: 'Migrations', source: 'builtin' }],
        loweredBy: [],
      },
      facts: { areas: [MIGRATIONS] },
    },
    repo: 'acme/shop',
    pr: 9,
  })
  assert.equal(built.ok, true, String(built.errors))
  return built.proof
}

const MIGRATION_DIFF = [
  'diff --git a/services/api/internal/migration/sql/0310_documents.sql b/services/api/internal/migration/sql/0310_documents.sql',
  'new file mode 100644',
  'index 0000000..94aadcc',
  '--- /dev/null',
  '+++ b/services/api/internal/migration/sql/0310_documents.sql',
  '@@ -0,0 +1,2 @@',
  '+CREATE TABLE documents (id TEXT PRIMARY KEY);',
  '+ALTER TABLE documents ADD COLUMN size_bytes BIGINT NOT NULL;',
].join('\n')

test('a Migrations area whose section shows a diff is a banner signal, not unproven', () => {
  const proof = record()
  const input = { proof, asks: { migrations: { diff: MIGRATION_DIFF } } }
  const { banner, areas } = proofPageModel(input)
  assert.equal(areas.length, 1)
  assert.equal(areas[0].id, 'migrations')
  assert.equal(areas[0].excerpts[0].files.length, 1)
  const row = banner.signals.find((signal) => signal.label === 'Migrations')
  assert.ok(row, 'Migrations should appear in banner signals')
  assert.equal(row.href, '#a-migrations')
  assert.equal(row.summary, 'Changes 0310_documents.sql')
  assert.deepEqual(banner.unproven, [])
  const html = renderProofHtml(input)
  assert.ok(html.includes('id="a-migrations"'))
})

test('an asking area whose section shows a non-diff text excerpt counts as shown', () => {
  const proof = record()
  const input = {
    proof,
    asks: { migrations: { diff: 'not a unified diff header\n+CREATE TABLE t;' } },
  }
  const { banner } = proofPageModel(input)
  const row = banner.signals.find((signal) => signal.label === 'Migrations')
  assert.ok(row, 'Migrations with a text excerpt should appear in banner signals')
  assert.equal(row.href, '#a-migrations')
  assert.equal(row.summary, DIFF_SHOWN)
  assert.deepEqual(banner.unproven, [])
})

test('a Migrations area with no diff, files, facts, or summary stays not proven', () => {
  const proof = record()
  const { banner } = proofPageModel({ proof })
  assert.equal(
    banner.signals.some((signal) => signal.label === 'Migrations'),
    false,
  )
  assert.deepEqual(banner.unproven, ['Migrations: nothing on this page shows it'])
})
