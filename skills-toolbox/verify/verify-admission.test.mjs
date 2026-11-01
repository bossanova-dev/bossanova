import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { verifyAdmissionPolicy } from '../skill-config.mjs'
import {
  admissionFacts,
  admissionLabelHits,
  adoptsVerifyOnly,
  authorMention,
  readRepairFacts,
  admittedLinks,
  admittedTicketless,
  classifyAdmission,
  judgeTier,
  prClass,
  storeAdmission,
  ticketlessReason,
} from './verify-admission.mjs'

const head = (letter) => letter.repeat(40)
const pr = (number, extra = {}) => ({
  number,
  headRefOid: head('a'),
  headRefName: 'dependabot/npm_and_yarn/pkg',
  body: '',
  ...extra,
})
const cron = (number, extra = {}) => pr(number, { headRefName: 'cron-test-123', ...extra })
const ticket = { identifier: 'BOS-1', attachments: [{ url: 'https://github.com/o/r/pull/4' }] }

test('admission classifies the admission seam without widening candidate selection', () => {
  const prs = [
    pr(1),
    cron(2),
    pr(3),
    pr(4),
    pr(5, { body: 'Linear issue: https://linear.app/demo/issue/BOS-2/title' }),
    cron(6),
    pr(7, { isDraft: true }),
    pr(8),
  ]
  const result = classifyAdmission({
    prs,
    linked: [
      { ticket, pr: prs[0] },
      { ticket: null, pr: prs[1] },
    ],
    tickets: [ticket],
    ticketlessSkipped: [
      { pr: 6, reason: 'no-session' },
      { pr: 8, reason: 'ambiguous-pr' },
    ],
    repo: 'o/r',
    admitsTicketless: true,
  })
  assert.deepEqual(
    result.map(({ pr, state, reason }) => ({ pr, state, reason })),
    [
      { pr: 1, state: 'admitted', reason: 'ticket' },
      { pr: 2, state: 'admitted', reason: 'ticketless-factory' },
      { pr: 3, state: 'excluded', reason: 'no-ticket' },
      { pr: 4, state: 'pending', reason: 'attached' },
      { pr: 5, state: 'pending', reason: 'ticket-not-in-review' },
      { pr: 6, state: 'pending', reason: 'no-session' },
      { pr: 7, state: 'pending', reason: 'draft' },
      { pr: 8, state: 'pending', reason: 'ambiguous-pr' },
    ],
  )
  assert.deepEqual(classifyAdmission({ prs: [cron(9)], admitsTicketless: false })[0], {
    pr: 9,
    head: head('a'),
    state: 'excluded',
    reason: 'selection',
  })
  for (const reason of ['session-has-ticket', 'claimed', 'unknown:claims', 'unknown:sessions']) {
    assert.equal(
      classifyAdmission({
        prs: [cron(9)],
        ticketlessSkipped: [{ pr: 9, reason }],
        admitsTicketless: true,
      })[0].state,
      'pending',
    )
  }
})

const at = '2026-10-24T00:00:00.000Z'
const entries = [
  { pr: 3550, head: head('A'), state: 'excluded', reason: 'no-ticket' },
  { pr: 3551, head: head('b'), state: 'admitted', reason: 'ticket' },
  { pr: 3552, head: head('c'), state: 'pending', reason: 'ticket-not-in-review' },
]

test('admission writer agrees with the shared version-one Go fixture', () => {
  const file = join(tempDir('admission-fixture-'), 'verify-admission.json')
  assert.equal(storeAdmission(file, 'default', entries, at), true)
  assert.equal(
    storeAdmission(
      file,
      'secondary',
      [entries[0], { pr: 3553, head: head('d'), state: 'excluded', reason: 'no-ticket' }],
      at,
    ),
    true,
  )
  assert.deepEqual(
    JSON.parse(fs.readFileSync(file, 'utf8')),
    JSON.parse(
      fs.readFileSync(
        new URL(
          '../../services/bossd/internal/verifyadmission/testdata/verify-admission.v1.json',
          import.meta.url,
        ),
        'utf8',
      ),
    ),
  )
})

