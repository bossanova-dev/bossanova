import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createGithubAdapter } from './github.mjs'
import { runCli } from './cli.mjs'

const config = { trackerConfig: { github: { repository: 'github.acme.test/acme/tasks' } } }
const row = (number, extra = {}) => ({
  number,
  title: `Issue ${number}`,
  body: 'Notes: key',
  state: 'open',
  state_reason: null,
  created_at: '2026-01-01T00:00:00Z',
  closed_at: null,
  author_association: 'MEMBER',
  ...extra,
})
function harness(pages) {
  const calls = []
  const adapter = createGithubAdapter({
    config,
    spawn: (_cmd, argv) => {
      calls.push(argv)
      const page = Number(argv.find((arg) => arg.startsWith('page='))?.slice(5))
      assert.ok(page > 0)
      return { status: 0, stdout: JSON.stringify(pages[page - 1]) }
    },
  })
  return { adapter, calls }
}

test('marked issues span bounded REST pages and exclude PRs, outsiders and unmarked bodies', async () => {
  const h = harness([
    Array.from({ length: 100 }, (_, i) => row(i + 1)),
    [
      row(101, { author_association: 'OWNER' }),
      row(102, { author_association: 'COLLABORATOR' }),
      row(103, { pull_request: {} }),
      row(104, { author_association: 'NONE' }),
      row(105, { body: null }),
      row(106, { body: 'ordinary' }),
    ],
  ])
  const rows = await h.adapter.selectMarked({ markerPrefix: 'Notes: ', updatedAfter: '2026-01-01' })
  assert.equal(rows.length, 102)
  assert.equal(rows.at(-1).identifier, 'GH-102')
  assert.equal(rows[0].description, 'Notes: key')
  assert.equal(rows[0].createdAt, '2026-01-01T00:00:00Z')
  assert.equal(rows[0].resolution, null)
  assert.equal(rows[0].resolvedAt, null)
  for (const call of h.calls) {
    assert.equal(call[1], 'repos/acme/tasks/issues')
    assert.ok(call.includes('github.acme.test'))
    for (const field of ['state=all', 'per_page=100', 'since=2026-01-01'])
      assert.ok(call.includes(field))
    assert.equal(call.includes('--paginate'), false)
  }
})

test('closed markers map their completion or cancellation and timestamp', async () => {
  const closed_at = '2026-02-01T00:00:00Z'
  const h = harness([
    [
      ...['completed', 'not_planned', 'duplicate', null].map((state_reason, i) =>
        row(i + 1, { state: 'closed', state_reason, closed_at }),
      ),
    ],
  ])
  const result = await h.adapter.selectMarked({ markerPrefix: 'Notes: ' })
  assert.deepEqual(
    result.map((r) => r.resolution),
    ['done', 'canceled', 'canceled', 'done'],
  )
  assert.ok(result.every((r) => r.resolvedAt === closed_at))
})

test('marked snapshot rejects incomplete pagination, malformed rows and invalid query shape', async () => {
  const full = Array.from({ length: 100 }, (_, i) => row(i + 1))
  await assert.rejects(
    harness([full]).adapter.selectMarked({ markerPrefix: 'Notes: ', maxPages: 1 }),
    /marker snapshot incomplete/,
  )
  for (const pages of [
    [{}],
    [[null]],
    [[row(1, { title: null })]],
    [[row(1, { number: '1' })]],
    [[row(1, { state: null })]],
  ]) {
    await assert.rejects(
      harness(pages).adapter.selectMarked({ markerPrefix: 'Notes: ' }),
      /malformed/,
    )
  }
  for (const query of [
    {},
    { markerPrefix: ' ' },
    { markerPrefix: 'x', maxPages: 0 },
    { markerPrefix: 'x', updatedAfter: 'no' },
    { markerPrefix: 'x', extra: true },
  ]) {
    const h = harness([])
    await assert.rejects(h.adapter.selectMarked(query))
    assert.equal(h.calls.length, 0)
  }
})

test('fetch-marked CLI writes a complete GitHub snapshot and preserves output on a full bound', async (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'github-marked-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const target = join(dir, 'marked.json')
  let output = '',
    error = ''
  const invoke = (adapter, args = []) =>
    runCli(['fetch-marked', '--prefix', 'Notes: ', '--out-file', target, ...args], {
      resolveAdapter: () => adapter,
      write: (s) => {
        output += s
      },
      errWrite: (s) => {
        error += s
      },
    })
  assert.equal(await invoke(harness([[row(1)]]).adapter), 0)
  assert.equal(JSON.parse(output).outcome, 'marked-fetched')
  assert.equal(JSON.parse(readFileSync(target, 'utf8'))[0].identifier, 'GH-1')
  writeFileSync(target, 'keep')
  assert.notEqual(
    await invoke(harness([Array.from({ length: 100 }, (_, i) => row(i + 1))]).adapter, [
      '--max-pages',
      '1',
    ]),
    0,
  )
  assert.match(error, /marker snapshot incomplete/)
  assert.equal(readFileSync(target, 'utf8'), 'keep')
})
