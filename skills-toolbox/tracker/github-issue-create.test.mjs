import assert from 'node:assert/strict'
import { test } from 'node:test'
import { readFileSync } from 'node:fs'
import { createGithubAdapter } from './github.mjs'

function fixture({
  missing = false,
  linkFail = false,
  absent = false,
  missingRanking,
  expectedLabels = ['agent-plan'],
} = {}) {
  const calls = []
  const raw = { id: 9001, number: 7, title: 'Child', body: 'body', state: 'OPEN', labels: [] }
  const adapter = createGithubAdapter({
    config: { trackerConfig: { github: { repository: 'acme/tasks' } } },
    spawn: (_cmd, args) => {
      calls.push(args)
      let out = ''
      if (args[0] === 'label')
        out = (
          missing
            ? []
            : [
                'agent-plan',
                'boss:unplanned',
                'boss:todo',
                ...[1, 2, 3, 4].map((n) => `boss:priority-${n}`),
                ...[0, 1, 2, 3, 5, 8].map((n) => `boss:estimate-${n}`),
              ]
        )
          .filter((name) => name !== missingRanking)
          .map((name) => ({ name }))
      else if (args[0] === 'issue') {
        if (args[1] === 'view') out = raw
        else if (args[1] === 'edit') raw.labels.push(args[args.indexOf('--add-label') + 1])
      } else if (args[1].endsWith('/sub_issues')) {
        if (args.includes('--input')) {
          assert.deepEqual(JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8')), {
            sub_issue_id: 9001,
          })
          if (linkFail) return { status: 1, stderr: 'HTTP 504' }
          out = raw
        } else out = absent ? [[]] : [[], [{ ...raw }]]
      } else if (args[1] === 'repos/acme/tasks/issues') {
        const body = JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8'))
        assert.deepEqual(body.labels, expectedLabels)
        assert.equal(body.body, 'body')
        raw.labels = [...body.labels]
        out = { ...raw, html_url: 'https://github.com/acme/tasks/issues/7' }
      } else throw new Error(`unexpected ${args}`)
      return { status: 0, stdout: typeof out === 'string' ? out : JSON.stringify(out) }
    },
  })
  return { adapter, calls, raw }
}
const args = {
  title: 'Child',
  description: 'body',
  labels: ['agent-plan'],
  state: 'boss:unplanned',
  parentId: 'GH-1',
}
test('create links by REST id, verifies paginated membership, then exposes state', async () => {
  const h = fixture()
  assert.equal(h.adapter.operationMap.createIssue.command.verb, 'create-issue')
  const result = await h.adapter.runOperation('create-issue', args)
  assert.equal(result.id, 'GH-7')
  assert.equal(result.parentId, 'GH-1')
  const create = h.calls.findIndex((a) => a[1] === 'repos/acme/tasks/issues')
  const link = h.calls.findIndex((a) => a[1]?.endsWith('/sub_issues') && a.includes('--input'))
  const verify = h.calls.findIndex((a) => a[1]?.endsWith('/sub_issues') && a.includes('--paginate'))
  const expose = h.calls.findIndex((a) => a[0] === 'issue' && a[1] === 'edit')
  assert.ok(create < link && link < verify && verify < expose)
})
test('invalid create inputs and missing labels perform no write', async () => {
  for (const input of [
    { ...args, project: 'x' },
    { ...args, description: 'x'.repeat(65537) },
    { ...args, labels: ['boss:todo'] },
  ]) {
    const h = fixture()
    await assert.rejects(h.adapter.runOperation('create-issue', input), /project|65536|workflow/)
    assert.equal(
      h.calls.some((a) => a.includes('--input')),
      false,
    )
  }
  const h = fixture({ missing: true })
  await assert.rejects(h.adapter.runOperation('create-issue', args), /missing label/)
  assert.equal(
    h.calls.some((a) => a.includes('--input')),
    false,
  )
})
test('failed or unverified link leaves inert orphan and names it', async () => {
  for (const options of [{ linkFail: true }, { absent: true }]) {
    const h = fixture(options)
    await assert.rejects(h.adapter.runOperation('create-issue', args), /GH-7.*may have applied/)
    assert.deepEqual(h.raw.labels, ['agent-plan'])
    assert.equal(h.adapter.normalizeTicket(h.raw).stateName, null)
    assert.equal(
      h.calls.some((a) => a[1] === 'edit'),
      false,
    )
  }
})
test('standalone create applies state without sub-issue calls', async () => {
  const h = fixture()
  const result = await h.adapter.runOperation('create-issue', { ...args, parentId: undefined })
  assert.equal(result.parentId, null)
  assert.equal(
    h.calls.some((a) => a[1]?.endsWith('/sub_issues')),
    false,
  )
  assert.ok(h.raw.labels.includes('boss:unplanned'))
})

test('create applies ranking on the POST and preserves it when exposing state', async () => {
  for (const priority of [1, 2, 3, 4]) {
    const h = fixture({
      expectedLabels: ['agent-plan', `boss:priority-${priority}`, 'boss:estimate-5'],
    })
    await h.adapter.runOperation('create-issue', { ...args, priority, estimate: 5 })
    const ticket = h.adapter.normalizeTicket(h.raw)
    assert.equal(ticket.priority, priority)
    assert.equal(ticket.estimate, 5)
    assert.equal(ticket.stateName, 'boss:unplanned')
  }
})
test('create clears supplied ranking case-insensitively and deduplicates labels', async () => {
  const h = fixture({ expectedLabels: ['agent-plan', 'boss:priority-2', 'boss:estimate-5'] })
  await h.adapter.runOperation('create-issue', {
    ...args,
    labels: ['agent-plan', 'AGENT-PLAN', 'BOSS:PRIORITY-1', 'BOSS:ESTIMATE-8'],
    priority: 2,
    estimate: 5,
  })
  assert.equal(h.adapter.normalizeTicket(h.raw).priority, 2)
  assert.equal(h.adapter.normalizeTicket(h.raw).estimate, 5)
})
test('create priority zero removes supplied priority and adds the estimate', async () => {
  const h = fixture({ expectedLabels: ['agent-plan', 'boss:estimate-3'] })
  await h.adapter.runOperation('create-issue', {
    ...args,
    labels: ['agent-plan', 'boss:priority-4'],
    priority: 0,
    estimate: 3,
  })
  assert.equal(
    h.raw.labels.some((name) => /^boss:priority-/i.test(name)),
    false,
  )
  assert.equal(h.adapter.normalizeTicket(h.raw).estimate, 3)
})
test('missing ranking labels and invalid values reject before any write', async () => {
  for (const [options, ranking, error] of [
    [{ missingRanking: 'boss:estimate-5' }, { priority: 2, estimate: 5 }, /missing label/],
    [{}, { priority: 7 }, /priority must be/],
    [{}, { estimate: -1 }, /estimate must be/],
  ]) {
    const h = fixture(options)
    await assert.rejects(h.adapter.runOperation('create-issue', { ...args, ...ranking }), error)
    assert.equal(
      h.calls.some((a) => a.includes('--input') || a[1] === 'edit'),
      false,
    )
  }
})
