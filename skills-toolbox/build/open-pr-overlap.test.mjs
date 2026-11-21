import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { decideOpenPrOverlap, runCli } from './open-pr-overlap.mjs'
import { DEFAULT_CONFIG, DEFAULT_TRACKER_STATES, mergeConfig } from '../skill-config.mjs'

const config = mergeConfig(DEFAULT_CONFIG, {
  adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
  trackerConfig: {
    demo: { labels: { agentBuild: 'build', agentPlan: 'plan' }, states: DEFAULT_TRACKER_STATES },
  },
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
  decideOpenPrOverlap({
    config,
    issue,
    planText: '',
    snapshot,
    rootEntries: { dirs: ['services'], files: [], knownFiles: [path] },
    ...overrides,
  })

test('unplanned exact files are recorded and sibling files do not overlap', () => {
  const d = decide()
  assert.equal(d.route, 'evaluated')
  assert.equal(d.overlaps[0].level, 'file')
  assert.equal(d.overlaps[0].planned, false)
  assert.equal(
    decide({
      snapshot: {
        ...snapshot,
        pullRequests: [{ ...pr, files: ['services/mobile/src/Other.tsx'] }],
      },
    }).overlaps.length,
    0,
  )
})

test('only mentions above Original notes count, with whole PR and ticket tokens', () => {
  for (const mention of ['#42', pr.url, 'ABC-2']) {
    const data = { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-2'] }] }
    assert.equal(
      decide({ snapshot: data, planText: `## Risks\n${mention}` }).overlaps.some(
        (p) => p.level === 'file' && !p.planned,
      ),
      false,
    )
    assert.equal(
      decide({
        snapshot: data,
        issue: {
          ...issue,
          description: `${issue.description}\nRelated (non-blocking, file overlap): ${mention}`,
        },
      }).overlaps.some((p) => p.level === 'file' && !p.planned),
      false,
    )
    assert.equal(
      decide({ snapshot: data, planText: `## Original notes\n${mention}` }).overlaps.some(
        (p) => p.level === 'file' && !p.planned,
      ),
      true,
    )
    assert.equal(
      decide({
        snapshot: data,
        issue: { ...issue, description: `${issue.description}\n## Original notes\n${mention}` },
      }).overlaps.some((p) => p.level === 'file' && !p.planned),
      true,
    )
  }
  assert.equal(
    decide({ planText: '#4' }).overlaps.some((p) => p.level === 'file' && !p.planned),
    true,
  )
  assert.equal(
    decide({ planText: '#420' }).overlaps.some((p) => p.level === 'file' && !p.planned),
    true,
  )
})

test('PR URL references match complete URLs, including markdown and prose', () => {
  for (const mention of [`${pr.url}0`, `${pr.url}/files`, `${pr.url}?view=files`])
    assert.equal(
      decide({ planText: mention }).overlaps.some((p) => p.level === 'file' && !p.planned),
      true,
    )
  for (const mention of [pr.url, `[prior PR](${pr.url})`, `<${pr.url}>`, `${pr.url}.`])
    assert.equal(
      decide({ planText: mention }).overlaps.some((p) => p.level === 'file' && !p.planned),
      false,
    )
})

test('incidental inferred IDs cannot substitute for tracker references', () => {
  const data = { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-2', 'UTF-8'] }] }
  assert.equal(
    decide({ snapshot: data, planText: 'Fix UTF-8 handling' }).overlaps.some(
      (p) => p.level === 'file' && !p.planned,
    ),
    true,
  )
  assert.equal(
    decide({ snapshot: data, planText: 'After ABC-2' }).overlaps.some(
      (p) => p.level === 'file' && !p.planned,
    ),
    false,
  )
})

test('uses plan Key changes, excludes own PRs, and reports missing areas and truncation', () => {
  assert.equal(
    decide({ issue: { ...issue, description: '' }, planText: issue.description }).overlaps.some(
      (p) => p.level === 'file' && !p.planned,
    ),
    true,
  )
  assert.equal(
    decide({ ownPr: 42 }).overlaps.some((p) => p.level === 'file' && !p.planned),
    false,
  )
  assert.equal(
    decide({ snapshot: { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-1'] }] } }).overlaps
      .length,
    0,
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
  assert.equal(
    d.overlaps.some((p) => p.level === 'file' && !p.planned),
    false,
  )
  assert.equal(d.warnings.length, 2)
})

async function run(t, flags = [], deps = {}, values = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'open-pr-overlap-'))
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

test('CLI records unplanned file overlaps and succeeds', async (t) => {
  const r = await run(t)
  assert.equal(r.code, 0)
  assert.equal(r.out.route, 'evaluated')
  assert.equal(r.out.overlaps[0].number, 42)
  assert.equal(r.out.overlaps[0].planned, false)
  assert.deepEqual(r.out.overlaps[0].shared, ['services/mobile/src/choice.tsx'])
  assert.equal((await run(t, [], {}, { plan: '#42' })).code, 0)
})

test('stale --apply callers receive a usage error', async (t) => {
  assert.equal((await run(t, ['--apply'])).code, 64)
})

test('unreadable and invalid inputs are not evaluated', async (t) => {
  for (const values of [{ snapshot: '{' }, { snapshot: '{}' }, { issue: '{' }]) {
    const r = await run(t, [], {}, values)
    assert.equal(r.code, 69)
    assert.equal(r.out.route, 'not-evaluated')
    assert.ok(r.out.reason)
  }
  assert.equal((await run(t, ['--open-prs', '/missing/snapshot'])).code, 69)
  assert.equal((await run(t, ['--plan', '/missing/plan'])).code, 69)
  const d = mkdtempSync(join(tmpdir(), 'open-pr-fetch-'))
  t.after(() => rmSync(d, { recursive: true, force: true, maxRetries: 3 }))
  writeFileSync(join(d, 'issue'), JSON.stringify(issue))
  writeFileSync(join(d, 'plan'), '')
  const r = await runCli(['check', '--issue-json', join(d, 'issue'), '--plan', join(d, 'plan')], {
    config,
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
