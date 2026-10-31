import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  decideVerdict,
  aggregateExtensions,
  resolveEffectiveVerdict,
  post,
} from './verify-gate.mjs'
import { buildBlockers, encodeBlockerTrailer, encodeOutcomesTrailer } from './verify-blockers.mjs'
const facts = (extra = {}) => ({
  prState: 'OPEN',
  isDraft: false,
  headSha: 'a'.repeat(40),
  receipt: 'success',
  ci: { settled: true, green: true },
  extensions: [],
  ...extra,
})
const finding = {
  id: 'f1',
  severity: 'P2',
  verified: true,
  introduced: true,
  fixed: false,
  file: 'services/bosso/api/orchestrator/agentaccounts/credentials.go',
  line: 198,
  title: 'Synchronous peer acknowledgements create reciprocal publication waits',
  evidence: 'Peer publication can wait reciprocally.',
}
test('standing P2 review findings send the head back with all blockers before parks or extensions', () => {
  const result = decideVerdict(
    facts({
      reviewFindings: [finding],
      receipt: 'absent',
      prClass: 'boss',
      eligibilityReasons: ['merge-state-dirty'],
      ci: { settled: false },
      alwaysHumanHits: ['a'],
      extensions: [{ name: 'proof' }],
    }),
  )
  assert.equal(result.verdict, 'blocked')
  assert.deepEqual(
    result.blockers.map((b) => b.kind),
    ['review-finding', 'conflict'],
  )
  assert.equal(result.blockers[0].severity, 'P2')
  assert.equal(result.blockers[0].line, 198)
  assert.equal(
    decideVerdict(facts({ reviewFindings: [finding], receipt: 'unknown' })).reason,
    'receipt-unknown',
  )
})
test('settled red CI blocks while unsettled or unreadable evidence waits', () => {
  assert.equal(
    decideVerdict(
      facts({
        ci: { settled: true, green: false, state: 'failing' },
        failedChecks: [{ name: 'ci' }],
      }),
    ).blockers[0].kind,
    'check',
  )
  assert.equal(
    decideVerdict(facts({ ci: { settled: true, green: false, state: 'unknown' } })).reason,
    'ci-not-green-on-head',
  )
})
test('notes and excluded findings do not block; an unreceipted PARTIAL first needs review', () => {
  for (const changed of [
    { severity: 'P3' },
    { category: 'style' },
    { introduced: false },
    { verified: false },
    { fixed: true },
  ])
    assert.equal(
      decideVerdict(facts({ reviewFindings: [{ ...finding, ...changed }] })).verdict,
      'pass',
    )
  assert.equal(
    decideVerdict(
      facts({
        reviewFindings: [finding],
        receipt: 'absent',
        prClass: 'boss',
        eligibilityReasons: ['partial-marker'],
      }),
    ).reason,
    'receipt-absent',
  )
  assert.equal(
    resolveEffectiveVerdict({ verdict: 'blocked', repairAttempts: 5 }).reason,
    'repair-exhausted',
  )
})
test('extension notes are surfaced without producing a defect', () => {
  const ext = { name: 'proof' }
  const note = { title: 'Polish', severity: 'P3' }
  const result = aggregateExtensions(
    [
      {
        extension: 'proof',
        result: {
          ok: true,
          extension: 'proof',
          role: 'verify',
          verdict: 'fail',
          evidence: [],
          findings: [note],
        },
      },
    ],
    [ext],
  )
  assert.equal(result.verdict, 'pass')
  assert.equal(result.findings.length, 0)
  assert.equal(result.findingNotes.length, 1)
})

test('round cap parks only blocked verdicts and preserves the legacy attempt cap', () => {
  assert.deepEqual(
    resolveEffectiveVerdict({
      verdict: 'blocked',
      reason: 'blockers',
      repairAttempts: null,
      repairRounds: 2,
    }),
    { effective: 'human', reason: 'repair-exhausted' },
  )
  for (const rounds of [1, null])
    assert.equal(
      resolveEffectiveVerdict({ verdict: 'blocked', repairAttempts: null, repairRounds: rounds })
        .effective,
      'blocked',
    )
  assert.equal(
    resolveEffectiveVerdict({ verdict: 'defect', repairAttempts: null, repairRounds: 2 }).effective,
    'defect',
  )
  for (const verdict of ['defect', 'blocked'])
    assert.equal(
      resolveEffectiveVerdict({ verdict, repairAttempts: 5, repairRounds: null }).effective,
      'human',
    )
})