test('admission writer replaces full scans and merges only the event PR in its scope', () => {
  const file = join(tempDir('admission-merge-'), 'verify-admission.json')
  storeAdmission(file, 'other', entries, at)
  storeAdmission(file, 'scope', entries, at)
  storeAdmission(
    file,
    'scope',
    [
      { ...entries[0], state: 'admitted' },
      { ...entries[1], state: 'excluded' },
    ],
    at,
    { eventPr: 3550 },
  )
  let data = JSON.parse(fs.readFileSync(file, 'utf8'))
  assert.equal(data.scopes.scope.prs[3550].state, 'admitted')
  assert.equal(data.scopes.scope.prs[3551].state, 'admitted')
  assert.equal(data.scopes.other.prs[3550].state, 'excluded')
  storeAdmission(file, 'scope', [entries[2]], at)
  data = JSON.parse(fs.readFileSync(file, 'utf8'))
  assert.deepEqual(Object.keys(data.scopes.scope.prs), ['3552'])
  assert.equal(Object.keys(data.scopes.other.prs).length, 3)
})

test('admission writer treats corrupt and other-version snapshots as empty and failures as false', () => {
  const dir = tempDir('admission-invalid-')
  const file = join(dir, 'snapshot.json')
  for (const content of ['bad json', JSON.stringify({ version: 2, scopes: { stale: {} } })]) {
    fs.writeFileSync(file, content)
    assert.equal(storeAdmission(file, 'scope', [], at), true)
    assert.deepEqual(Object.keys(JSON.parse(fs.readFileSync(file, 'utf8')).scopes), ['scope'])
  }
  assert.equal(storeAdmission(join(file, 'blocked'), 'scope', [], at), false)
  assert.equal(
    storeAdmission(file, 'scope', [], at, {
      fs: {
        ...fs,
        renameSync() {
          throw new Error('EACCES')
        },
      },
    }),
    false,
  )
  assert.deepEqual(fs.readdirSync(dir), ['snapshot.json'])
})

test('GitHub ticket lines keep admission pending and prevent ticketless factory routing', async () => {
  for (const label of ['Linear issue', 'Tracker issue']) {
    const p = pr(3, { body: `${label}: https://github.com/acme/tasks/issues/3` })
    assert.deepEqual(classifyAdmission({ prs: [p] })[0], {
      pr: 3,
      head: head('a'),
      state: 'pending',
      reason: 'ticket-not-in-review',
    })
    const unexpectedRead = () => {
      throw new Error('ticket line must settle routing')
    }
    assert.equal(
      await ticketlessReason(cron(3, { body: p.body }), {
        claimed: unexpectedRead,
        sessions: unexpectedRead,
      }),
      'not-factory',
    )
  }
})

const policyOf = (admission) => verifyAdmissionPolicy({ verifyDefaults: { admission } })
const human = (number, extra = {}) =>
  pr(number, {
    headRefName: `feat/${number}`,
    author: { login: 'dev', __typename: 'User' },
    ...extra,
  })
const answer = (result, number) => {
  const hit = result.find((entry) => entry.pr === number)
  return hit && `${hit.state}/${hit.reason}`
}

test('prClass names the trust class of a PR', () => {
  const ticketLine = 'Linear issue: https://linear.app/demo/issue/BOS-2/title'
  for (const [name, p, opts, expected] of [
    ['fork', human(1, { isCrossRepository: true, body: ticketLine }), { linked: true }, 'fork'],
    ['ticket line', human(1, { body: ticketLine }), {}, 'boss'],
    ['cron branch', cron(1), {}, 'boss'],
    ['linked', human(1), { linked: true }, 'boss'],
    ['GraphQL Bot', human(1, { author: { login: 'dependabot', __typename: 'Bot' } }), {}, 'bot'],
    ['gh is_bot', human(1, { author: { login: 'app/dependabot', is_bot: true } }), {}, 'bot'],
    ['[bot] login', human(1, { author: { login: 'renovate[bot]' } }), {}, 'bot'],
    ['app/ login', human(1, { author: { login: 'app/renovate' } }), {}, 'bot'],
    ['person', human(1), {}, 'human'],
    ['no author', human(1, { author: null }), {}, 'human'],
  ])
    assert.equal(prClass(p, opts), expected, name)
})

