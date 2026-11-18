import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { INVENTORY_SCHEMA, compareInventories, proofInventory } from './verify-proof-inventory.mjs'

const CLI = 'skills-toolbox/verify/verify-proof-page.mjs'

/** @param {Partial<import('./verify-proof-inventory.mjs').ProofInventory>} [more] */
const inventory = (more = {}) => ({
  schema: INVENTORY_SCHEMA,
  items: ['Adds column invoices.due_on', 'Drops view legacy_totals'],
  blocks: [
    { kind: 'table', title: 'invoices' },
    { kind: 'table', title: 'legacy_totals' },
    { kind: 'steps', title: 'Run the backfill' },
  ],
  signals: ['Database', 'Terraform'],
  sections: ['a-migrations', 'sec-infra'],
  clips: ['media/check-in.mp4'],
  ...more,
})

test('identical inventories lose nothing and reword nothing', () => {
  assert.deepEqual(compareInventories(inventory(), inventory()), { lost: [], reworded: [] })
})

test('each kind of loss is named', () => {
  const before = inventory()
  const cases = [
    [{ items: ['Adds column invoices.due_on'] }, 'items: 2 before, 1 after'],
    [{ blocks: before.blocks.slice(1) }, 'table blocks: 2 before, 1 after'],
    [{ blocks: before.blocks.slice(0, 2) }, 'steps blocks: 1 before, 0 after'],
    [{ signals: ['Database'] }, 'signal: Terraform'],
    [{ sections: ['a-migrations'] }, 'section: sec-infra'],
    [{ clips: [] }, 'clip: media/check-in.mp4'],
  ]
  for (const [after, loss] of cases)
    assert.deepEqual(compareInventories(before, inventory(after)).lost, [loss], loss)
})

test('the same counts in other words are reworded, not lost, and additions lose nothing', () => {
  const after = inventory({
    items: ['Adds invoices.due_on', 'Drops legacy_totals', 'Adds an index'],
    blocks: [
      { kind: 'table', title: 'billing.invoices' },
      { kind: 'table', title: 'legacy_totals' },
      { kind: 'steps', title: 'Run the backfill' },
      { kind: 'code', title: 'main.tf:1' },
    ],
    signals: ['Database', 'Terraform', 'Cost'],
  })
  const { lost, reworded } = compareInventories(inventory(), after)
  assert.deepEqual(lost, [])
  assert.deepEqual(reworded, [
    'item before only: Adds column invoices.due_on',
    'item before only: Drops view legacy_totals',
    'item after only: Adds invoices.due_on',
    'item after only: Drops legacy_totals',
    'item after only: Adds an index',
    'block before only: table invoices',
    'block after only: table billing.invoices',
    'block after only: code main.tf:1',
  ])
})

test('the inventory counts each After merge step as a steps block and names media as the page does', () => {
  const still = (file) => ({ file, caption: file })
  const listed = proofInventory(
    /** @type {any} */ ({
      areas: [
        {
          id: 'migrations',
          facts: [{ text: 'Adds column invoices.due_on', flag: false }],
          blocks: [
            { kind: 'route', method: 'GET', path: '/invoices', auth: 'user', change: 'added' },
          ],
        },
      ],
      afterMerge: [{ id: 'after-1', text: 'Run the backfill', done: false }],
      banner: { signals: [{ label: 'Database' }] },
      repoSections: [{ id: 'infra' }],
      evidence: {
        heads: [{ clip: { file: 'clips/head.mp4', stills: [still('shots/a.png')] }, index: 0 }],
        pairs: [{ base: still('shots/before.png'), head: still('shots/a.png') }],
        documents: [{ head: { file: 'docs/r.pdf', pages: [still('docs/r-1.png')] }, base: null }],
      },
    }),
  )
  assert.deepEqual(listed, {
    schema: INVENTORY_SCHEMA,
    items: ['Adds column invoices.due_on'],
    blocks: [
      { kind: 'route', title: 'GET /invoices' },
      { kind: 'steps', title: 'Run the backfill' },
    ],
    signals: ['Database'],
    sections: ['a-migrations', 'sec-infra'],
    clips: ['media/head.mp4', 'media/a.png', 'media/before.png', 'media/r.pdf', 'media/r-1.png'],
  })
})

test('inventory-diff exits 0 with nothing lost, 1 naming each loss, 2 on unreadable input', (t) => {
  const dir = tempDir('inventory-diff-', t)
  const file = (name, value) => {
    const path = join(dir, name)
    writeFileSync(path, typeof value === 'string' ? value : JSON.stringify(value))
    return path
  }
  const before = file('before.json', inventory())
  const run = (...args) =>
    spawnSync(process.execPath, [CLI, 'inventory-diff', ...args], { encoding: 'utf8' })

  const same = run('--before', before, '--after', file('same.json', inventory()))
  assert.equal(same.status, 0, same.stderr)
  assert.equal(same.stdout, 'nothing lost\n')

  const reworded = run(
    '--before',
    before,
    '--after',
    file(
      'reworded.json',
      inventory({ items: ['Adds invoices.due_on', 'Drops view legacy_totals'] }),
    ),
  )
  assert.equal(reworded.status, 0, reworded.stderr)
  assert.match(reworded.stdout, /^nothing lost\nreworded item before only: Adds column/)

  const lossy = run(
    '--before',
    before,
    '--after',
    file('lossy.json', inventory({ signals: ['Database'], clips: [] })),
  )
  assert.equal(lossy.status, 1, lossy.stderr)
  assert.equal(lossy.stdout, 'lost signal: Terraform\nlost clip: media/check-in.mp4\n')

  const broken = run('--before', before, '--after', file('broken.json', '{"schema": '))
  assert.equal(broken.status, 2)
  assert.ok(broken.stderr.includes('broken.json'))
  const foreign = run('--before', before, '--after', file('foreign.json', { schema: 'other/1' }))
  assert.equal(foreign.status, 2)
  assert.ok(foreign.stderr.includes('foreign.json'))
  const missing = run('--before', before)
  assert.equal(missing.status, 2)
  assert.ok(missing.stderr.includes('inventory-diff --before'))
})
