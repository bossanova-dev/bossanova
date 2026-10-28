import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { createGithubAdapter } from './github.mjs'
import { DEFAULT_CONFIG, withTrackerDefaults } from '../skill-config.mjs'
import {
  epicPhase25WritePlan,
  detectEpicParent,
  epicReverifyVerdict,
} from '../plan/plan-epic-phase25.mjs'
import {
  serializeEpicSpec,
  parseEpicSpec,
  reconcileEpicChildren,
  epicWiringPlan,
} from '../plan/plan-epic-lib.mjs'
import { normalizeTicket, buildGraph, readyTickets } from '../epic/bs-epic-lib.mjs'

const config = withTrackerDefaults({
  ...DEFAULT_CONFIG,
  adapters: { tracker: 'github' },
  trackerConfig: { github: { repository: 'acme/tasks' } },
})
const labels = [
  'boss:unplanned',
  'boss:todo',
  'boss:in-progress',
  'boss:in-review',
  'agent-build',
  'agent-plan',
  'needs-human',
  'agent-question',
  'epic',
  'boss:priority-2',
  'boss:estimate-3',
]
const raw = (number, title, body, names) => ({
  number,
  id: 1000 + number,
  title,
  body,
  labels: names.map((name) => ({ name })),
  state: 'open',
  html_url: `https://github.com/acme/tasks/issues/${number}`,
  repository_url: 'https://api.github.com/repos/acme/tasks',
  issue_dependencies_summary: { total_blocked_by: 0, total_blocking: 0 },
  sub_issues_summary: { total: 0 },
})