test('classifyAdmission with no policy ignores labels and author and keeps its answers', () => {
  const prs = [
    human(1, { labels: ['boss-verify'] }),
    human(2, { labels: ['boss-no-verify'] }),
    cron(3, { labels: ['boss-no-verify'] }),
    human(4, { isCrossRepository: true, labels: ['boss-verify'] }),
  ]
  const input = { prs, linked: [{ ticket: null, pr: prs[2] }], admitsTicketless: true }
  const bare = classifyAdmission({
    ...input,
    prs: prs.map(({ labels, author, isCrossRepository, ...rest }) => rest),
  })
  assert.deepEqual(classifyAdmission({ ...input, policy: null }), bare)
  assert.deepEqual(
    bare.map((entry) => entry.reason),
    ['no-ticket', 'no-ticket', 'ticketless-factory', 'no-ticket'],
  )
})

test('classifyAdmission admits by class and label under a policy', () => {
  const policy = policyOf({ human: { admit: true } })
  const prs = [
    human(1),
    human(2, { author: { login: 'dependabot', __typename: 'Bot' } }),
    human(3, { author: { login: 'renovate[bot]' }, labels: ['Boss-Verify'] }),
    human(4, {
      body: 'Linear issue: https://linear.app/demo/issue/BOS-4/title',
      labels: ['boss-no-verify'],
    }),
    human(5, { isDraft: true }),
    human(6, { isCrossRepository: true }),
    human(7, { isCrossRepository: true, labels: ['boss-verify'] }),
    human(8, { labels: ['boss-no-verify'] }),
    cron(9, { labels: ['unrelated'] }),
  ]
  const result = classifyAdmission({
    prs,
    linked: [
      { ticket: { identifier: 'BOS-4' }, pr: prs[3] },
      { ticket: null, pr: prs[8] },
    ],
    admitsTicketless: true,
    policy,
  })
  assert.deepEqual(
    prs.map((p) => answer(result, p.number)),
    [
      'admitted/policy:human',
      'excluded/not-admitted:bot',
      'admitted/policy:bot',
      'excluded/opted-out',
      'pending/draft',
      'excluded/not-admitted:fork',
      'admitted/policy:fork',
      'excluded/opted-out',
      'admitted/ticketless-factory',
    ],
  )
  // A fork admits only by its own switch or the opt-in label.
  const forks = classifyAdmission({
    prs: [prs[5]],
    admitsTicketless: true,
    policy: policyOf({ human: { admit: true }, fork: { admit: true } }),
  })
  assert.equal(answer(forks, 6), 'admitted/policy:fork')
  // A ticket-scoped or include-filtered scan takes no ticketless PR, whatever the policy says.
  const scoped = classifyAdmission({ prs, admitsTicketless: false, policy })
  assert.deepEqual(
    [1, 3, 7].map((n) => answer(scoped, n)),
    ['excluded/selection', 'excluded/selection', 'excluded/selection'],
  )
  assert.equal(answer(scoped, 2), 'excluded/not-admitted:bot')
  // Configured label names replace the defaults.
  const renamed = classifyAdmission({
    prs: [human(1, { labels: ['boss-verify'] }), human(2, { labels: ['verify-me'] })],
    admitsTicketless: true,
    policy: policyOf({ labels: { optIn: 'verify-me' } }),
  })
  assert.deepEqual(
    [answer(renamed, 1), answer(renamed, 2)],
    ['excluded/not-admitted:human', 'admitted/policy:human'],
  )
})

test('admittedLinks drops opted-out links and adds policy-admitted PRs with their class', () => {
  const policy = policyOf({ human: { admit: true }, bot: { admit: true } })
  const prs = [
    human(1, {
      body: 'Linear issue: https://linear.app/demo/issue/BOS-1/title',
      labels: ['boss-no-verify'],
    }),
    human(2),
    human(3, { url: 'https://github.com/o/r/pull/3' }),
    human(4, { author: { login: 'app/dependabot', is_bot: true } }),
    cron(5),
  ]
  const linked = [
    { ticket: { identifier: 'BOS-1' }, pr: prs[0] },
    { ticket: null, pr: prs[4] },
  ]
  const admission = classifyAdmission({ prs, linked, admitsTicketless: true, policy })
  const out = admittedLinks({
    linked,
    admission,
    prs,
    excluded: new Set(['https://github.com/o/r/pull/3']),
  })
  assert.deepEqual(
    out.linked.map((link) => [link.pr.number, link.ticket, link.class]),
    [
      [2, null, 'human'],
      [4, null, 'bot'],
      [5, null, undefined],
    ],
  )
  assert.deepEqual(out.optedOut, [{ ticket: 'BOS-1', pr: 1, reason: 'opted-out' }])
  // No policy: the links pass through untouched.
  const plain = classifyAdmission({ prs, linked, admitsTicketless: true })
  assert.deepEqual(admittedLinks({ linked, admission: plain, prs }).linked, [linked[0], linked[1]])
})

