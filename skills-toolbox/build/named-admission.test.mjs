import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { APPROVAL_MARKER, decideAdmission, humanReasons, runCli } from './named-admission.mjs'
import { DEFAULT_CONFIG, mergeConfig } from '../skill-config.mjs'

const CLI = fileURLToPath(new URL('./named-admission.mjs', import.meta.url))

/** A config whose sole tracker block carries the given labels and optional selection. */
const configWith = ({ labels = {}, selection } = {}) =>
  mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { planned: 'Planned', unplanned: 'Unplanned', inReview: 'In Review' },
        labels: {
          agentBuild: 'agent-build',
          needsHuman: 'needs-human',
          agentQuestion: 'agent-question',
          ...labels,
        },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })

const DESCRIPTION = [
  '## Summary',
  '',
  'Make the thing.',
  '',
  '## Why this needs a human',
  '',
  '- Two valid designs; the operator should pick.',
  '  continuation line that is dropped',
  '- Touches billing.',
  '',
  '## Open Questions',
  '',
  '- Which design?',
  '',
  '## Original notes',
  '',
  '- not a reason',
].join('\n')

// The BOS-2080 shape: planned needs-human, labelled agent-question, no build label.
const QUESTION_ONLY = {
  id: 'uuid-2080',
  identifier: 'ABC-2080',
  labels: [{ name: 'agent-question' }],
  description: DESCRIPTION,
}

const issue = (labels, extra = {}) => ({
  id: 'uuid-1',
  identifier: 'ABC-1',
  labels: { nodes: labels.map((name) => ({ name })) },
  description: DESCRIPTION,
  ...extra,
})

/** Run the CLI in-process against an issue written to a temp file. */
async function run(t, verb, issueValue, { flags = [], ...deps } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'named-admission-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const file = join(dir, 'issue.json')
  writeFileSync(file, typeof issueValue === 'string' ? issueValue : JSON.stringify(issueValue))
  return runCli([verb, file, ...flags], { config: configWith(), env: {}, ...deps })
}

/** A fake adapter that records every applyIssueWrites call and answers with `outcome`. */
function fakeAdapter(outcome = 'ok', extra = {}) {
  const calls = []
  return {
    calls,
    async applyIssueWrites(writes) {
      calls.push(writes)
      return { ok: outcome === 'ok', applied: [], outcome, ...extra }
    },
  }
}

test('check admits a ticket carrying the build label and no needs-human, and writes nothing', async (t) => {
  const adapter = fakeAdapter()
  const r = await run(t, 'check', issue(['agent-build', 'agent-question']), { adapter })
  assert.deepEqual(r, { code: 0, out: { route: 'admissible' } })
  const applied = await run(t, 'apply', issue(['agent-build']), { adapter })
  assert.deepEqual(applied, { code: 0, out: { route: 'admissible' } })
  assert.equal(adapter.calls.length, 0)
})

test('check refuses a ticket missing the build label, naming the ticket, label, reason and remedies', async (t) => {
  const r = await run(t, 'check', QUESTION_ONLY)
  assert.equal(r.code, 3)
  assert.equal(r.out.route, 'refuse')
  assert.deepEqual(r.out.missing, ['agent-build'])
  assert.deepEqual(r.out.present, [])
  assert.deepEqual(r.out.humanReasons, [
    'Two valid designs; the operator should pick.',
    'Touches billing.',
  ])
  for (const part of [
    'ABC-2080',
    'missing "agent-build"',
    '"Two valid designs; the operator should pick."',
    'label it "agent-build"',
    're-run with --approve',
  ]) {
    assert.ok(r.out.reason.includes(part), `${part} in ${r.out.reason}`)
  }
})

test('check refuses a ticket carrying needs-human even with the build label', async (t) => {
  const r = await run(t, 'check', issue(['agent-build', 'needs-human']))
  assert.equal(r.code, 3)
  assert.deepEqual([r.out.route, r.out.missing, r.out.present], ['refuse', [], ['needs-human']])
  assert.match(r.out.reason, /ABC-1: .*labelled "needs-human".*and remove "needs-human"/)
})

