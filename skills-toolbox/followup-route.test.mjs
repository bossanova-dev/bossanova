import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import {
  FOLLOWUP_SOURCES,
  main,
  ROUTES,
  renderFollowUpSections,
  routeFollowUps,
} from './followup-route.mjs'
import { sections } from './merge-eligibility.mjs'
import { addTrigger } from './notes-record.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))

// A PR body around the rendered sections, shaped like the one boss-build publishes.
function bodyWith(rendered) {
  return [
    'Issue: https://tracker.example/ISSUE-1',
    '',
    rendered,
    '## Review coverage',
    'full',
    '',
  ].join('\n')
}

// Stage-owned evidence, a host credential problem, and a plan open question whose chosen option
// the build already implemented: none of them is a person's to act on.
const PARKED_PR_ITEMS = [
  { text: 'Publish or review the live-drive evidence for this change.', source: 'stage-evidence' },
  {
    text: 'Re-authenticate the review CLI on the build host (token refresh returned 401).',
    source: 'host',
  },
  {
    text: 'Should the fast path wait for the evidence comment?',
    source: 'plan-question',
    chosen: 'do not wait; the verify stage owns evidence',
    alternative: 'wait for the comment before merging',
  },
]

function memoryFs() {
  const files = new Map()
  const absent = () => Object.assign(new Error('not found'), { code: 'ENOENT' })
  return {
    files,
    writeFileSync: (p, text) => files.set(p, String(text)),
    appendFileSync: (p, text) => files.set(p, (files.get(p) || '') + text),
    readFileSync: (p) => {
      if (!files.has(p)) throw absent()
      return files.get(p)
    },
  }
}

function runMain(argv, memFs = memoryFs()) {
  const out = []
  const err = []
  const code = main(argv, {
    fs: memFs,
    stdout: (t) => out.push(t),
    stderr: (t) => err.push(t),
  })
  return { code, out: out.join(''), err: err.join(''), fs: memFs }
}

test('vocabulary is frozen and closed', () => {
  assert.deepEqual(Object.keys(FOLLOWUP_SOURCES).sort(), [
    'host',
    'outside-worktree',
    'plan-question',
    'post-merge',
    'review-finding',
    'stage-evidence',
  ])
  assert.ok(Object.isFrozen(FOLLOWUP_SOURCES))
  for (const description of Object.values(FOLLOWUP_SOURCES))
    assert.equal(typeof description, 'string')
})

test('items no person acts on route to no human follow-up or question', () => {
  const routed = routeFollowUps(PARKED_PR_ITEMS)
  assert.deepEqual(routed.humanFollowUp, [])
  assert.deepEqual(routed.questions, [])
  assert.equal(routed.hostNotes.length, 1)
  assert.equal(routed.decisions.length, 2)
  assert.deepEqual(routed.warnings, [])

  const parsed = sections(bodyWith(renderFollowUpSections(routed)))
  assert.equal(parsed.has('human follow-up'), false)
  assert.equal(parsed.has('open questions'), false)
  assert.ok(parsed.has('host notes'))
})

test('stage evidence names its stage, defaulting to verify', () => {
  const routed = routeFollowUps([
    { text: 'Capture the demo', source: 'stage-evidence' },
    { text: 'Record the release smoke', source: 'stage-evidence', stage: 'release' },
  ])
  assert.match(routed.decisions[0], /evidence left to the `verify` stage/)
  assert.match(routed.decisions[1], /evidence left to the `release` stage/)
})

test('a resolved plan question records the question, the choice and the alternative', () => {
  const [decision] = routeFollowUps([PARKED_PR_ITEMS[2]]).decisions
  assert.ok(decision.includes(PARKED_PR_ITEMS[2].text))
  assert.ok(decision.includes(PARKED_PR_ITEMS[2].chosen))
  assert.ok(decision.includes(PARKED_PR_ITEMS[2].alternative))
})

test('outside-worktree criteria are listed under ## Human follow-up', () => {
  const candidates = [
    {
      text: 'Confirm the dashboard loads against production data',
      source: 'outside-worktree',
      criterion: 'dashboard renders live totals',
    },
  ]
  const routed = routeFollowUps(candidates)
  assert.equal(routed.humanFollowUp.length, 1)
  assert.deepEqual(routed.warnings, [])
  assert.deepEqual(sections(bodyWith(renderFollowUpSections(routed))).get('human follow-up'), [
    '- [ ] Confirm the dashboard loads against production data (criterion: dashboard renders live totals)',
    '',
  ])
})