test('admittedTicketless and admissionFacts answer only under a policy', async () => {
  const policy = policyOf({ human: { admit: true } })
  const claims = (set) => async () => set
  assert.equal(
    await admittedTicketless(human(2), { claimed: claims(new Set()), policy: null }),
    false,
  )
  assert.equal(await admittedTicketless(human(2), { claimed: claims(new Set()), policy }), true)
  assert.equal(await admittedTicketless(human(2), { claimed: claims(new Set([2])), policy }), false)
  assert.equal(await admittedTicketless(human(2), { claimed: claims(null), policy }), false)
  assert.equal(await admittedTicketless(cron(2), { claimed: claims(new Set()), policy }), false)
  assert.equal(admissionFacts(human(2, { labels: ['boss-verify'] }), null), undefined)
  assert.deepEqual(
    admissionFacts(human(2, { labels: ['boss-merge-on-pass', 'x', 'boss-verify'] }), policy),
    { class: 'human', labels: ['mergeOnPass', 'optIn'] },
  )
})

test("judgeTier: no base policy keeps today's rules; a policy tiers each class by admission, labels and merge", () => {
  const human = pr(7, { headRefName: 'feature/x', author: { login: 'dev', __typename: 'User' } })
  const bot = { ...human, author: { login: 'app/dependabot', is_bot: true } }
  const fork = { ...human, isCrossRepository: true }
  const boss = { ...human, headRefName: 'cron-build-1' }
  const policy = (admission) => verifyAdmissionPolicy({ verifyDefaults: { admission } })
  const labelled = (p, ...labels) => ({ ...p, labels: labels.map((name) => ({ name })) })
  assert.equal(judgeTier(human, null), null)
  const open = policy({ human: { admit: true }, bot: { admit: false, merge: 'auto' } })
  const tier = (p, at = open) => {
    const t = judgeTier(p, at)
    return `${t.class}/${t.admitted}/${t.review}/${t.merge}`
  }
  const cases = [
    [boss, open, 'boss/true/receipt/auto'],
    [human, open, 'human/true/verify/human'],
    [labelled(human, 'boss-merge-on-pass'), open, 'human/true/verify/human'],
    [
      labelled(human, 'BOSS-MERGE-ON-PASS'),
      policy({ human: { admit: true, merge: 'auto' } }),
      'human/true/verify/auto',
    ],
    [human, policy({ human: { admit: true, merge: 'auto' } }), 'human/true/verify/human'],
    [labelled(human, 'boss-no-verify'), open, 'human/false/receipt/human'],
    [human, policy({}), 'human/false/receipt/human'],
    [bot, open, 'bot/false/receipt/human'],
    [labelled(bot, 'boss-no-verify', 'boss-merge-on-pass'), open, 'bot/false/receipt/auto'],
    [labelled(bot, 'boss-verify'), open, 'bot/true/verify/human'],
    [labelled(bot, 'boss-verify', 'boss-merge-on-pass'), open, 'bot/true/verify/auto'],
    [fork, open, 'fork/false/none/human'],
    [labelled(fork, 'boss-verify', 'boss-merge-on-pass'), open, 'fork/true/none/human'],
    [fork, policy({ fork: { admit: true } }), 'fork/true/none/human'],
  ]
  for (const [p, at, want] of cases) assert.equal(tier(p, at), want, want)
})