test('--approve returns the minimal label deltas and never removes agent-question', async (t) => {
  const cases = [
    [QUESTION_ONLY, { issueId: 'uuid-2080', addLabels: ['agent-build'], removeLabels: [] }],
    [
      issue(['agent-build', 'needs-human']),
      { issueId: 'uuid-1', addLabels: [], removeLabels: ['needs-human'] },
    ],
    [
      issue(['needs-human', 'agent-question']),
      { issueId: 'uuid-1', addLabels: ['agent-build'], removeLabels: ['needs-human'] },
    ],
  ]
  for (const [value, writes] of cases) {
    const r = await run(t, 'check', value, { flags: ['--approve'] })
    assert.equal(r.code, 0)
    assert.equal(r.out.route, 'approve')
    assert.deepEqual(r.out.writes, writes)
    assert.ok(!r.out.writes.removeLabels.includes('agent-question'))
  }
})

test('custom label role names are honoured in the decision and the writes', () => {
  const config = configWith({ labels: { agentBuild: 'ready-for-agent', needsHuman: 'hold' } })
  assert.deepEqual(decideAdmission({ config, issue: issue(['ready-for-agent']) }), {
    route: 'admissible',
  })
  // The stock names mean nothing under a renamed config.
  assert.equal(decideAdmission({ config, issue: issue(['agent-build']) }).route, 'refuse')
  const approved = decideAdmission({ config, issue: issue(['hold', 'needs-human']), approve: true })
  assert.deepEqual(approved.writes, {
    issueId: 'uuid-1',
    addLabels: ['ready-for-agent'],
    removeLabels: ['hold'],
  })
})