test('a post-merge observation is listed under ## After merge', () => {
  const candidates = [
    {
      text: 'Observe the nightly build run on its configured schedule',
      source: 'post-merge',
      criterion: 'scheduled build cadence',
    },
  ]
  const routed = routeFollowUps(candidates)
  assert.deepEqual(routed.humanFollowUp, [])
  assert.deepEqual(routed.afterMerge, [
    'Observe the nightly build run on its configured schedule (criterion: scheduled build cadence)',
  ])
  assert.deepEqual(routed.warnings, [])
  const parsed = sections(bodyWith(renderFollowUpSections(routed)))
  assert.deepEqual(parsed.get('after merge').filter(Boolean), [
    '- [ ] Observe the nightly build run on its configured schedule (criterion: scheduled build cadence)',
  ])
  assert.equal(parsed.has('human follow-up'), false)
  assert.equal(
    sections(bodyWith(renderFollowUpSections(routeFollowUps([])))).has('after merge'),
    false,
  )
})

test('a pre-existing review finding is listed for a ticket', () => {
  const candidates = [{ text: 'P1 a.go:3 — nil map write: m is nil', source: 'review-finding' }]
  const routed = routeFollowUps(candidates)
  assert.deepEqual(routed.reviewFollowUps, ['P1 a.go:3 — nil map write: m is nil'])
  assert.deepEqual(routed.humanFollowUp, [])
  assert.deepEqual(routed.warnings, [])
  assert.ok(sections(bodyWith(renderFollowUpSections(routed))).has('review follow-ups'))
})

test('outside-worktree without a criterion is still listed, with a warning', () => {
  const routed = routeFollowUps([{ text: 'Check the deployed env', source: 'outside-worktree' }])
  assert.deepEqual(routed.humanFollowUp, ['Check the deployed env'])
  assert.equal(routed.warnings.length, 1)
})

test('missing or unknown source defaults to ## Human follow-up with a warning', () => {
  for (const candidate of [
    { text: 'Something unclassified' },
    { text: 'Something unclassified', source: 'weather' },
    'Something unclassified',
  ]) {
    const routed = routeFollowUps([candidate])
    assert.deepEqual(routed.humanFollowUp, ['Something unclassified'])
    assert.equal(routed.warnings.length, 1)
  }
})

test('a plan question without a chosen option is returned to decide or ask, never published', () => {
  const routed = routeFollowUps([{ text: 'Which cache to use?', source: 'plan-question' }])
  assert.deepEqual(routed.questions, ['Which cache to use?'])
  assert.deepEqual(routed.decisions, [])
  assert.equal(routed.warnings.length, 1)
  assert.doesNotMatch(renderFollowUpSections(routed), /cache/)
})

test('empty text is dropped with a warning', () => {
  for (const candidate of [{ text: '   ', source: 'host' }, { source: 'host' }, null, 42]) {
    const routed = routeFollowUps([candidate])
    assert.deepEqual(routed.humanFollowUp, [])
    assert.deepEqual(routed.hostNotes, [])
    assert.equal(routed.warnings.length, 1)
  }
})

test('text is collapsed to one line and a leading checklist marker is stripped', () => {
  const routed = routeFollowUps([{ text: '- [ ] Verify\n  the\r\nrollout ', source: 'bogus' }])
  assert.deepEqual(routed.humanFollowUp, ['Verify the rollout'])
  const injected = renderFollowUpSections(
    routeFollowUps([{ text: '- [ ] a\n## Open questions\n- none', source: 'x' }]),
  )
  assert.equal(sections(injected).has('open questions'), false)
})

test('source spelling is normalised, not rejected', () => {
  const routed = routeFollowUps([
    { text: 'token expired', source: ' HOST ' },
    { text: 'q?', source: 'Plan_Question', chosen: 'a' },
  ])
  assert.equal(routed.hostNotes.length, 1)
  assert.equal(routed.decisions.length, 1)
})

test('every declared source has its own route', () => {
  assert.deepEqual(Object.keys(ROUTES).sort(), Object.keys(FOLLOWUP_SOURCES).sort())
})

test('non-array input is normalised', () => {
  assert.equal(routeFollowUps({ text: 'x', source: 'host' }).hostNotes.length, 1)
  assert.equal(routeFollowUps({ candidates: [{ text: 'x', source: 'host' }] }).hostNotes.length, 1)
  const empty = routeFollowUps(undefined)
  assert.deepEqual(empty.humanFollowUp, [])
  assert.equal(empty.warnings.length, 1)
})

