import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { linearListChildren, main } from './linear-children.mjs'
import { routeSelectedIssue } from '../plan/plan-child-fanout.mjs'
import { siblingSplitPlan, siblingMarker } from '../plan/plan-sibling-split.mjs'
import { DEFAULT_CONFIG, withTrackerDefaults } from '../skill-config.mjs'
import { classifyTickets, normalizeTicket } from '../epic/bs-epic-lib.mjs'

const connection = (nodes = [], hasNextPage = false) => ({ nodes, pageInfo: { hasNextPage } })
const state = { name: 'Todo', type: 'unstarted' }
const related = (identifier) => ({ id: `uuid-${identifier}`, identifier, state })
const issue = (identifier, extra = {}) => ({
  ...related(identifier),
  title: identifier,
  description: 'long\n'.repeat(1000),
  url: `https://example.com/${identifier}`,
  priority: 2,
  estimate: 3,
  createdAt: '2026-01-01',
  updatedAt: '2026-01-02',
  archivedAt: null,
  parent: null,
  assignee: null,
  creator: null,
  project: null,
  labels: connection([{ name: 'agent-plan' }]),
  attachments: connection([
    {
      id: 'attachment',
      title: 'plan',
      subtitle: null,
      url: 'https://example.com/plan',
      createdAt: '2026-01-01',
    },
  ]),
  relations: connection(),
  inverseRelations: connection(),
  ...extra,
})
test('child-reader preflight validates command credentials before planning can claim', async (t) => {
  const dir = mkdtempSync(path.join(tmpdir(), 'linear-preflight-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const file = path.join(dir, 'args.json')
  writeFileSync(file, '{}')
  const argv = ['preflight', '--args-file', file]
  let requests = 0
  const fetchImpl = async (_url, options) => {
    requests++
    const request = JSON.parse(options.body)
    assert.match(request.query, /viewer/)
    assert.doesNotMatch(request.query, /mutation/)
    return { ok: true, json: async () => ({ data: { viewer: { id: 'viewer-id' } } }) }
  }
  for (const key of [undefined, '', '  ']) {
    let output = '',
      errors = ''
    assert.equal(
      await main(argv, {
        env: key === undefined ? {} : { LINEAR_API_KEY: key },
        fetchImpl,
        write: (s) => {
          output += s
        },
        errWrite: (s) => {
          errors += s
        },
      }),
      2,
    )
    assert.equal(output, '')
    assert.match(errors, /LINEAR_API_KEY/)
  }
  assert.equal(requests, 0)
  let output = ''
  assert.equal(
    await main(['preflight'], {
      env: { LINEAR_API_KEY: 'private-test-key' },
      fetchImpl,
      write: (s) => {
        output += s
      },
      errWrite: assert.fail,
    }),
    0,
  )
  assert.deepEqual(JSON.parse(output), { ok: true })
  assert.equal(requests, 1)
  assert.doesNotMatch(output, /private-test-key|viewer-id/)
  for (const response of [
    { ok: false, status: 401, text: async () => 'Unauthorized' },
    { ok: true, json: async () => ({ data: { viewer: {} } }) },
  ]) {
    output = ''
    let errors = ''
    assert.equal(
      await main(argv, {
        env: { LINEAR_API_KEY: 'private-test-key' },
        fetchImpl: async () => response,
        write: (s) => {
          output += s
        },
        errWrite: (s) => {
          errors += s
        },
      }),
      2,
    )
    assert.equal(output, '')
    assert.ok(errors)
  }
})
function fixture({ pages = [['APP-2', 'APP-3']], overrides = {} } = {}) {
  const calls = []
  const issues = {
    'APP-1': issue('APP-1'),
    'APP-2': issue('APP-2', {
      parent: { identifier: 'APP-1' },
      relations: connection([
        { type: 'blocks', relatedIssue: related('APP-4') },
        { type: 'related', relatedIssue: related('APP-5') },
      ]),
      inverseRelations: connection([
        { type: 'blocks', issue: related('APP-6') },
        { type: 'related', issue: related('APP-7') },
      ]),
    }),
    'APP-3': issue('APP-3', { parent: { identifier: 'APP-1' }, description: null }),
    ...overrides,
  }
  const fetchImpl = async (_url, options) => {
    const { query, variables } = JSON.parse(options.body)
    calls.push({ query, variables })
    let value
    if (query.includes('children(')) {
      const index = variables.after === null ? 0 : Number(variables.after)
      value = {
        children: {
          nodes: pages[index].map((identifier) => ({ identifier })),
          pageInfo: { hasNextPage: index < pages.length - 1, endCursor: String(index + 1) },
        },
      }
    } else value = issues[variables.id]
    return { ok: true, json: async () => ({ data: { issue: value } }) }
  }
  return { options: { apiKey: 'test-key', parentId: 'APP-1', fetchImpl }, calls, issues }
}
test('hydrates full descriptions and both relation directions in page order', async () => {
  const f = fixture({ pages: [['APP-2'], ['APP-3']] })
  const result = await linearListChildren(f.options)
  assert.deepEqual(
    result.children.map((child) => child.id),
    ['APP-2', 'APP-3'],
  )
  assert.equal(result.parent.id, 'APP-1')
  assert.equal(result.children[1].description, '')
  assert.deepEqual(result.relationWarnings, [])
  const child = result.children[0]
  assert.equal(child.uuid, 'uuid-APP-2')
  assert.equal(child.description, f.issues['APP-2'].description)
  assert.equal(child.parentId, 'APP-1')
  assert.deepEqual(child.labels, ['agent-plan'])
  assert.deepEqual(child.attachments, f.issues['APP-2'].attachments.nodes)
  assert.deepEqual(child.state, state)
  for (const [field, expected] of Object.entries({
    blocks: ['APP-4'],
    blockedBy: ['APP-6'],
    relatedTo: ['APP-5', 'APP-7'],
  }))
    assert.deepEqual(
      child.relations[field].map((item) => item.id),
      expected,
    )
  assert.deepEqual(normalizeTicket(child).blockedBy, ['APP-6'])
  assert.equal(
    routeSelectedIssue({
      ...result,
      config: withTrackerDefaults({ ...DEFAULT_CONFIG, trackerConfig: { linear: {} } }),
    }).verdict,
    'fan-out',
  )
  child.description += '\n' + siblingMarker('APP-9', 'next')
  const split = siblingSplitPlan({
    sourceId: 'APP-9',
    sourceTitle: 'Source',
    parentId: 'APP-1',
    siblings: [
      { key: 'next', title: 'Next', estimate: 2, summary: 'Next task', keyChanges: ['src/a.mjs'] },
    ],
    existingChildren: result.children,
  })
  assert.deepEqual(split.reused, { next: 'APP-2' })
  assert.deepEqual(split.creates, [])
})
test('rejects truncated nested connections and missing parent', async () => {
  for (const field of ['labels', 'attachments', 'relations', 'inverseRelations']) {
    const f = fixture({ overrides: { 'APP-2': issue('APP-2', { [field]: connection([], true) }) } })
    await assert.rejects(linearListChildren(f.options), new RegExp(`APP-2.*${field}`))
  }
  await assert.rejects(
    linearListChildren(fixture({ overrides: { 'APP-1': null } }).options),
    /APP-1/,
  )
})
test('rejects repeated pagination cursors rather than returning partial children', async () => {
  const f = fixture()
  f.options.fetchImpl = async () => ({
    ok: true,
    json: async () => ({
      data: {
        issue: {
          children: { nodes: [], pageInfo: { hasNextPage: true, endCursor: 'same' } },
        },
      },
    }),
  })
  await assert.rejects(linearListChildren(f.options), /cursor/)
})
test('validates parent and credentials without requesting the network', async () => {
  const f = fixture()
  await assert.rejects(linearListChildren({ ...f.options, parentId: ' ' }), /parentId/)
  await assert.rejects(linearListChildren({ ...f.options, apiKey: '' }), {
    code: 'TRACKER_CREDENTIALS_MISSING',
  })
  assert.equal(f.calls.length, 0)
})
test('CLI writes result JSON and rejects bad arguments', async (t) => {
  const dir = mkdtempSync(path.join(tmpdir(), 'linear-children-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const file = path.join(dir, 'args.json')
  writeFileSync(file, JSON.stringify({ parentId: 'APP-1' }))
  const f = fixture()
  let output = '',
    errors = ''
  const deps = {
    env: { LINEAR_API_KEY: 'key' },
    fetchImpl: f.options.fetchImpl,
    write: (s) => {
      output += s
    },
    errWrite: (s) => {
      errors += s
    },
  }
  assert.equal(await main(['list-children', '--args-file', file], deps), 0)
  assert.equal(JSON.parse(output).children.length, 2)
  assert.equal(errors, '')
  output = ''
  assert.equal(await main(['list-children', '--unknown'], deps), 2)
  assert.equal(output, '')
  assert.ok(errors.length)
})

test('bounds hydration concurrency at four reads while retaining listing order', async () => {
  const ids = Array.from({ length: 8 }, (_, i) => `APP-${i + 2}`)
  const overrides = Object.fromEntries(ids.map((id) => [id, issue(id)]))
  const f = fixture({ pages: [ids], overrides })
  const fetchImpl = f.options.fetchImpl
  let active = 0,
    peak = 0
  f.options.fetchImpl = async (url, options) => {
    if (JSON.parse(options.body).query.includes('children(')) return fetchImpl(url, options)
    active++
    peak = Math.max(peak, active)
    await new Promise(setImmediate)
    active--
    return fetchImpl(url, options)
  }
  const result = await linearListChildren(f.options)
  assert.equal(peak, 4)
  assert.deepEqual(
    result.children.map((child) => child.id),
    ids,
  )
})

test('hydrated child identities preserve epic include and exclude selections', async () => {
  const f = fixture({
    overrides: {
      'APP-2': issue('APP-2', {
        parent: { identifier: 'APP-1' },
        labels: connection([{ name: 'agent-build' }]),
        assignee: { id: 'user-a' },
        creator: { id: 'user-c' },
        project: { id: 'project-p' },
      }),
      'APP-3': issue('APP-3', {
        parent: { identifier: 'APP-1' },
        labels: connection([{ name: 'agent-build' }]),
      }),
    },
  })
  const { children } = await linearListChildren(f.options)
  const tickets = children.map(normalizeTicket)
  for (const [dimension, field, id] of [
    ['assignees', 'assigneeId', 'user-a'],
    ['creators', 'creatorId', 'user-c'],
    ['projects', 'projectId', 'project-p'],
  ]) {
    assert.equal(tickets[0][field], id)
    assert.equal(tickets[1][field], null)
    for (const [mode, expected] of [
      ['include', ['APP-2']],
      ['exclude', ['APP-3']],
    ]) {
      const classified = classifyTickets(tickets, 'Todo', {
        selection: { [dimension]: { [mode]: [id] } },
      })
      assert.deepEqual(
        classified.eligible.map((ticket) => ticket.id),
        expected,
      )
      assert.equal(classified.skipped.length, 1)
    }
  }
})