test('off-review links preserve duplicate ambiguity before applying scope', async () => {
  const { offReviewLinks } = await import('./verify-admission.mjs')
  const body = (id) => `Tracker issue: https://linear.app/demo/issue/BOS-${id}/title`
  const duplicates = [pr(3, { body: body(3) }), pr(4, { body: body(3) })]
  const calls = []
  const probe = async (ticket) => {
    calls.push(ticket.id)
    return { available: true, labels: ['agent-build'] }
  }
  const scoped = await offReviewLinks([...duplicates, pr(8, { body: body(8) })], {
    included: (p) => p.number === 3,
    probe,
  })
  assert.deepEqual(scoped, {
    linked: [],
    skipped: [
      { ticket: 'BOS-3', pr: 3, reason: 'ambiguous-pr' },
      { ticket: 'BOS-3', pr: 4, reason: 'ambiguous-pr' },
    ],
    tickets: [],
  })
  assert.deepEqual(await offReviewLinks(duplicates, { included: () => true, probe }), scoped)
  assert.deepEqual(
    await offReviewLinks([...duplicates, pr(8, { body: body(8) })], {
      included: () => false,
      probe,
    }),
    { linked: [], skipped: [], tickets: [] },
  )
  assert.deepEqual(calls, [])
})

test('off-review links retain label rules, visible skips and avoid unnecessary probes', async () => {
  const { offReviewLinks } = await import('./verify-admission.mjs')
  const body = (id) => `Tracker issue: https://linear.app/demo/issue/BOS-${id}/title`
  const calls = []
  const result = await offReviewLinks(
    [
      pr(1, { body: body(1) }),
      pr(2, { body: body(2), isDraft: true }),
      pr(3, { body: body(3) }),
      pr(4, { body: body(3) }),
      pr(5, { body: body(5) }),
      pr(6, { body: body(6) }),
      pr(7, { body: body(7) }),
      pr(8, { body: body(8) }),
      pr(9, { body: body(9) }),
    ],
    {
      tickets: [],
      repo: 'o/r',
      included: (p) => p.number !== 8,
      rules: { selection: { labels: { exclude: ['excluded'] } }, requireLabels: ['agent-build'] },
      probe: async (ticket) => {
        calls.push(ticket.id)
        if (ticket.id === 'BOS-5') throw new Error('offline')
        if (ticket.id === 'BOS-9') return { available: false }
        return {
          available: true,
          labels:
            ticket.id === 'BOS-6'
              ? []
              : ticket.id === 'BOS-7'
                ? ['agent-build', 'excluded']
                : ['agent-build'],
        }
      },
    },
  )
  assert.deepEqual(
    result.linked.map(({ ticket, pr }) => [ticket.identifier, pr.number]),
    [['BOS-1', 1]],
  )
  assert.deepEqual(
    result.skipped.map(({ pr, reason }) => [pr, reason]).sort((a, b) => a[0] - b[0]),
    [
      [2, 'draft'],
      [3, 'ambiguous-pr'],
      [4, 'ambiguous-pr'],
      [5, 'unknown:ticket'],
      [6, 'ticket-not-selected'],
      [7, 'ticket-not-selected'],
      [9, 'unknown:ticket'],
    ],
  )
  assert.deepEqual(calls.sort(), ['BOS-1', 'BOS-5', 'BOS-6', 'BOS-7', 'BOS-9'])
  assert.equal(result.tickets.find((t) => t.identifier === 'BOS-5').labels, null)
})

test('off-review links distinguish configured person labels only on selection misses', async () => {
  const { offReviewLinks } = await import('./verify-admission.mjs')
  const labels = [
    ['question-for-person'],
    ['person-needed'],
    ['other-label'],
    ['agent-question'],
    ['agent-build', 'person-needed'],
  ]
  const prs = labels.map((_, index) =>
    pr(index + 1, { body: `Tracker issue: https://linear.app/demo/issue/ABC-${index + 1}/title` }),
  )
  const result = await offReviewLinks(prs, {
    included: () => true,
    rules: { selection: null, requireLabels: ['agent-build'], excludeLabels: [] },
    personLabels: ['question-for-person', 'person-needed'],
    probe: async (ticket) => ({
      available: true,
      labels: labels[Number(ticket.id.split('-')[1]) - 1],
    }),
  })
  assert.deepEqual(result.skipped, [
    { ticket: 'ABC-1', pr: 1, reason: 'ticket-needs-person' },
    { ticket: 'ABC-2', pr: 2, reason: 'ticket-needs-person' },
    { ticket: 'ABC-3', pr: 3, reason: 'ticket-not-selected', rule: 'labels' },
    { ticket: 'ABC-4', pr: 4, reason: 'ticket-not-selected', rule: 'labels' },
  ])
  assert.deepEqual(
    result.linked.map(({ pr }) => pr.number),
    [5],
  )
})

