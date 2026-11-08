import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { decideOpenPrHold, holdComment, runCli } from './open-pr-hold.mjs'
import { DEFAULT_CONFIG, mergeConfig } from '../skill-config.mjs'

const config = mergeConfig(DEFAULT_CONFIG, {
  adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
  trackerConfig: { demo: { labels: { agentBuild: 'build', needsHuman: 'human' } } },
})
const path = 'services/mobile/src/Choice.tsx'
const issue = { id: 'uuid', identifier: 'ABC-1', description: `## Key changes\n- \`${path}\`` }
const pr = {
  number: 42,
  url: 'https://example.test/pull/42',
  title: 'Choice',
  headRefName: 'choice',
  isDraft: true,
  files: [path],
  filesTruncated: false,
  ticketIds: [],
}
const snapshot = { listTruncated: false, pullRequests: [pr] }
const decide = (overrides = {}) =>
  decideOpenPrHold({
    config,
    issue,
    planText: '',
    snapshot,
    rootEntries: { dirs: ['services'], files: [], knownFiles: [path] },
    ...overrides,
  })

test('unplanned exact file holds and sibling files do not', () => {
  const d = decide()
  assert.equal(d.route, 'hold')
  assert.equal(d.overlaps[0].level, 'file')
  assert.equal(d.overlaps[0].planned, false)
  assert.equal(
    decide({
      snapshot: {
        ...snapshot,
        pullRequests: [{ ...pr, files: ['services/mobile/src/Other.tsx'] }],
      },
    }).route,
    'clear',
  )
})

