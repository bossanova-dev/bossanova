import { test } from 'node:test'
import assert from 'node:assert/strict'
import { writeFileSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { runCli } from '../tracker/cli.mjs'
import { DEFAULT_CONFIG, mergeConfig } from '../skill-config.mjs'
import { verifyWriteback } from './plan-writeback-verify.mjs'
import { applyReadReceipt, loadReadReceipt } from './plan-writeback-receipt.mjs'

process.env.BOSS_GATE_OUTCOME_FILE = join(tempDir('gate-outcome-suite-'), 'outcomes.tsv')

const intended = [
  'Summary',
  'Approach',
  'Key changes',
  'Testing',
  'Risks / unknowns',
  'Acceptance criteria',
  'Required proof',
  'Planning',
  'Original notes',
]
  .map((heading) => `## ${heading}\n\n- first\n- second`)
  .join('\n\n')
const receipt = { settled: false, attempts: 3, elapsedMs: 10000 }
const config = mergeConfig(DEFAULT_CONFIG, {
  adapters: { tracker: 'demo' },
  trackerConfig: { demo: { descriptionNormalization: { tolerated: [] } } },
})
const verify = (storedText) => verifyWriteback({ config, intendedText: intended, storedText })
for (const [name, stored, verdict] of [
  ['drift', 'pre-save description', 'unsettled'],
  ['unattributed', intended.replace('- first\n- second', '* first\n* second'), 'unsettled'],
  ['byte-exact', intended, 'byte-exact'],
]) {
  test(`unsettled receipt with ${name} content gives ${verdict}`, () => {
    const result = applyReadReceipt(verify(stored), receipt)
    assert.equal(result.verdict, verdict)
    assert.equal(result.exitCode, verdict === 'unsettled' ? 1 : 0)
    if (verdict === 'unsettled') assert.match(result.reason, /3.*10000.*unconfirmed/)
  })
}
test('normalized-equivalent stays a pass and settled drift stays drift', () => {
  const normalized = verifyWriteback({
    config: DEFAULT_CONFIG,
    intendedText: intended,
    storedText: intended.replace('- first\n- second', '* first\n* second'),
  })
  assert.equal(normalized.verdict, 'normalized-equivalent')
  assert.equal(applyReadReceipt(normalized, receipt), normalized)
  const drift = verify('old')
  assert.equal(applyReadReceipt(drift, { ...receipt, settled: true }), drift)
})
const helper = fileURLToPath(new URL('./plan-writeback-verify.mjs', import.meta.url))
function cli(dir, receiptPath, storedPath) {
  const intendedPath = join(dir, 'intended.md')
  writeFileSync(intendedPath, intended)
  return spawnSync(
    process.execPath,
    [helper, '--intended', intendedPath, '--stored', storedPath, '--read-receipt', receiptPath],
    {
      encoding: 'utf8',
      env: { ...process.env, BOSS_GATE_OUTCOME_FILE: join(dir, 'outcomes.tsv') },
    },
  )
}
for (const value of [null, '{', JSON.stringify({ settled: 'false' })]) {
  test(`unreadable receipt refuses without a verdict: ${value}`, (t) => {
    const dir = tempDir('read-receipt-', t)
    const receiptPath = join(dir, 'receipt.json')
    const storedPath = join(dir, 'stored.md')
    writeFileSync(storedPath, intended)
    if (value !== null) writeFileSync(receiptPath, value)
    assert.throws(() => loadReadReceipt(receiptPath))
    const result = cli(dir, receiptPath, storedPath)
    assert.equal(result.status, 1)
    assert.doesNotMatch(result.stdout + result.stderr, /writeback-verdict:/)
    assert.match(readFileSync(join(dir, 'outcomes.tsv'), 'utf8'), /unreadable-receipt/)
  })
}
for (const alwaysStale of [false, true]) {
  test(`lagged adapter receipt feeds verifier (always stale: ${alwaysStale})`, async (t) => {
    const dir = tempDir('writeback-integration-', t)
    const expected = join(dir, 'expected.md')
    const stale = join(dir, 'stale.md')
    const storedPath = join(dir, 'stored.md')
    const receiptPath = join(dir, 'receipt.json')
    writeFileSync(expected, intended)
    writeFileSync(stale, 'old')
    let calls = 0
    let time = 0
    let out = ''
    const code = await runCli(
      [
        'read-description',
        '--id',
        'ABC-1',
        '--out-file',
        storedPath,
        '--expect-file',
        expected,
        '--stale-file',
        stale,
        '--settle-ms',
        '10000',
      ],
      {
        write: (s) => {
          out += s
        },
        errWrite: () => {},
        resolveAdapter: () => ({
          readDescription: async () => ({
            id: 'uuid',
            identifier: 'ABC-1',
            description: ++calls < 3 || alwaysStale ? 'old' : intended,
          }),
        }),
        now: () => time,
        sleep: async (ms) => {
          time += ms
        },
      },
    )
    assert.equal(code, 0)
    writeFileSync(receiptPath, out)
    const result = cli(dir, receiptPath, storedPath)
    assert.equal(result.status, alwaysStale ? 1 : 0)
    assert.match(
      result.stdout,
      new RegExp(`writeback-verdict: ${alwaysStale ? 'unsettled' : 'byte-exact'}`),
    )
  })
}