test('judged off-review links override selection only after a known, unambiguous probe', async () => {
  const { offReviewLinks } = await import('./verify-admission.mjs')
  const prs = Array.from({ length: 7 }, (_, i) =>
    pr(i + 1, { body: `Tracker issue: https://linear.app/demo/issue/BOS-${i + 1}/title` }),
  )
  prs[3].isDraft = true
  const probe = async ({ id }) => ({
    available: id !== 'BOS-3',
    labels: id === 'BOS-2' ? ['needs-human'] : id === 'BOS-7' ? ['agent-build'] : ['bug'],
  })
  const options = {
    included: () => true,
    probe,
    rules: { selection: null, requireLabels: ['agent-build'], excludeLabels: [] },
    personLabels: ['needs-human'],
  }
  for (const judged of [undefined, () => false, () => true]) {
    const result = await offReviewLinks([...prs, { ...prs[4], number: 8 }], { ...options, judged })
    const overrides = judged?.(prs[0]) === true
    assert.deepEqual(
      result.linked.map(({ pr, judged }) => [pr.number, judged]),
      overrides
        ? [
            [1, true],
            [2, true],
            [6, true],
            [7, undefined],
          ]
        : [[7, undefined]],
    )
    assert.deepEqual(
      result.skipped.map(({ pr, reason }) => [pr, reason]),
      [
        ...(overrides
          ? []
          : [
              [1, 'ticket-not-selected'],
              [2, 'ticket-needs-person'],
            ]),
        [3, 'unknown:ticket'],
        [4, 'draft'],
        [5, 'ambiguous-pr'],
        [8, 'ambiguous-pr'],
        ...(overrides ? [] : [[6, 'ticket-not-selected']]),
      ],
    )
    assert.deepEqual(
      result.tickets.map(({ identifier }) => identifier),
      ['BOS-1', 'BOS-2', 'BOS-3', 'BOS-6', 'BOS-7'],
    )
  }
})

test('judged admission persists its reason while ordinary links remain ticket admissions', () => {
  const prs = [pr(1), pr(2)]
  const entries = classifyAdmission({
    prs,
    linked: [
      { ticket, pr: prs[0], judged: true },
      { ticket, pr: prs[1] },
    ],
  })
  assert.deepEqual(
    entries.map(({ state, reason }) => ({ state, reason })),
    [
      { state: 'admitted', reason: 'judged' },
      { state: 'admitted', reason: 'ticket' },
    ],
  )
  const file = join(tempDir('admission-judged-'), 'verify-admission.json')
  assert.equal(storeAdmission(file, 'default', entries, at), true)
  assert.equal(JSON.parse(fs.readFileSync(file, 'utf8')).scopes.default.prs['1'].reason, 'judged')
})

test('closing issue refs accept closing keywords, supported URLs, ids and numeric references', async () => {
  const { closingIssueRefs } = await import('./verify-admission.mjs')
  for (const keyword of [
    'close',
    'closes',
    'closed',
    'fix',
    'fixes',
    'fixed',
    'resolve',
    'resolves',
    'resolved',
  ]) {
    assert.deepEqual(
      closingIssueRefs(
        `${keyword.toUpperCase()} https://tracker.example/issue/ABC-1/title\n${keyword} ABC-2\n${keyword} #3`,
      ),
      [
        { id: 'ABC-1', url: 'https://tracker.example/issue/ABC-1/title' },
        { id: 'ABC-2' },
        { id: 'GH-3' },
      ],
    )
  }
  assert.deepEqual(
    closingIssueRefs('Mentions ABC-1 https://tracker.example/issue/ABC-2 and #3'),
    [],
  )
  assert.deepEqual(
    closingIssueRefs('Fixes ABC-1\nCloses abc-1\nFixes https://github.com/o/r/issues/2'),
    [{ id: 'ABC-1' }, { id: 'GH-2', url: 'https://github.com/o/r/issues/2' }],
  )
})