test('only mentions above Original notes count, with whole PR and ticket tokens', () => {
  for (const mention of ['#42', pr.url, 'ABC-2']) {
    const data = { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-2'] }] }
    assert.equal(decide({ snapshot: data, planText: `## Risks\n${mention}` }).route, 'clear')
    assert.equal(
      decide({
        snapshot: data,
        issue: {
          ...issue,
          description: `${issue.description}\nRelated (non-blocking, file overlap): ${mention}`,
        },
      }).route,
      'clear',
    )
    assert.equal(
      decide({ snapshot: data, planText: `## Original notes\n${mention}` }).route,
      'hold',
    )
    assert.equal(
      decide({
        snapshot: data,
        issue: { ...issue, description: `${issue.description}\n## Original notes\n${mention}` },
      }).route,
      'hold',
    )
  }
  assert.equal(decide({ planText: '#4' }).route, 'hold')
  assert.equal(decide({ planText: '#420' }).route, 'hold')
})

test('PR URL references match complete URLs, including markdown and prose', () => {
  for (const mention of [`${pr.url}0`, `${pr.url}/files`, `${pr.url}?view=files`])
    assert.equal(decide({ planText: mention }).route, 'hold')
  for (const mention of [pr.url, `[prior PR](${pr.url})`, `<${pr.url}>`, `${pr.url}.`])
    assert.equal(decide({ planText: mention }).route, 'clear')
})

test('incidental inferred IDs cannot substitute for tracker references', () => {
  const data = { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-2', 'UTF-8'] }] }
  assert.equal(decide({ snapshot: data, planText: 'Fix UTF-8 handling' }).route, 'hold')
  assert.equal(decide({ snapshot: data, planText: 'After ABC-2' }).route, 'clear')
})

test('uses plan Key changes, excludes own PRs, and reports missing areas and truncation', () => {
  assert.equal(
    decide({ issue: { ...issue, description: '' }, planText: issue.description }).route,
    'hold',
  )
  assert.equal(decide({ ownPr: 42 }).route, 'clear')
  assert.equal(
    decide({ snapshot: { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-1'] }] } }).route,
    'clear',
  )
  assert.equal(decide({ issue: { ...issue, description: '' } }).reason, 'no-key-changes')
  assert.equal(
    decide({ issue: { ...issue, description: `## Risks\n${path}` } }).reason,
    'no-key-changes',
  )
  const d = decide({
    snapshot: {
      listTruncated: true,
      pullRequests: [{ ...pr, files: ['other/file.js'], filesTruncated: true }],
    },
  })
  assert.equal(d.route, 'clear')
  assert.equal(d.warnings.length, 2)
})

async function run(t, flags = [], deps = {}, values = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'open-pr-hold-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  for (const [file, value] of Object.entries({
    issue: JSON.stringify(issue),
    plan: '',
    snapshot: JSON.stringify(snapshot),
    ...values,
  }))
    writeFileSync(join(dir, file), value)
  return runCli(
    [
      'check',
      '--issue-json',
      join(dir, 'issue'),
      '--plan',
      join(dir, 'plan'),
      '--open-prs',
      join(dir, 'snapshot'),
      ...flags,
    ],
    { config, cwd: process.cwd(), ...deps },
  )
}

test('CLI holds without writes and clear --apply does not write', async (t) => {
  assert.equal((await run(t)).code, 3)
  const r = await run(
    t,
    ['--apply'],
    {
      adapter: {
        applyIssueWrites() {
          assert.fail('clear must not write')
        },
      },
    },
    { plan: '#42' },
  )
  assert.equal(r.code, 0)
})

test('apply writes once with labels, bounded evidence and stable marker, without a state', async (t) => {
  const calls = []
  const r = await run(t, ['--apply'], {
    adapter: {
      async applyIssueWrites(writes) {
        calls.push(writes)
        return { outcome: 'ok' }
      },
    },
  })
  assert.equal(r.code, 3)
  assert.equal(r.out.outcome, 'ok')
  assert.equal(calls.length, 1)
  assert.deepEqual(calls[0].addLabels, ['human'])
  assert.deepEqual(calls[0].removeLabels, ['build'])
  assert.equal(calls[0].stateName, undefined)
  assert.match(calls[0].marker, /#42/)
  for (const text of ['#42', pr.url, path])
    assert.ok(calls[0].comment.toLowerCase().includes(text.toLowerCase()))
  const many = {
    ...decide(),
    overlaps: Array.from({ length: 7 }, (_, i) => ({
      ...decide().overlaps[0],
      number: i + 1,
      shared: Array.from({ length: 12 }, (_, n) => `file${n}`),
    })),
  }
  assert.match(holdComment(many), /\+2 more/)
  assert.ok(!holdComment(many).includes('file10'))
})

test('changed overlap sets publish fresh comments while identical sets deduplicate', async (t) => {
  const comments = []
  const adapter = {
    async applyIssueWrites(writes) {
      if (!comments.some((body) => body.includes(writes.marker)))
        comments.push(`${writes.comment}\n${writes.marker}`)
      return { outcome: 'ok' }
    },
  }
  for (const numbers of [[12, 13], [12], [1], [12]]) {
    const data = {
      ...snapshot,
      pullRequests: numbers.map((number) => ({
        ...pr,
        number,
        url: `https://example.test/pull/${number}`,
      })),
    }
    const result = await run(t, ['--apply'], { adapter }, { snapshot: JSON.stringify(data) })
    assert.equal(result.code, 3)
  }
  assert.equal(comments.length, 3)
  for (const [index, number] of [12, 12, 1].entries())
    assert.ok(comments[index].includes(`- #${number} https://example.test/pull/${number}:`))
})

test('failed and indeterminate writes are never retried; missing capabilities fail closed', async (t) => {
  for (const outcome of ['failed', 'indeterminate']) {
    let calls = 0
    const r = await run(t, ['--apply'], {
      adapter: {
        async applyIssueWrites() {
          calls++
          return { outcome }
        },
      },
    })
    assert.equal(r.code, 4)
    assert.equal(calls, 1)
    assert.ok(r.out.reason)
  }
  assert.equal((await run(t, ['--apply'], { adapter: {} })).code, 4)
  assert.equal(
    (
      await run(t, ['--apply'], {
        config: mergeConfig(config, { trackerConfig: { demo: { labels: { needsHuman: null } } } }),
      })
    ).code,
    4,
  )
})

test('unreadable and invalid inputs fail open without adapter writes', async (t) => {
  const adapter = {
    applyIssueWrites() {
      assert.fail('must not write')
    },
  }
  for (const values of [{ snapshot: '{' }, { snapshot: '{}' }, { issue: '{' }]) {
    const r = await run(t, ['--apply'], { adapter }, values)
    assert.equal(r.code, 69)
    assert.equal(r.out.route, 'not-evaluated')
    assert.ok(r.out.reason)
  }
  assert.equal((await run(t, ['--open-prs', '/missing/snapshot'], { adapter })).code, 69)
  assert.equal((await run(t, ['--plan', '/missing/plan'], { adapter })).code, 69)
  const d = mkdtempSync(join(tmpdir(), 'open-pr-fetch-'))
  t.after(() => rmSync(d, { recursive: true, force: true, maxRetries: 3 }))
  writeFileSync(join(d, 'issue'), JSON.stringify(issue))
  writeFileSync(join(d, 'plan'), '')
  const r = await runCli(['check', '--issue-json', join(d, 'issue'), '--plan', join(d, 'plan')], {
    config,
    adapter,
    fetch() {
      throw new Error('offline')
    },
  })
  assert.equal(r.code, 69)
  assert.match(r.out.reason, /offline/)
})

test('strict arguments require plan and issue', async () => {
  for (const argv of [
    [],
    ['check'],
    ['check', '--plan', 'x'],
    ['check', '--issue-json', 'x'],
    ['check', '--wat'],
  ])
    assert.equal((await runCli(argv)).code, 64)
})