test('empty routing renders no section at all', () => {
  assert.equal(renderFollowUpSections(routeFollowUps([])), '')
  assert.equal(renderFollowUpSections(undefined), '')
})

test('host notes render as a plain list with no checkboxes', () => {
  const rendered = renderFollowUpSections(
    routeFollowUps([
      { text: 'token refresh returned 401', source: 'host', where: 'cross-model review' },
      { text: 'linter binary missing', source: 'host' },
    ]),
  )
  const lines = sections(rendered).get('host notes').filter(Boolean)
  assert.deepEqual(lines, [
    '- cross-model review: token refresh returned 401',
    '- linter binary missing',
  ])
})

test('route CLI prints the routed result with sections and writes notes-out', () => {
  const memFs = memoryFs()
  memFs.files.set('/in.json', JSON.stringify(PARKED_PR_ITEMS))
  const { code, out, err } = runMain(
    ['route', '--candidates', '/in.json', '--notes-out', '/notes.json'],
    memFs,
  )
  assert.equal(code, 0)
  assert.equal(err, '')
  const result = JSON.parse(out)
  assert.equal(result.hostNotes.length, 1)
  assert.equal(typeof result.sections, 'string')
  assert.equal(sections(result.sections).has('human follow-up'), false)

  const notes = JSON.parse(memFs.files.get('/notes.json'))
  assert.deepEqual(notes, result.hostNotes)
  for (const note of notes) assert.deepEqual(Object.keys(note).sort(), ['detail', 'where'])
})

test('notes-out feeds notes-record add --from: one buffered line per host note', () => {
  const memFs = memoryFs()
  memFs.files.set(
    '/in.json',
    JSON.stringify([
      { text: 'token refresh returned 401', source: 'host', where: 'cross-model review' },
      { text: 'linter binary missing', source: 'host' },
      { text: 'Check prod', source: 'outside-worktree', criterion: 'prod works' },
    ]),
  )
  assert.equal(
    runMain(['route', '--candidates', '/in.json', '--notes-out', '/notes.json'], memFs).code,
    0,
  )
  const added = addTrigger(
    { from: '/notes.json', trigger: 'host-environment', core: 'boss-build' },
    { fs: memFs, gitDir: () => '/git', env: {}, now: () => new Date('2026-10-12T00:00:00Z') },
  )
  assert.equal(added.ok, true)
  assert.equal(added.buffered, 2)
  const lines = memFs.files.get('/git/boss-notes-record.jsonl').trim().split('\n').map(JSON.parse)
  assert.equal(lines.length, 2)
  assert.deepEqual(
    lines.map((line) => [line.trigger, line.where]),
    [
      ['host-environment', 'cross-model review'],
      ['host-environment', 'linter binary missing'],
    ],
  )
})

test('unreadable candidates fail without rendering any sections', () => {
  for (const setup of [() => {}, (memFs) => memFs.files.set('/in.json', '{not json')]) {
    const memFs = memoryFs()
    setup(memFs)
    const { code, out, err } = runMain(['route', '--candidates', '/in.json'], memFs)
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.match(err, /followup-route:/)
  }
})

test('unwritable notes-out is a warning, not a failure', () => {
  const memFs = memoryFs()
  memFs.files.set('/in.json', JSON.stringify([{ text: 'x', source: 'host' }]))
  memFs.writeFileSync = () => {
    throw new Error('read-only')
  }
  const { code, out } = runMain(
    ['route', '--candidates', '/in.json', '--notes-out', '/n.json'],
    memFs,
  )
  assert.equal(code, 0)
  assert.equal(JSON.parse(out).warnings.length, 1)
})

test('usage errors exit 2: unknown verb, unknown flag, missing candidates', () => {
  for (const argv of [
    [],
    ['classify', '--candidates', '/in.json'],
    ['route', '--candidates', '/in.json', '--bogus', 'x'],
    ['route'],
  ]) {
    const { code, out, err } = runMain(argv)
    assert.equal(code, 2, JSON.stringify(argv))
    assert.equal(out, '')
    assert.match(err, /usage:/)
  }
})

test('the CLI runs as an entry point', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'followup-route-'))
  try {
    const input = path.join(dir, 'in.json')
    fs.writeFileSync(input, JSON.stringify(PARKED_PR_ITEMS))
    const result = spawnSync(
      process.execPath,
      [path.join(HERE, 'followup-route.mjs'), 'route', '--candidates', input],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
    assert.equal(JSON.parse(result.stdout).decisions.length, 2)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})