test('adoption disables repair for non-boss classes unless explicitly opted in', () => {
  for (const cls of ['human', 'bot', 'fork', 'boss', undefined]) {
    for (const repairOptIn of [false, true]) {
      assert.equal(
        adoptsVerifyOnly(cls, repairOptIn),
        cls === 'fork' || (['human', 'bot'].includes(cls) && !repairOptIn),
      )
    }
  }
})

test('author mentions only safe human logins', () => {
  assert.equal(authorMention(pr(1, { author: { login: 'alice' } })), '@alice')
  for (const author of [
    { login: 'alice', __typename: 'Bot' },
    { login: 'app/x' },
    { login: 'x[bot]' },
    {},
    ...['a b', '`x`', 'x]', '-alice', 'a'.repeat(40)].map((login) => ({ login })),
  ]) {
    assert.equal(authorMention(pr(1, { author })), '')
  }
  for (const extra of [
    { isCrossRepository: true },
    { headRefName: 'cron-verify-123' },
    { body: 'Tracker issue: https://linear.app/x/issue/BOS-1/title' },
  ]) {
    assert.equal(authorMention(pr(1, { author: { login: 'alice' }, ...extra })), '')
  }
})

test('repair facts respect the session verify-only fact and older daemon fields', () => {
  for (const is_verify_only of [true, false, undefined]) {
    const read = (_cmd, args) => ({
      ok: true,
      payload:
        args[0] === 'ls'
          ? { sessions: [{ id: 's', pr_url: 'url' }] }
          : { session: { repo_can_auto_repair: true, is_verify_only } },
    })
    assert.deepEqual(readRepairFacts(read, 'url'), {
      autoRepair: is_verify_only !== true,
      verifyOnly: is_verify_only === true,
      attempts: 0,
    })
  }
  assert.deepEqual(admissionLabelHits(pr(1, { labels: ['BOSS-REPAIR'] }), policyOf({})), ['repair'])
})

test('off-review links match the resolved selection and name the failed rule (#7006)', async () => {
  const { offReviewLinks } = await import('./verify-admission.mjs')
  const rules = {
    selection: { assignees: { include: ['me'] } },
    requireLabels: ['agent-build'],
    excludeLabels: [],
  }
  const assignee = { 'BOS-1': 'usr-me', 'BOS-2': 'usr-other' }
  let resolutions = 0
  const result = await offReviewLinks(
    [1, 2].map((n) => pr(n, { body: `Tracker issue: https://linear.app/d/issue/BOS-${n}/t` })),
    {
      included: () => true,
      rules,
      resolveRules: async () => {
        resolutions += 1
        return { ...rules, selection: { assignees: { include: ['usr-me'], exclude: [] } } }
      },
      probe: async ({ id }) => ({
        available: true,
        labels: ['agent-build'],
        assigneeId: assignee[id],
      }),
    },
  )
  assert.deepEqual(
    result.linked.map(({ ticket, pr }) => [ticket.identifier, ticket.assigneeId, pr.number]),
    [['BOS-1', 'usr-me', 1]],
  )
  assert.deepEqual(result.skipped, [
    { ticket: 'BOS-2', pr: 2, reason: 'ticket-not-selected', rule: 'assignees' },
  ])
  assert.equal(resolutions, 1, 'the selection resolves once per scan')
})

test('off-review links skip an unresolvable selection as unknown, never a hold', async () => {
  const { offReviewLinks } = await import('./verify-admission.mjs')
  const prs = [1, 2].map((n) =>
    pr(n, { body: `Tracker issue: https://linear.app/d/issue/BOS-${n}/t` }),
  )
  const rules = { selection: { creators: { exclude: ['bot@x.io'] } } }
  const probe = async () => ({ available: true, labels: [], creatorId: 'usr-1' })
  for (const resolveRules of [
    async () => {
      throw new Error('tracker offline')
    },
    async () => null,
    undefined,
  ]) {
    const result = await offReviewLinks(prs, {
      included: () => true,
      rules,
      resolveRules,
      probe,
      judged: (p) => p.number === 2,
    })
    assert.deepEqual(result.skipped, [{ ticket: 'BOS-1', pr: 1, reason: 'unknown:selection' }])
    assert.deepEqual(
      result.linked.map(({ pr, judged }) => [pr.number, judged]),
      [[2, true]],
    )
  }
})

