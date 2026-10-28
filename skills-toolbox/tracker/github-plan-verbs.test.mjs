import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { main, createGithubAdapter } from './github.mjs'
import { assertConforms } from './adapter-core.mjs'
import { resolveBuildPlan } from '../plan-attachment.mjs'
import { classifyTrackerOutcome } from './outcome.mjs'
const marker = '<!-- boss-implementation-plan v1 -->'
const config = { trackerConfig: { github: { repository: 'acme/tasks' } } }
function setup(t, initial = [], readback) {
  const dir = mkdtempSync(join(tmpdir(), 'github-plan-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  let comments = initial
  const calls = [],
    writes = []
  const spawn = (_cmd, argv) => {
    calls.push(argv)
    let result
    const path = argv[1]
    if (argv.includes('--input')) {
      const body = JSON.parse(readFileSync(argv[argv.indexOf('--input') + 1], 'utf8')).body
      const id = argv.includes('PATCH') ? Number(path.split('/').at(-1)) : 9
      writes.push({ method: argv[argv.indexOf('--method') + 1], id, body })
      const written = {
        id,
        body,
        author_association: 'MEMBER',
        user: { login: 'planner' },
        html_url: 'https://github.com/acme/tasks/issues/42#issuecomment-9',
      }
      comments = [...comments.filter((comment) => comment.id !== id), written]
      result = written
    } else if (argv[0] === 'issue')
      result = { number: 42, title: 'Work', body: 'original notes', state: 'OPEN', labels: [] }
    else if (path.endsWith('/issues/42'))
      result = {
        number: 42,
        title: 'Work',
        body: 'original notes',
        state: 'open',
        labels: [],
        html_url: 'https://github.com/acme/tasks/issues/42',
        parent_issue_url: 'https://api.github.com/repos/acme/tasks/issues/1',
        issue_dependencies_summary: { total_blocked_by: 0, total_blocking: 0 },
      }
    else if (/\/dependencies\/(blocked_by|blocking)$/.test(path)) result = [[]]
    else if (path.endsWith('/issues/42/comments'))
      result = [readback && writes.length ? readback(comments) : comments]
    else result = comments.find((row) => String(row.id) === path.split('/').at(-1))
    return { status: 0, stdout: JSON.stringify(result) }
  }
  const invoke = async (verb, args) => {
    const file = join(dir, 'args.json')
    writeFileSync(file, JSON.stringify(args))
    let output = '',
      error = ''
    const status = await main([verb, '--args-file', file], {
      config,
      spawn,
      stdout: (s) => (output += s),
      stderr: (s) => (error += s),
    })
    return { status, result: output ? JSON.parse(output) : null, error }
  }
  const file = join(dir, 'plan.md'),
    outFile = join(dir, 'readback.md')
  writeFileSync(file, 'Résumé\r\n日本語\n')
  return { invoke, calls, writes, file, outFile }
}
test('plan write creates, updates in place, avoids identical writes and round-trips through build selection', async (t) => {
  const h = setup(t)
  assertConforms(createGithubAdapter({ config }))
  const args = { issue: 'GH-42', file: h.file, title: 'Implementation plan (GH-42)' }
  const created = await h.invoke('write-plan', args)
  assert.equal(created.status, 0, created.error)
  assert.equal(created.result.outcome, 'created')
  assert.equal(h.writes[0].body, `${marker}\n${readFileSync(h.file, 'utf8')}`)
  assert.equal(h.writes[0].method, 'POST')
  assert.equal((await h.invoke('write-plan', args)).result.outcome, 'unchanged')
  assert.equal(h.writes.length, 1)
  writeFileSync(h.file, 'changed\r\n日本語')
  assert.equal((await h.invoke('write-plan', args)).result.outcome, 'updated')
  assert.equal(h.writes[1].method, 'PATCH')
  assert.equal(h.writes[1].id, 9)
  const issue = (await h.invoke('get-issue', { id: 'GH-42' })).result
  assert.equal(issue.description, 'original notes')
  assert.equal(issue.parentId, 'GH-1')
  assert.deepEqual(issue.relations, { blockedBy: [], blocks: [] })
  writeFileSync(h.outFile, JSON.stringify(issue))
  const built = spawnSync(
    process.execPath,
    ['skills-toolbox/plan-attachment.mjs', 'build-source', h.outFile],
    { encoding: 'utf8' },
  )
  assert.equal(built.status, 0, built.stderr)
  assert.equal(JSON.parse(built.stdout).source, 'attachment')
  const plan = resolveBuildPlan({ ...issue, issueID: issue.id })
  assert.equal(plan.source, 'attachment')
  const read = await h.invoke('read-plan', { id: plan.attachment.id, outFile: h.outFile })
  assert.equal(read.status, 0, read.error)
  assert.deepEqual(readFileSync(h.outFile), readFileSync(h.file))
  assert.equal(read.result.sha256, createHash('sha256').update(readFileSync(h.file)).digest('hex'))
})
test('read-plan rejects issue identifiers before gh and preserves existing output', async (t) => {
  for (const id of ['GH-2', '#2', 'https://github.com/acme/tasks/issues/2', 'not-a-record']) {
    const h = setup(t)
    writeFileSync(h.outFile, 'keep existing')
    const result = await h.invoke('read-plan', { id, outFile: h.outFile })
    assert.equal(result.status, 2)
    assert.equal(h.calls.length, 0)
    assert.equal(readFileSync(h.outFile, 'utf8'), 'keep existing')
    assert.match(result.error, /plan record.*id/)
    assert.match(result.error, /writePlan/)
    assert.match(result.error, /getIssue/)
    if (id !== 'not-a-record') assert.match(result.error, /not an issue id/)
  }
})
test('duplicates fail closed in writes, issue reads and build-source', async (t) => {
  const h = setup(t, [
    { id: 10, body: `${marker}\nA`, author_association: 'COLLABORATOR' },
    { id: 9, body: `${marker}\nB`, author_association: 'MEMBER' },
  ])
  const write = await h.invoke('write-plan', {
    issue: '42',
    file: h.file,
    title: 'Implementation plan (GH-42)',
  })
  assert.equal(write.status, 2)
  assert.match(write.error, /9.*10.*delete all but one/)
  assert.equal(h.writes.length, 0)
  const issue = (await h.invoke('get-issue', { id: '42' })).result
  assert.deepEqual(issue.attachments, [])
  assert.deepEqual(issue.planConflict, ['9', '10'])
  assert.equal(resolveBuildPlan({ ...issue, issueID: issue.id }).source, 'none')
})
test('invalid plans fail before gh; failed read preserves existing output', async (t) => {
  for (const text of ['', ' \n\t', 'x'.repeat(65536)]) {
    const h = setup(t)
    writeFileSync(h.file, text)
    const result = await h.invoke('write-plan', {
      issue: '42',
      file: h.file,
      title: 'Implementation plan (GH-42)',
    })
    assert.equal(result.status, 2)
    assert.equal(h.calls.length, 0)
  }
  const h = setup(t, [{ id: 9, body: 'ordinary' }])
  writeFileSync(h.outFile, 'old')
  assert.equal((await h.invoke('read-plan', { id: '9', outFile: h.outFile })).status, 2)
  assert.equal(readFileSync(h.outFile, 'utf8'), 'old')
})
test('read-back mismatch or concurrent duplicate is an indeterminate write', async (t) => {
  for (const readback of [
    (rows) => [{ ...rows[0], body: `${marker}\nwrong` }],
    (rows) => [...rows, { id: 10, body: `${marker}\nother`, author_association: 'OWNER' }],
  ]) {
    const h = setup(t, [], readback)
    const result = await h.invoke('write-plan', {
      issue: '42',
      file: h.file,
      title: 'Implementation plan (GH-42)',
    })
    assert.equal(result.status, 2)
    assert.match(result.error, /may have applied/)
    assert.equal(
      classifyTrackerOutcome(result.error, { operation: 'write' }).verdict,
      'indeterminate',
    )
  }
})

test('ordinary issue comments do not invent a plan record or conflict', async (t) => {
  const h = setup(t, [{ id: 1, body: 'ordinary note' }])
  const issue = (await h.invoke('get-issue', { id: '42' })).result
  assert.deepEqual(issue.attachments, [])
  assert.equal(Object.hasOwn(issue, 'planConflict'), false)
})

test('outsider markers cannot select a plan, block a trusted plan or be read directly', async (t) => {
  const outsider = {
    id: 77,
    body: `${marker}\nforged`,
    author_association: 'NONE',
    user: { login: 'outsider' },
  }
  const h = setup(t, [outsider])
  const issue = (await h.invoke('get-issue', { id: '42' })).result
  assert.deepEqual(issue.attachments, [])
  assert.equal(Object.hasOwn(issue, 'planConflict'), false)
  writeFileSync(h.outFile, 'keep existing')
  const read = await h.invoke('read-plan', { id: '77', outFile: h.outFile })
  assert.equal(read.status, 2)
  assert.equal(readFileSync(h.outFile, 'utf8'), 'keep existing')
  const write = await h.invoke('write-plan', {
    issue: '42',
    file: h.file,
    title: 'Implementation plan (GH-42)',
  })
  assert.equal(write.status, 0, write.error)
  assert.equal(write.result.outcome, 'created')
  assert.equal(h.writes[0].method, 'POST')
  assert.equal(h.writes[0].id, 9)

  const trusted = {
    id: 9,
    body: `${marker}\nmaintainer plan`,
    author_association: 'COLLABORATOR',
    user: { login: 'teammate' },
  }
  const team = setup(t, [outsider, trusted])
  const trustedIssue = (await team.invoke('get-issue', { id: '42' })).result
  assert.equal(trustedIssue.planAttachment.id, '9')
  assert.equal(Object.hasOwn(trustedIssue, 'planConflict'), false)
  const updated = await team.invoke('write-plan', {
    issue: '42',
    file: team.file,
    title: 'Implementation plan (GH-42)',
  })
  assert.equal(updated.status, 0, updated.error)
  assert.equal(updated.result.outcome, 'updated')
  assert.equal(team.writes[0].method, 'PATCH')
  assert.equal(team.writes[0].id, 9)
})

test('a write by an untrusted account cannot pass authoritative read-back', async (t) => {
  const h = setup(t, [], (rows) => rows.map((row) => ({ ...row, author_association: 'NONE' })))
  const result = await h.invoke('write-plan', {
    issue: '42',
    file: h.file,
    title: 'Implementation plan (GH-42)',
  })
  assert.equal(result.status, 2)
  assert.match(result.error, /may have applied.*read-back disagrees/)
  assert.equal(result.error.includes(readFileSync(h.file, 'utf8')), false)
})

const specMarker = '<!-- boss-epic-spec v1 -->'
test('epic specs create once, round-trip exactly and coexist with the overview', async (t) => {
  const h = setup(t, [
    { id: 8, body: `${marker}\noverview`, author_association: 'OWNER' },
    { id: 77, body: `${specMarker}\nforged`, author_association: 'NONE' },
  ])
  const args = { issue: 'GH-42', file: h.file, title: 'Epic spec (GH-42)' }
  const created = await h.invoke('write-epic-spec', args)
  assert.equal(created.status, 0, created.error)
  assert.equal(created.result.outcome, 'created')
  assert.equal(h.writes[0].method, 'POST')
  assert.equal(h.writes[0].body, `${specMarker}\n${readFileSync(h.file, 'utf8')}`)
  const unchanged = await h.invoke('write-epic-spec', args)
  assert.equal(unchanged.result.outcome, 'unchanged')
  assert.equal(h.writes.length, 1)
  const issue = (await h.invoke('get-issue', { id: '42' })).result
  assert.equal(issue.planAttachment.id, '8')
  assert.deepEqual(
    issue.attachments.map((row) => row.title),
    ['Implementation plan (GH-42)', 'Epic spec (GH-42)'],
  )
  assert.equal(issue.attachments[1].contentType, 'application/json')
  const read = await h.invoke('read-plan', { id: '9', outFile: h.outFile })
  assert.equal(read.status, 0, read.error)
  assert.deepEqual(readFileSync(h.outFile), readFileSync(h.file))
  writeFileSync(h.file, 'changed spec')
  const changed = await h.invoke('write-epic-spec', args)
  assert.equal(changed.status, 2)
  assert.match(changed.error, /written exactly once.*resume/)
  assert.equal(h.writes.length, 1)
  assert.equal(
    h.calls.some((call) => call.includes('PATCH')),
    false,
  )
})

test('duplicate trusted specs expose ambiguity and refuse writes', async (t) => {
  const h = setup(
    t,
    [9, 10].map((id) => ({ id, body: `${specMarker}\nspec`, author_association: 'MEMBER' })),
  )
  const issue = (await h.invoke('get-issue', { id: '42' })).result
  assert.deepEqual(issue.specConflict, ['9', '10'])
  const { detectEpicParent } = await import('../plan/plan-epic-phase25.mjs')
  assert.equal(detectEpicParent(issue).ambiguous, true)
  const write = await h.invoke('write-epic-spec', {
    issue: '42',
    file: h.file,
    title: 'Epic spec (GH-42)',
  })
  assert.equal(write.status, 2)
  assert.equal(h.writes.length, 0)
})

test('spec digest mismatch and untrusted read-back fail closed', async (t) => {
  for (const readback of [
    (rows) => rows.map((row) => ({ ...row, body: `${specMarker}\nwrong` })),
    (rows) => rows.map((row) => ({ ...row, author_association: 'NONE' })),
  ]) {
    const h = setup(t, [], readback)
    const result = await h.invoke('write-epic-spec', {
      issue: '42',
      file: h.file,
      title: 'Epic spec (GH-42)',
    })
    assert.equal(result.status, 2)
    assert.match(result.error, /may have applied.*read-back disagrees/)
  }
})
