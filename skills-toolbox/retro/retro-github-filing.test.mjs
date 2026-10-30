import assert from 'node:assert/strict'
import test from 'node:test'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { buildLinearOperationMap } from '../tracker/linear.mjs'
import { sourceNotesTitle } from './retro-notes.mjs'

const toolbox = fileURLToPath(new URL('../', import.meta.url))
test('GitHub retro files linked issues, verifies source notes and marks enforced promises', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'retro-github-filing-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const state = path.join(dir, 'state.json')
  fs.writeFileSync(state, JSON.stringify({ issues: [], comments: [] }))
  fs.writeFileSync(
    path.join(dir, '.boss-skills.json'),
    JSON.stringify({
      adapters: { tracker: 'github' },
      trackerConfig: {
        github: {
          repository: 'acme/tasks',
          states: { unplanned: 'boss:unplanned' },
          labels: { epic: 'epic', agentPlan: 'agent-plan' },
        },
      },
    }),
  )
  fs.writeFileSync(
    path.join(dir, 'gh'),
    `#!${process.execPath}
const fs = require('node:fs')
const args = process.argv.slice(2), stateFile = process.env.RETRO_TEST_STATE
const state = JSON.parse(fs.readFileSync(stateFile, 'utf8'))
const endpoint = args[1], inputAt = args.indexOf('--input')
const input = inputAt < 0 ? null : JSON.parse(fs.readFileSync(args[inputAt + 1], 'utf8'))
const issueNumber = Number(endpoint?.match(/issues\\/(\\d+)/)?.[1])
const issue = state.issues.find(row => row.number === issueNumber)
let out
if (args[0] === 'label') out = ['epic', 'agent-plan', 'boss:unplanned'].map(name => ({name}))
else if (args[0] === 'issue' && args[1] === 'edit') {
  const row = state.issues.find(row => row.number === Number(args[2]))
  row.labels.push({name: args[args.indexOf('--add-label') + 1]}); out = row
} else if (args[0] === 'issue' && args[1] === 'view') out = state.issues.find(row => row.number === Number(args[2]))
else if (endpoint === 'repos/acme/tasks/issues' && input) {
  const number = state.issues.length + 1
  out = {id: 100 + number, number, title: input.title, body: input.body, labels: input.labels.map(name => ({name})), state: 'open', author_association: 'MEMBER', html_url: 'https://github.com/acme/tasks/issues/' + number, created_at: '2026-01-01T00:00:00Z', issue_dependencies_summary: {total_blocked_by: 0, total_blocking: 0}}
  state.issues.push(out)
} else if (endpoint === 'repos/acme/tasks/issues') out = state.issues
else if (endpoint.endsWith('/sub_issues')) {
  if (input) {
    out = state.issues.find(row => row.id === input.sub_issue_id)
    out.parent_issue_url = 'https://api.github.com/repos/acme/tasks/issues/' + issueNumber
  } else out = [state.issues.filter(row => row.parent_issue_url?.endsWith('/' + issueNumber))]
} else if (endpoint.endsWith('/comments') && input) {
  out = {id: 900 + state.comments.length, body: input.body, author_association: 'MEMBER', user: {login: 'maintainer'}, issueNumber}
  state.comments.push(out)
} else if (endpoint.endsWith('/comments')) out = [state.comments.filter(row => row.issueNumber === issueNumber)]
else if (endpoint.includes('/issues/comments/')) out = state.comments.find(row => row.id === Number(endpoint.split('/').at(-1)))
else if (/\\/dependencies\\/(blocked_by|blocking)$/.test(endpoint)) out = [[]]
else if (issue) { if (input) issue.body = input.body; out = issue }
else throw new Error('Unexpected fake gh invocation: ' + args.join(' '))
fs.writeFileSync(stateFile, JSON.stringify(state))
process.stdout.write(JSON.stringify(out))
`,
    { mode: 0o700 },
  )
  const invoke = (script, args) => {
    const result = spawnSync(process.execPath, [path.join(toolbox, script), ...args], {
      cwd: dir,
      encoding: 'utf8',
      env: {
        ...process.env,
        TRACKER: 'github',
        PATH: `${dir}:${process.env.PATH}`,
        RETRO_TEST_STATE: state,
      },
    })
    assert.equal(result.status, 0, result.stderr)
    return result.stdout
  }
  const cli = (...args) => JSON.parse(invoke('tracker/cli.mjs', args))
  const save = (name, value) => {
    const file = path.join(dir, name)
    fs.writeFileSync(file, typeof value === 'string' ? value : JSON.stringify(value))
    return file
  }
  const operations = cli('operations')
  assert.equal(
    JSON.parse(invoke('retro/retro-run.mjs', ['storage-route', save('ops.json', operations)]))
      .route,
    'single-record',
  )
  assert.equal(cli('resolve-selection', '--stage', 'retro').stage, 'retro')
  const marked = path.join(dir, 'marked.json')
  cli('fetch-marked', '--prefix', 'Notes: ', '--out-file', marked)
  assert.deepEqual(JSON.parse(fs.readFileSync(marked)), [])
  const body = save(
    'body.md',
    'Notes-promise: theme-key rung=guard round=1 instances=0\nNotes: theme-key\n',
  )
  const parent = cli(
    'create-issue',
    '--title',
    'Retro',
    '--body-file',
    body,
    '--state-role',
    'unplanned',
    '--label-role',
    'epic',
  ).result
  const child = cli(
    'create-issue',
    '--title',
    'Fix theme',
    '--body-file',
    body,
    '--state-role',
    'unplanned',
    '--label-role',
    'agentPlan',
    '--parent',
    parent.id,
  ).result
  assert.equal(child.parentId, parent.id)
  const notes = save('notes.md', 'Résumé\r\n日本語 source notes\n')
  const written = cli(
    'invoke-operation',
    'writeSourceNotes',
    '--args-file',
    save('write.json', { issue: child.id, file: notes, title: sourceNotesTitle(child.id) }),
  ).result
  const issue = cli(
    'invoke-operation',
    'getIssue',
    '--args-file',
    save('get.json', { id: child.id }),
  ).result
  const presence = JSON.parse(
    invoke('retro/retro-notes.mjs', [
      'attachments',
      save('attachments.json', { [child.id]: issue.attachments }),
    ]),
  )
  assert.deepEqual(presence.present, [child.id])
  const outFile = path.join(dir, 'readback.md')
  cli(
    'invoke-operation',
    'readPlanAttachment',
    '--args-file',
    save('read.json', { id: written.id, outFile }),
  )
  assert.deepEqual(fs.readFileSync(outFile), fs.readFileSync(notes))
  const enforced = invoke('retro/retro-notes.mjs', [
    'mark-enforced',
    body,
    'theme-key',
    '2026-10-29T00:00:00Z',
  ])
  cli('write-description', '--id', child.id, '--body-file', save('enforced.md', enforced))
  assert.equal(
    cli('invoke-operation', 'getIssue', '--args-file', save('get.json', { id: child.id })).result
      .description,
    enforced,
  )
  assert.match(enforced, /Notes-enforced: theme-key 2026-10-29T00:00:00Z/)
  const linearOps = { operationMap: buildLinearOperationMap('linear') }
  assert.equal(
    JSON.parse(invoke('retro/retro-run.mjs', ['storage-route', save('linear.json', linearOps)]))
      .route,
    'attachment',
  )
})