test('off-review links never resolve a selection without a relation slot', async () => {
  const { offReviewLinks } = await import('./verify-admission.mjs')
  const prs = [pr(1, { body: 'Tracker issue: https://linear.app/d/issue/BOS-1/t' })]
  for (const selection of [null, { labels: { include: ['agent-build'] } }]) {
    const result = await offReviewLinks(prs, {
      included: () => true,
      rules: { selection },
      resolveRules: async () => assert.fail('resolution is not needed'),
      probe: async () => ({ available: true, labels: ['agent-build'] }),
    })
    assert.deepEqual(
      result.linked.map(({ pr }) => pr.number),
      [1],
    )
  }
})

test('the admission entry carries and persists the rule only when present', () => {
  const prs = [1, 2, 3].map((n) =>
    pr(n, { body: `Tracker issue: https://linear.app/d/issue/BOS-${n}/t` }),
  )
  const entries = classifyAdmission({
    prs,
    ticketlessSkipped: [
      { ticket: 'BOS-1', pr: 1, reason: 'ticket-not-selected', rule: 'assignees' },
      { ticket: 'BOS-2', pr: 2, reason: 'ticket-needs-person' },
    ],
  })
  assert.deepEqual(
    entries.map(({ reason, rule }) => [reason, rule]),
    [
      ['ticket-not-selected', 'assignees'],
      ['ticket-needs-person', undefined],
      ['ticket-not-in-review', undefined],
    ],
  )
  assert.equal('rule' in entries[1], false)
  const file = join(tempDir('admission-rule-'), 'verify-admission.json')
  assert.equal(storeAdmission(file, 'default', entries, at), true)
  const stored = JSON.parse(fs.readFileSync(file, 'utf8')).scopes.default.prs
  assert.deepEqual(stored['1'], {
    head: head('a'),
    state: 'pending',
    reason: 'ticket-not-selected',
    rule: 'assignees',
  })
  assert.deepEqual(stored['2'], {
    head: head('a'),
    state: 'pending',
    reason: 'ticket-needs-person',
  })
  storeAdmission(file, 'default', [{ ...entries[2], rule: '' }], at)
  assert.equal('rule' in JSON.parse(fs.readFileSync(file, 'utf8')).scopes.default.prs['3'], false)
})

test('trackerProbe forwards the relation ids the read carries and leaves absent ones absent', async () => {
  const { trackerProbe } = await import('./verify-admission.mjs')
  const adapter = (issue) => ({ applyIssueWrites: async () => ({ outcome: 'ok', issue }) })
  const full = await trackerProbe(
    adapter({ labels: ['a'], assigneeId: 'u1', creatorId: null, projectId: 'p1' }),
    { id: 'BOS-1' },
  )
  assert.deepEqual(full, {
    available: true,
    labels: ['a'],
    assigneeId: 'u1',
    creatorId: null,
    projectId: 'p1',
  })
  const github = await trackerProbe(
    adapter({ labels: ['a'], assigneeId: undefined, creatorId: 'alice' }),
    { id: 'GH-1' },
  )
  assert.deepEqual(github, { available: true, labels: ['a'], creatorId: 'alice' })
  assert.equal('assigneeId' in github, false)
  assert.deepEqual(await trackerProbe({}, { id: 'BOS-1' }), {
    available: false,
    why: 'no-capability',
  })
})

test('resolvedRules resolves the selection through the adapter, or answers null', async () => {
  const { resolvedRules } = await import('./verify-admission.mjs')
  const query = { state: 'In Review', selection: { assignees: { include: ['me'] } } }
  const adapter = { resolveSelection: async () => ({ assignees: { include: ['u1'] } }) }
  assert.deepEqual(await resolvedRules(adapter, query), {
    state: 'In Review',
    selection: { assignees: { include: ['u1'] } },
  })
  assert.equal(await resolvedRules({}, query), null)
  assert.equal(await resolvedRules({ resolveSelection: async () => null }, query), null)
})

test('skipReasonText names the rule only when there is one', async () => {
  const { skipReasonText } = await import('./verify-admission.mjs')
  assert.equal(
    skipReasonText({ reason: 'ticket-not-selected', rule: 'assignees' }),
    'ticket-not-selected (assignees)',
  )
  assert.equal(skipReasonText({ reason: 'draft' }), 'draft')
  assert.equal(skipReasonText({ reason: 'draft', rule: '' }), 'draft')
})