test("an issue verify's selection.labels.exclude excludes is refused even with --approve", () => {
  const config = configWith({
    selection: { stages: { verify: { labels: { exclude: ['website'] } } } },
  })
  for (const labels of [['website'], ['agent-build', 'website']]) {
    const r = decideAdmission({ config, issue: issue(labels), approve: true })
    assert.equal(r.route, 'refuse', JSON.stringify(labels))
    assert.match(r.reason, /ABC-1: verify's label selection would not admit it .*"website"/)
    assert.match(r.reason, /"agent-build" alone cannot fix that/)
  }
  // A selection exclude that does not match leaves the approve route open.
  assert.equal(decideAdmission({ config, issue: issue([]), approve: true }).route, 'approve')
})

test('humanReasons reads the needs-human section, falls back to open questions, else is empty', () => {
  const config = configWith()
  assert.deepEqual(humanReasons(config, DESCRIPTION), [
    'Two valid designs; the operator should pick.',
    'Touches billing.',
  ])
  const questionsOnly =
    '## Summary\n\nx\n\n## Open Questions\n\n1. Which design?\n\n## Original notes\n'
  assert.deepEqual(humanReasons(config, questionsOnly), ['Which design?'])
  assert.deepEqual(humanReasons(config, '## Summary\n\nx\n\n## Original notes\n\n- notes\n'), [])
  assert.deepEqual(humanReasons(config, undefined), [])
})

test('apply writes the deltas once with a comment quoting the reasons and the fixed marker', async (t) => {
  const adapter = fakeAdapter('ok')
  const r = await run(t, 'apply', QUESTION_ONLY, { adapter, env: { BOSS_SESSION_ID: 'sess-9' } })
  assert.equal(r.code, 0)
  assert.equal(r.out.outcome, 'ok')
  assert.equal(adapter.calls.length, 1)
  const [call] = adapter.calls
  assert.deepEqual(
    { issueId: call.issueId, addLabels: call.addLabels, removeLabels: call.removeLabels },
    { issueId: 'uuid-2080', addLabels: ['agent-build'], removeLabels: [] },
  )
  assert.equal(call.marker, APPROVAL_MARKER)
  for (const part of [
    'Two valid designs; the operator should pick.',
    'Touches billing.',
    'agent-build',
    'sess-9',
  ])
    assert.ok(call.comment.includes(part), part)
})

test('apply fails closed on a missing capability, failed or indeterminate outcome, or a throw', async (t) => {
  const missing = await run(t, 'apply', QUESTION_ONLY, { adapter: {} })
  assert.equal(missing.code, 69)
  assert.match(missing.out.reason, /applyIssueWrites/)
  const unresolved = await run(t, 'apply', QUESTION_ONLY, { adapter: null })
  assert.equal(unresolved.code, 69)

  for (const outcome of ['failed', 'indeterminate']) {
    const adapter = fakeAdapter(outcome, { reason: `writes ${outcome} upstream` })
    const r = await run(t, 'apply', QUESTION_ONLY, { adapter })
    assert.equal(r.code, 69, outcome)
    assert.equal(r.out.reason, `writes ${outcome} upstream`)
    assert.equal(adapter.calls.length, 1, `${outcome} is never retried`)
  }

  const throwing = {
    async applyIssueWrites() {
      throw new Error('credentials missing')
    },
  }
  const thrown = await run(t, 'apply', QUESTION_ONLY, { adapter: throwing })
  assert.deepEqual([thrown.code, thrown.out.reason], [69, 'credentials missing'])
})

test('apply on a refused ticket exits 3 without writing', async (t) => {
  const config = configWith({
    selection: { stages: { verify: { labels: { exclude: ['website'] } } } },
  })
  const adapter = fakeAdapter()
  const r = await run(t, 'apply', issue(['website']), { adapter, config })
  assert.equal(r.code, 3)
  assert.equal(adapter.calls.length, 0)
})

test('usage errors exit 64 and unreadable input exits 69, each with a reason', async (t) => {
  for (const argv of [
    [],
    ['check'],
    ['nope', 'x.json'],
    ['check', 'a', 'b'],
    ['check', 'x.json', '--bogus'],
  ]) {
    const r = await runCli(argv, { config: configWith() })
    assert.equal(r.code, 64, JSON.stringify(argv))
    assert.match(r.out.reason, /usage: named-admission\.mjs/)
  }
  const bad = await run(t, 'check', '{not json')
  assert.equal(bad.code, 69)
  assert.match(bad.out.reason, /issue json/)
  const noLabels = await run(t, 'check', { identifier: 'ABC-1' })
  assert.deepEqual(
    [noLabels.code, noLabels.out.reason],
    [69, 'issue: labels are absent or unreadable'],
  )
})

test('the CLI loads the repo config from its cwd and prints one JSON line', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'named-admission-cli-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  writeFileSync(
    join(dir, '.boss-skills.json'),
    JSON.stringify({
      adapters: { tracker: 'linear' },
      trackerConfig: { linear: { team: 'Demo', labels: { agentBuild: 'ready-for-agent' } } },
    }),
  )
  const file = join(dir, 'issue.json')
  writeFileSync(file, JSON.stringify(QUESTION_ONLY))
  const refused = spawnSync(process.execPath, [CLI, 'check', file], { cwd: dir, encoding: 'utf8' })
  assert.equal(refused.status, 3, refused.stderr)
  const out = JSON.parse(refused.stdout)
  assert.deepEqual(out.missing, ['ready-for-agent'])
  const approved = spawnSync(process.execPath, [CLI, 'check', file, '--approve'], {
    cwd: dir,
    encoding: 'utf8',
  })
  assert.equal(approved.status, 0, approved.stderr)
  assert.deepEqual(JSON.parse(approved.stdout).writes.addLabels, ['ready-for-agent'])
  const usage = spawnSync(process.execPath, [CLI], { cwd: dir, encoding: 'utf8' })
  assert.equal(usage.status, 64)
})