// Stateful API simulation: reads are computed from writes, never from expected result bundles.
function tracker(t) {
  const dir = mkdtempSync(join(tmpdir(), 'github-epic-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const rows = [raw(1, 'Epic', 'Reporter context.', ['boss:unplanned', 'agent-plan'])]
  const comments = [],
    children = new Map(),
    edges = [],
    writes = []
  const spawn = (_cmd, argv) => {
    let result = ''
    if (argv[0] === 'label') result = labels.map((name) => ({ name }))
    else if (argv[0] === 'issue') {
      const row = rows.find((row) => row.number === Number(argv[2]))
      if (argv[1] === 'view') result = row
      else if (argv[1] === 'edit') {
        for (let i = 3; i < argv.length; i++) {
          if (argv[i] === '--add-label') row.labels.push({ name: argv[++i] })
          else if (argv[i] === '--remove-label') {
            const remove = argv[++i]
            row.labels = row.labels.filter((label) => label.name !== remove)
          }
        }
      } else throw new Error(`unexpected issue command ${argv}`)
    } else {
      const path = argv[1],
        number = Number(/\/issues\/(\d+)/.exec(path)?.[1])
      const row = rows.find((row) => row.number === number)
      const payload = argv.includes('--input')
        ? JSON.parse(readFileSync(argv[argv.indexOf('--input') + 1], 'utf8'))
        : null
      if (payload) {
        writes.push({ path, payload, method: argv[argv.indexOf('--method') + 1] })
        if (path === 'repos/acme/tasks/issues') {
          result = raw(rows.length + 1, payload.title, payload.body, payload.labels)
          rows.push(result)
        } else if (path.endsWith('/sub_issues')) {
          const child = rows.find((row) => row.id === payload.sub_issue_id)
          children.set(child.number, number)
          child.parent_issue_url = `https://api.github.com/repos/acme/tasks/issues/${number}`
          row.sub_issues_summary.total++
          result = child
        } else if (path.endsWith('/dependencies/blocked_by')) {
          const blocker = rows.find((row) => row.id === payload.issue_id)
          edges.push([number, blocker.number])
          row.issue_dependencies_summary.total_blocked_by++
          blocker.issue_dependencies_summary.total_blocking++
          result = blocker
        } else if (path.endsWith('/comments')) {
          result = {
            id: comments.length + 50,
            issue: number,
            body: payload.body,
            author_association: 'MEMBER',
            html_url: `${row.html_url}#issuecomment-${comments.length + 50}`,
          }
          comments.push(result)
        } else throw new Error(`unexpected write ${path}`)
      } else if (path.endsWith('/sub_issues'))
        result = [[], rows.filter((row) => children.get(row.number) === number)]
      else if (path.endsWith('/dependencies/blocked_by'))
        result = [
          edges
            .filter(([child]) => child === number)
            .map(([, blocker]) => rows.find((row) => row.number === blocker)),
        ]
      else if (path.endsWith('/dependencies/blocking'))
        result = [
          edges
            .filter(([, blocker]) => blocker === number)
            .map(([child]) => rows.find((row) => row.number === child)),
        ]
      else if (path.endsWith('/comments'))
        result = [comments.filter((comment) => comment.issue === number)]
      else if (/\/issues\/comments\/\d+$/.test(path))
        result = comments.find((comment) => comment.id === Number(path.split('/').at(-1)))
      else if (path.endsWith('/parent')) {
        if (!children.has(number)) return { status: 1, stderr: 'HTTP 404: Not Found' }
        result = rows.find((row) => row.number === children.get(number))
      } else if (row) result = row
      else throw new Error(`unexpected read ${path}`)
    }
    return { status: 0, stdout: typeof result === 'string' ? result : JSON.stringify(result) }
  }
  return { adapter: createGithubAdapter({ config, spawn }), dir, rows, writes }
}

const childBody = (key) =>
  [
    `## Summary\n\nImplement ${key}.`,
    '## Approach\n\nDo the work.',
    '## Key changes\n\n- Component update.',
    '## Testing\n\n- Unit coverage.',
    '## Risks / unknowns\n\n- None.',
    '## Acceptance criteria\n\n- [ ] Works.',
    '## Required proof\n\n- [ ] (backend-only) Unit tests.',
    '## Planning\n\n- Contract: v1',
    `<!-- boss-plan-epic-child:${key} -->\n\n## Original notes\n\nReporter context.`,
  ].join('\n\n')
const overview = (ids) =>
  `## Summary\n\nShip the epic.\n\n## Child tickets\n\n${ids.map((id) => `- ${id}`).join('\n')}\n\n## Planning\n\n- Contract: v1\n\n## Original notes\n\nReporter context.`
const spec = () =>
  parseEpicSpec(
    serializeEpicSpec({
      parentId: 'GH-1',
      parent: { title: 'Epic', goal: 'Ship', keyChanges: ['Component'], priority: 2 },
      children: ['a', 'b', 'c'].map((key, i) => ({
        key,
        title: `Child ${key}`,
        goal: 'Implement',
        keyChanges: ['Component'],
        priority: 2,
        estimate: 3,
        blockedByKeys: ['a', 'b'].slice(0, i),
        agentBuild: true,
      })),
    }),
  )

test('GitHub decomposition persists spec, links children, preserves all edges and passes epic reverify', async (t) => {
  const h = tracker(t),
    run = (verb, args) => h.adapter.runOperation(verb, args)
  const storedSpec = spec(),
    file = join(h.dir, 'spec.json'),
    ids = { parent: 'GH-1' },
    bodies = {}
  writeFileSync(file, serializeEpicSpec(storedSpec))
  for (const op of epicPhase25WritePlan({
    parentId: 'GH-1',
    spec: storedSpec,
    storage: 'single-record',
    unplannedState: 'boss:unplanned',
    labelsToStrip: ['agent-plan'],
  })) {
    if (op.op === 'moveState') await run('move-state', { id: 'GH-1', labels: ['boss:unplanned'] })
    else if (op.op === 'writeEpicSpec') {
      const written = await run('write-epic-spec', { ...op.args, file })
      const outFile = join(h.dir, 'spec-read.json')
      await run('read-plan', { id: written.id, outFile })
      assert.deepEqual(parseEpicSpec(readFileSync(outFile, 'utf8')), storedSpec)
      assert.equal(h.rows.length, 1, 'spec read-back precedes the first child')
    } else if (op.op === 'createChild') {
      const { key, title, state, priority, estimate, parentId } = op.args
      const description = childBody(key)
      const created = await run('create-issue', {
        title,
        description,
        parentId,
        state,
        priority,
        estimate,
        labels: ['agent-build'],
      })
      ids[key] = created.id
      bodies[created.id] = description
      await run('set-priority-estimate', { id: created.id, priority, estimate })
      // The planner's normal child publish stores the detailed plan separately from its creation body.
      writeFileSync(join(h.dir, `${key}.md`), description)
      await run('write-plan', {
        issue: created.id,
        file: join(h.dir, `${key}.md`),
        title: `Implementation plan (${created.id})`,
      })
      await run('move-state', { id: created.id, state: 'boss:todo' })
    } else assert.fail(`unexpected op ${op.op}`)
  }
  for (const edge of epicWiringPlan(storedSpec, ids))
    await run('append-dependency', { id: edge.childId, blockedBy: edge.blockedBy })
  const intended = overview([ids.a, ids.b, ids.c]),
    overviewFile = join(h.dir, 'overview.md')
  writeFileSync(overviewFile, intended)
  const published = await run('write-plan', {
    issue: 'GH-1',
    title: 'Implementation plan (GH-1)',
    file: overviewFile,
  })
  await run('read-plan', { id: published.id, outFile: overviewFile })
  await run('move-state', { id: 'GH-1', state: 'boss:todo', labels: ['boss:unplanned', 'epic'] })
  const { children } = await run('list-children', { parentId: 'GH-1' })
  const parent = await run('get-issue', { id: 'GH-1' })
  assert.equal(detectEpicParent(parent).isEpicParent, true)
  const reconciled = reconcileEpicChildren(storedSpec, children)
  assert.equal(reconciled.ok, true)
  assert.deepEqual(reconciled.repairs, [])
  assert.deepEqual(reconciled.missing, [])
  for (const child of children) {
    const hydrated = await run('get-issue', { id: child.id })
    assert.deepEqual(h.adapter.readDependencies(hydrated), h.adapter.readDependencies(child))
  }
  const graph = buildGraph(children.map(normalizeTicket))
  assert.deepEqual(graph.inEpicBlockers.get(ids.b), [ids.a])
  assert.deepEqual(graph.inEpicBlockers.get(ids.c), [ids.a, ids.b])
  const state = {
    merged: new Set(),
    failed: new Set(),
    inFlight: new Set(),
    externallyCleared: new Set(),
  }
  assert.deepEqual(
    readyTickets(graph, state).map((row) => row.id),
    [ids.a],
  )
  state.merged.add(ids.a)
  assert.deepEqual(
    readyTickets(graph, state).map((row) => row.id),
    [ids.b],
  )
  state.merged.add(ids.b)
  assert.deepEqual(
    readyTickets(graph, state).map((row) => row.id),
    [ids.c],
  )
  const verdict = epicReverifyVerdict({
    parentId: 'GH-1',
    childIds: [ids.a, ids.b, ids.c],
    parent,
    children,
    spec: storedSpec,
    config,
    roles: {
      planned: 'boss:todo',
      unplanned: 'boss:unplanned',
      epic: 'epic',
      agentBuild: 'agent-build',
      needsHuman: 'needs-human',
      agentQuestion: 'agent-question',
      agentPlan: 'agent-plan',
    },
    parentOverview: { intended, stored: readFileSync(overviewFile, 'utf8') },
    childBodies: Object.fromEntries(
      children.map((child) => [
        child.id,
        { intended: bodies[child.id], stored: child.description },
      ]),
    ),
  })
  assert.deepEqual(verdict.blockers, [])
  assert.equal(verdict.ok, true)
  assert.equal(
    h.writes.some((write) => write.method === 'PATCH'),
    false,
  )
  assert.equal(h.rows[0].body, 'Reporter context.')
})

test('hand-assembled sub-issues enumerate without inventing an epic spec store', async (t) => {
  const h = tracker(t)
  await h.adapter.runOperation('create-issue', {
    title: 'Hand assembled',
    description: 'Human notes',
    parentId: 'GH-1',
    state: 'boss:todo',
    labels: ['agent-build'],
  })
  const result = await h.adapter.runOperation('list-children', { parentId: 'GH-1' })
  assert.equal(result.children.length, 1)
  const parent = await h.adapter.runOperation('get-issue', { id: 'GH-1' })
  assert.equal(detectEpicParent(parent).isEpicParent, false)
})