const HEAD = 'a'.repeat(40)
const PR_URL = 'https://github.com/o/r/pull/7'
const blockerFile = (head) => ({
  version: 1,
  pr: 7,
  head,
  blockers: buildBlockers({
    reviewFindings: [{ ...finding, severity: 'P1' }],
    failedChecks: [{ name: 'integration', text: 'database unavailable' }],
  }),
})
function roundComments(count, { sameHead = false } = {}) {
  const comments = []
  for (let round = 0; round < count; round++) {
    const head = String(sameHead ? 1 : round + 1).repeat(40)
    const file = blockerFile(head)
    const outcomes = {
      version: 1,
      blockersHead: head,
      pushedHead: String(round + 2).repeat(40),
      outcomes: [
        {
          id: file.blockers[0].id,
          outcome: 'not-fixable',
          reason: 'Requires an operator credential',
        },
      ],
    }
    for (const body of [encodeBlockerTrailer(file), encodeOutcomesTrailer(outcomes)])
      comments.push({
        id: comments.length + 1,
        body,
        created_at: new Date(Date.UTC(2026, 9, 1, 0, comments.length)).toISOString(),
        author_association: 'OWNER',
        user: { login: 'operator' },
      })
  }
  return comments
}
// Only the post path's IO surface is faked; marker decoding, permissions, round counting and note
// rendering all run through their production implementations.
function postWorld(comments, { unreadable = false, permission = 'admin' } = {}) {
  const statuses = [],
    commentBodies = [],
    trackerWrites = [],
    reads = []
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const read = (command, args) => {
    reads.push({ command, args })
    if (command === 'boss' && args[0] === 'ls')
      return ok({ sessions: [{ id: 'session', pr_url: PR_URL }] })
    if (command === 'boss' && args[0] === 'show')
      return ok({ session: { repo_can_auto_repair: true, last_repair: null } })
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view')
      return ok({
        headRefOid: HEAD,
        url: PR_URL,
        title: 'Repair bounded loop',
        body: 'Linear issue: https://linear.app/acme/issue/BOS-1/title',
        baseRefName: 'main',
      })
    if (command === 'gh' && args[0] === 'api') {
      const path = args[1]
      if (path.includes('/commits/') && path.includes('/statuses')) return ok([[]])
      if (path.includes('/comments?'))
        return unreadable ? { ok: false, exit: 1, output: '' } : ok([comments])
      if (path.includes('/permission')) return ok({ permission })
      if (path === 'repos/o/r') return ok({ owner: { login: 'o', type: 'Organization' } })
      // An unread base tip leaves the park's review request to the owner.
      if (path === 'repos/o/r/commits/main') return { ok: false, exit: 1, output: '' }
      if (path === '--method' && args[2] === 'POST') {
        const fields = Object.fromEntries(
          args
            .filter((arg) => /^(body|state|description)=/.test(arg))
            .map((arg) => {
              const at = arg.indexOf('=')
              return [arg.slice(0, at), arg.slice(at + 1)]
            }),
        )
        if (args[3].includes('/statuses/')) statuses.push(fields)
        else if (args[3].endsWith('/comments')) commentBodies.push(fields.body)
        else throw new Error('Unexpected POST: ' + args[3])
        return ok({ html_url: PR_URL + '#issuecomment-99' })
      }
    }
    throw new Error('Unexpected read: ' + command + ' ' + args.join(' '))
  }
  return {
    statuses,
    commentBodies,
    trackerWrites,
    reads,
    deps: {
      read,
      now: () => Date.UTC(2026, 9, 2),
      loadConfig: () => ({}),
      adapter: {
        applyIssueWrites: async (writes) => {
          if (writes.addLabels) trackerWrites.push(writes)
          return { outcome: 'ok', issue: { labels: [] } }
        },
      },
    },
  }
}
async function postBlocked(t, world) {
  const dir = mkdtempSync(join(tmpdir(), 'verify-round-post-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  const path = join(dir, 'blockers.json')
  writeFileSync(path, JSON.stringify(blockerFile(HEAD)))
  return post({ pr: 7, head: HEAD, verdict: 'blocked', blockers: path, repo: 'o/r' }, world.deps)
}
test('two repair rounds park the blocked head with outstanding evidence and a tracker handoff', async (t) => {
  const w = postWorld(roundComments(2))
  const result = await postBlocked(t, w)
  assert.equal(result.verdict, 'human')
  assert.equal(result.reason, 'repair-exhausted')
  assert.equal(result.posted, true)
  assert.deepEqual(w.statuses, [{ state: 'success', description: 'needs human: repair-exhausted' }])
  assert.equal(result.reviewRequest, 'skipped:no-reviewer')
  assert.equal(w.trackerWrites.length, 1)
  assert.deepEqual(w.trackerWrites[0].addLabels, ['needs-human'])
  assert.equal(w.commentBodies.length, 1)
  const note = w.commentBodies[0]
  assert.match(note, /not fixable: Requires an operator credential/)
  assert.match(note, /P1.*credentials\.go:198/)
  assert.match(note, /Synchronous peer acknowledgements create reciprocal publication waits/)
  assert.match(note, /integration/)
  assert.doesNotMatch(note, /boss-verify-blockers/)
  assert.equal(w.reads.filter(({ args }) => args[1]?.includes('/comments?')).length, 1)
})
test('one repair round still sends blockers back with failure', async (t) => {
  const w = postWorld(roundComments(1))
  assert.equal((await postBlocked(t, w)).verdict, 'blocked')
  assert.equal(w.statuses[0].state, 'failure')
  assert.match(w.commentBodies[0], /boss-verify-blockers/)
  assert.equal(w.trackerWrites.length, 0)
})
test('unreadable marker history never invents a repair-round park', async (t) => {
  const w = postWorld(roundComments(2), { unreadable: true })
  assert.equal((await postBlocked(t, w)).verdict, 'blocked')
  assert.equal(w.statuses[0].state, 'failure')
  assert.match(w.commentBodies[0], /boss-verify-blockers/)
})
test('anchor-associated commenters without repository write permission cannot spend repair rounds', async (t) => {
  const w = postWorld(roundComments(2), { permission: 'read' })
  assert.equal((await postBlocked(t, w)).verdict, 'blocked')
  assert.equal(w.statuses[0].state, 'failure')
  assert.equal(w.trackerWrites.length, 0)
})

test('boss heads with or without tiers review absent receipts and accept unusable-review approval', () => {
  for (const tier of [null, { class: 'boss', review: 'receipt', merge: 'auto' }]) {
    const extra = { prClass: 'boss', tier, receipt: 'absent' }
    const result = decideVerdict(facts(extra))
    assert.deepEqual([result.verdict, result.reason], ['review-required', 'receipt-absent'])
    const approved = decideVerdict(facts({ ...extra, waive: 'extension-failed' }))
    assert.deepEqual([approved.verdict, approved.reason], ['pass', 'approved'])
    for (const [receipt, reason] of [
      ['pending', 'receipt-pending'],
      ['unknown', 'receipt-unknown'],
    ]) {
      const waiting = decideVerdict(facts({ ...extra, receipt }))
      assert.deepEqual([waiting.verdict, waiting.reason], ['wait', reason])
    }
    const unsettled = decideVerdict(facts({ ...extra, ci: { settled: false, green: false } }))
    assert.deepEqual([unsettled.verdict, unsettled.reason], ['wait', 'ci-not-settled'])
  }
})

test('repeated not-fixable current blockers exhaust repair below the round cap', () => {
  const options = {
    verdict: 'blocked',
    reason: 'blockers',
    repairAttempts: null,
    repairRounds: 1,
    repeatNotFixable: ['check:ci'],
    blockers: [{ id: 'check:ci' }],
  }
  assert.deepEqual(resolveEffectiveVerdict(options), {
    effective: 'human',
    reason: 'repair-exhausted',
  })
  assert.equal(
    resolveEffectiveVerdict({ ...options, blockers: [{ id: 'check:other' }] }).effective,
    'blocked',
  )
  assert.equal(resolveEffectiveVerdict({ ...options, verdict: 'pass' }).effective, 'pass')
})
test('same-head repeated not-fixable sends park with the blocker reason before the round cap', async (t) => {
  const comments = roundComments(2, { sameHead: true })
  const w = postWorld(comments)
  const result = await postBlocked(t, w)
  assert.deepEqual([result.verdict, result.reason], ['human', 'repair-exhausted'])
  assert.match(w.commentBodies[0], /not fixable twice: Requires an operator credential/)
})
