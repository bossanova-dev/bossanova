import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import {
  SELECTION_DIMENSIONS,
  SELECTION_STAGES,
  applyResolution,
  assertEffectiveSelection,
  describeEmptyScan,
  effectiveSelection,
  gateSelectionFlags,
  isNarrowed,
  matchIssue,
  parseSelectionFlags,
  renderLinearIssueFilter,
  selectionResolutionRefs,
  splitArgs,
  stageLabelRoles,
  validateSelectionBlock,
} from './selection.mjs'
import { normalizeTicket } from './bs-epic-lib.mjs'
import {
  DEFAULT_CONFIG,
  mergeConfig,
  stageSelectionQuery,
  validateConfig,
} from './skill-config.mjs'

const UUID_A = '2b3c4d5e-1111-4222-8333-444455556666'

/** An effective selection with only the given slots set. */
function sel(slots = {}) {
  return assertEffectiveSelection(slots)
}

// --- the render table -------------------------------------------------------------------------

const RENDER_CASES = [
  [
    'labels.include',
    { labels: { include: ['backend', 'api'] } },
    { labels: { some: { name: { in: ['backend', 'api'] } } } },
  ],
  [
    'labels.exclude',
    { labels: { exclude: ['infra'] } },
    { labels: { every: { name: { nin: ['infra'] } } } },
  ],
  [
    'assignees.include',
    { assignees: { include: ['usr_1'] } },
    { assignee: { id: { in: ['usr_1'] } } },
  ],
  [
    'assignees.exclude (keeps unassigned)',
    { assignees: { exclude: ['usr_1'] } },
    { or: [{ assignee: { null: true } }, { assignee: { id: { nin: ['usr_1'] } } }] },
  ],
  [
    'creators.include',
    { creators: { include: ['usr_2'] } },
    { creator: { id: { in: ['usr_2'] } } },
  ],
  [
    'creators.exclude (keeps creator-less)',
    { creators: { exclude: ['usr_bot'] } },
    { or: [{ creator: { null: true } }, { creator: { id: { nin: ['usr_bot'] } } }] },
  ],
  [
    'projects.include',
    { projects: { include: ['prj_1'] } },
    { project: { id: { in: ['prj_1'] } } },
  ],
  [
    'projects.exclude (keeps project-less)',
    { projects: { exclude: ['prj_1'] } },
    { or: [{ project: { null: true } }, { project: { id: { nin: ['prj_1'] } } }] },
  ],
]

for (const [name, slots, clause] of RENDER_CASES) {
  test(`renderLinearIssueFilter: ${name} renders exactly one AND clause`, () => {
    assert.deepEqual(renderLinearIssueFilter(sel(slots), { state: 'Todo' }), {
      state: { name: { eq: 'Todo' } },
      and: [clause],
    })
  })
}

test('renderLinearIssueFilter: every dimension x polarity is covered by the table', () => {
  const covered = new Set(RENDER_CASES.map(([name]) => name.split(' ')[0]))
  for (const dimension of SELECTION_DIMENSIONS) {
    for (const polarity of ['include', 'exclude']) {
      assert.ok(
        covered.has(`${dimension}.${polarity}`),
        `${dimension}.${polarity} has no render case`,
      )
    }
  }
})

test('renderLinearIssueFilter: no selection and no stage rules renders only state and team', () => {
  assert.deepEqual(renderLinearIssueFilter(sel(), { state: 'Todo', team: 'Core' }), {
    state: { name: { eq: 'Todo' } },
    team: { name: { eq: 'Core' } },
  })
})

test('renderLinearIssueFilter: stage labels are ANDed and needs-human merges with labels.exclude', () => {
  const filter = renderLinearIssueFilter(
    sel({ labels: { include: ['backend'], exclude: ['infra'] } }),
    {
      state: 'Todo',
      requireLabels: ['agent-build'],
      excludeLabels: ['needs-human'],
    },
  )
  assert.deepEqual(filter.and, [
    { labels: { some: { name: { eq: 'agent-build' } } } },
    { labels: { some: { name: { in: ['backend'] } } } },
    { labels: { every: { name: { nin: ['needs-human', 'infra'] } } } },
  ])
})

// --- stage-label AND-ing through the one config derivation ------------------------------------

function configWith(selection) {
  const config = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { planned: 'Planned', unplanned: 'Unplanned' },
        labels: { agentBuild: 'agent-build', agentPlan: 'agent-plan', needsHuman: 'needs-human' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })
  validateConfig(config, 'test')
  return config
}

test('stageSelectionQuery: each stage ANDs its own label and always excludes needs-human', () => {
  const config = configWith(undefined)
  const cases = [
    ['plan', 'Unplanned', ['agent-plan']],
    ['build', 'Planned', ['agent-build']],
    ['epic', 'Planned', []],
    ['verify', null, []],
  ]
  for (const [stage, state, requireLabels] of cases) {
    const query = stageSelectionQuery(config, stage)
    assert.equal(query.state, state, stage)
    assert.deepEqual(query.requireLabels, requireLabels, stage)
    assert.deepEqual(query.excludeLabels, ['needs-human'], stage)
    assert.equal(isNarrowed(query.selection), false, stage)
  }
})

test("stageSelectionQuery: a user's labels.exclude never re-admits needs-human", () => {
  const query = stageSelectionQuery(configWith({ labels: { exclude: ['infra'] } }), 'build')
  assert.deepEqual(query.excludeLabels, ['needs-human'])
  const filter = renderLinearIssueFilter(query.selection, query)
  assert.deepEqual(filter.and.at(-1), {
    labels: { every: { name: { nin: ['needs-human', 'infra'] } } },
  })
})

test('stageLabelRoles: plan and build carry a role; the other stages carry none', () => {
  assert.deepEqual(stageLabelRoles('plan'), ['agentPlan'])
  assert.deepEqual(stageLabelRoles('build'), ['agentBuild'])
  for (const stage of ['epic', 'verify', 'retro', 'release'])
    assert.deepEqual(stageLabelRoles(stage), [])
  assert.throws(() => stageLabelRoles('deploy'), /unknown stage "deploy"/)
})

// --- precedence: flag over stage over shared, per slot ----------------------------------------

const BLOCK = {
  labels: { include: ['shared-in'], exclude: ['shared-out'] },
  creators: { exclude: ['bot@example.com'] },
  stages: {
    build: { labels: { exclude: ['stage-out'] }, projects: { include: ['Stage Project'] } },
  },
}

const PRECEDENCE_CASES = [
  [
    'shared only (another stage)',
    'plan',
    {},
    { 'labels.include': ['shared-in'], 'labels.exclude': ['shared-out'] },
  ],
  [
    'stage replaces its one slot',
    'build',
    {},
    { 'labels.include': ['shared-in'], 'labels.exclude': ['stage-out'] },
  ],
  [
    'flag replaces its one slot over the stage',
    'build',
    { labels: { exclude: ['flag-out'] } },
    { 'labels.include': ['shared-in'], 'labels.exclude': ['flag-out'] },
  ],
  [
    'flag replaces the other polarity only',
    'build',
    { labels: { include: ['flag-in'] } },
    { 'labels.include': ['flag-in'], 'labels.exclude': ['stage-out'] },
  ],
  [
    'stage adds a slot the shared block lacks',
    'build',
    {},
    { 'projects.include': ['Stage Project'], 'creators.exclude': ['bot@example.com'] },
  ],
  [
    'flag over shared for a dimension the stage lacks',
    'build',
    { creators: { exclude: ['me'] } },
    { 'creators.exclude': ['me'] },
  ],
]

for (const [name, stage, flags, expected] of PRECEDENCE_CASES) {
  test(`effectiveSelection precedence: ${name}`, () => {
    const effective = effectiveSelection(BLOCK, stage, flags)
    for (const [slot, values] of Object.entries(expected)) {
      const [dimension, polarity] = slot.split('.')
      assert.deepEqual(effective[dimension][polarity], values, slot)
    }
  })
}

test('effectiveSelection: an empty stage array explicitly clears the shared slot', () => {
  const block = { labels: { include: ['a'] }, stages: { epic: { labels: { include: [] } } } }
  assert.deepEqual(effectiveSelection(block, 'epic').labels.include, [])
  assert.deepEqual(effectiveSelection(block, 'build').labels.include, ['a'])
})

test('effectiveSelection: no block and no flags is the all-empty selection', () => {
  const effective = effectiveSelection(null, 'build')
  assert.equal(isNarrowed(effective), false)
  assert.deepEqual(Object.keys(effective), SELECTION_DIMENSIONS)
})

// --- validation -------------------------------------------------------------------------------

const P = 'trackerConfig.linear.selection'
const VALIDATION_CASES = [
  [
    'an unknown top-level key',
    { label: { include: ['a'] } },
    /has unknown key "label"; allowed keys: labels, assignees, creators, projects, stages/,
  ],
  [
    'an unknown stage',
    { stages: { deploy: {} } },
    /\.stages has unknown stage "deploy"; allowed stages: plan, build, epic, verify, retro, release/,
  ],
  [
    'the legacy assigneeOrCreator key',
    { assigneeOrCreator: 'me' },
    /selection\.assigneeOrCreator was removed; use trackerConfig\.linear\.selection\.assignees\.include and\/or trackerConfig\.linear\.selection\.creators\.include/,
  ],
  [
    'the legacy array-valued labels',
    { labels: ['a', 'b'] },
    /selection\.labels as an array was removed; use trackerConfig\.linear\.selection\.labels: \{include: \[\.\.\.\]\}/,
  ],
  [
    'a legacy array-valued labels inside a stage',
    { stages: { build: { labels: ['a'] } } },
    /stages\.build\.labels as an array was removed; use .*stages\.build\.labels: \{include: \[\.\.\.\]\}/,
  ],
  [
    'an unknown slot key',
    { labels: { only: ['a'] } },
    /labels has unknown key "only"; allowed keys: include, exclude/,
  ],
  [
    'a non-array slot',
    { labels: { include: 'a' } },
    /labels\.include must be an array of non-empty strings/,
  ],
  [
    'an empty string entry',
    { projects: { exclude: [''] } },
    /projects\.exclude entries must be non-empty strings/,
  ],
  [
    'a display name as a user',
    { assignees: { include: ['Dave Perrett'] } },
    /"Dave Perrett" is not a user selector; use me, a user id \(UUID\), or an email/,
  ],
  [
    'a stage nested in a stage',
    { stages: { build: { stages: {} } } },
    /stages\.build has unknown key "stages"/,
  ],
  ['a non-object block', ['a'], /selection must be an object when present/],
]

for (const [name, block, pattern] of VALIDATION_CASES) {
  test(`validateSelectionBlock rejects ${name}`, () => {
    const errors = validateSelectionBlock(block, P)
    assert.ok(errors.length > 0, 'expected an error')
    assert.match(errors.join('\n'), pattern)
  })
}

test('validateSelectionBlock accepts the full schema, every user form, and empty arrays', () => {
  const block = {
    labels: { include: ['backend'], exclude: [] },
    assignees: { include: ['me', UUID_A, 'dev@example.com'], exclude: [] },
    creators: { exclude: ['bot@example.com'] },
    projects: { include: ['Platform', UUID_A] },
    stages: {
      build: { labels: { exclude: ['infra'] } },
      plan: {},
      release: { projects: { exclude: [] } },
    },
  }
  assert.deepEqual(validateSelectionBlock(block, P), [])
})

// --- flag parsing -----------------------------------------------------------------------------

const FLAG_CASES = [
  ['repeatable', ['--label', 'a', '--label', 'b'], { labels: { include: ['a', 'b'] } }, []],
  ['comma-separated', ['--exclude-label', 'a, b'], { labels: { exclude: ['a', 'b'] } }, []],
  [
    'equals form',
    ['--exclude-creator=bot@example.com'],
    { creators: { exclude: ['bot@example.com'] } },
    [],
  ],
  [
    'all eight flags',
    [
      '--label',
      'l',
      '--exclude-label',
      'xl',
      '--assignee',
      'me',
      '--exclude-assignee',
      'a@b.co',
      '--creator',
      'c@b.co',
      '--exclude-creator',
      'd@b.co',
      '--project',
      'P',
      '--exclude-project',
      'Q',
    ],
    {
      labels: { include: ['l'], exclude: ['xl'] },
      assignees: { include: ['me'], exclude: ['a@b.co'] },
      creators: { include: ['c@b.co'], exclude: ['d@b.co'] },
      projects: { include: ['P'], exclude: ['Q'] },
    },
    [],
  ],
  [
    'others become positionals',
    ['BOS-1', '--limit', '5', '--label', 'a'],
    { labels: { include: ['a'] } },
    ['BOS-1', '--limit', '5'],
  ],
  ['duplicates collapse', ['--label', 'a,a', '--label', 'a'], { labels: { include: ['a'] } }, []],
]

for (const [name, argv, flags, positionals] of FLAG_CASES) {
  test(`parseSelectionFlags: ${name}`, () => {
    assert.deepEqual(parseSelectionFlags(argv), { flags, positionals })
  })
}

for (const [name, argv, pattern] of [
  ['a missing value', ['--label'], /--label requires a non-empty value/],
  [
    'a flag as the value',
    ['--label', '--exclude-label', 'x'],
    /--label requires a non-empty value/,
  ],
  ['an empty equals value', ['--project='], /--project requires a non-empty value/],
  ['an empty comma entry', ['--label', 'a,,b'], /--label "a,,b" carries an empty entry/],
]) {
  test(`parseSelectionFlags rejects ${name}`, () => {
    assert.match(parseSelectionFlags(argv).error, pattern)
  })
}

test('gateSelectionFlags refuses positionals and unknown flags', () => {
  assert.throws(
    () => gateSelectionFlags('boss-build', ['BOS-1']),
    /unexpected argument\(s\) "BOS-1"; the boss-build gate accepts only/,
  )
  assert.throws(() => gateSelectionFlags('boss-plan', ['--labels', 'x']), /unexpected argument/)
  assert.deepEqual(gateSelectionFlags('boss-plan', ['--label', 'x']), {
    labels: { include: ['x'] },
  })
})

// --- resolution -------------------------------------------------------------------------------

test('selectionResolutionRefs is empty when nothing is configured and dedupes across slots', () => {
  assert.deepEqual(selectionResolutionRefs(effectiveSelection(null, 'build')), {
    users: [],
    labels: [],
    projects: [],
  })
  const refs = selectionResolutionRefs(
    sel({
      labels: { include: ['a'], exclude: ['a', 'b'] },
      assignees: { include: ['me'] },
      creators: { exclude: ['me', 'bot@x.io'] },
      projects: { exclude: ['P'] },
    }),
  )
  assert.deepEqual(refs, { users: ['me', 'bot@x.io'], labels: ['a', 'b'], projects: ['P'] })
})

test('applyResolution maps every slot and keeps every label case variant', () => {
  const resolved = applyResolution(
    sel({
      labels: { exclude: ['infra'] },
      creators: { exclude: ['me'] },
      projects: { include: ['P'] },
    }),
    {
      users: { me: 'usr_1' },
      labels: { infra: ['infra', 'Infra'] },
      projects: { P: ['prj_1', 'prj_2'] },
    },
  )
  assert.deepEqual(resolved.labels.exclude, ['infra', 'Infra'])
  assert.deepEqual(resolved.creators.exclude, ['usr_1'])
  assert.deepEqual(resolved.projects.include, ['prj_1', 'prj_2'])
})

test('applyResolution throws naming every unresolved ref instead of dropping its slot', () => {
  assert.throws(
    () =>
      applyResolution(sel({ labels: { exclude: ['infra'] }, assignees: { include: ['x@y.io'] } }), {
        labels: {},
        users: {},
      }),
    /could not resolve label "infra", user "x@y.io"; refusing to drop the filter/,
  )
})

test('assertEffectiveSelection rejects a misspelt dimension or slot', () => {
  assert.throws(
    () => assertEffectiveSelection({ label: { include: ['a'] } }),
    /unknown selection dimension "label"/,
  )
  assert.throws(
    () => assertEffectiveSelection({ labels: { inc: ['a'] } }),
    /unknown selection slot labels\."inc"/,
  )
})

// --- matchIssue agrees with the rendered filter -----------------------------------------------

// A reference interpreter of EXACTLY the shapes the renderer emits, against a canonical issue.
// Any other key throws, so a renderer that grew a new shape fails here instead of being skipped.
function evaluate(filter, issue) {
  return Object.entries(filter).every(([key, value]) => {
    if (key === 'and') return value.every((clause) => evaluate(clause, issue))
    if (key === 'or') return value.some((clause) => evaluate(clause, issue))
    if (key === 'state' || key === 'team') return compare(issue[key].name, value.name)
    if (key === 'labels') {
      const [quantifier, inner] = Object.entries(value)[0]
      const hits = issue.labels.map((name) => compare(name, inner.name))
      if (quantifier === 'some') return hits.some(Boolean)
      if (quantifier === 'every') return hits.every(Boolean)
      throw new Error(`interpreter: labels.${quantifier}`)
    }
    if (['assignee', 'creator', 'project'].includes(key)) {
      if (value.null === true) return issue[key] === null
      if (value.id) return issue[key] !== null && compare(issue[key], value.id)
      throw new Error(`interpreter: ${key} ${JSON.stringify(value)}`)
    }
    throw new Error(`interpreter: unknown key ${key}`)
  })
}

function compare(actual, comparator) {
  const [op, operand] = Object.entries(comparator)[0]
  if (op === 'eq') return actual === operand
  if (op === 'in') return operand.includes(actual)
  if (op === 'nin') return !operand.includes(actual)
  throw new Error(`interpreter: comparator ${op}`)
}

// Canonical issues; each is rendered into the three shapes matchIssue accepts.
const ISSUES = [
  {
    id: 'T-1',
    labels: ['agent-build', 'backend'],
    assignee: 'usr_1',
    creator: 'usr_2',
    project: 'prj_1',
  },
  {
    id: 'T-2',
    labels: ['agent-build', 'infra'],
    assignee: null,
    creator: 'usr_bot',
    project: null,
  },
  {
    id: 'T-3',
    labels: ['agent-build', 'needs-human'],
    assignee: 'usr_2',
    creator: 'usr_1',
    project: 'prj_2',
  },
  { id: 'T-4', labels: [], assignee: null, creator: null, project: null },
  {
    id: 'T-5',
    labels: ['backend', 'Infra'],
    assignee: 'usr_1',
    creator: 'usr_bot',
    project: 'prj_2',
  },
]

const ref = (id) => (id === null ? null : { id })
const shapes = (issue) => {
  const graphql = {
    identifier: issue.id,
    labels: { nodes: issue.labels.map((name) => ({ name })) },
    assignee: ref(issue.assignee),
    creator: ref(issue.creator),
    project: ref(issue.project),
    state: { name: 'Todo', type: 'unstarted' },
  }
  return {
    graphql,
    mcp: {
      id: issue.id,
      labels: [...issue.labels],
      assignee: issue.assignee ? 'Display Name' : null,
      assigneeId: issue.assignee,
      createdById: issue.creator,
      project: issue.project ? 'Project Name' : null,
      projectId: issue.project,
      status: 'Todo',
    },
    normalized: normalizeTicket(graphql),
  }
}

const SELECTIONS = [
  [{}, {}],
  [{ labels: { include: ['backend'] } }, {}],
  [{ labels: { exclude: ['infra', 'Infra'] } }, {}],
  [{}, { requireLabels: ['agent-build'], excludeLabels: ['needs-human'] }],
  [{ assignees: { include: ['usr_1'] } }, {}],
  [{ assignees: { exclude: ['usr_1'] } }, {}],
  [{ creators: { include: ['usr_1', 'usr_2'] } }, {}],
  [{ creators: { exclude: ['usr_bot'] } }, {}],
  [{ projects: { include: ['prj_2'] } }, {}],
  [{ projects: { exclude: ['prj_1'] } }, {}],
  [
    {
      labels: { exclude: ['infra'] },
      creators: { exclude: ['usr_bot'] },
      assignees: { exclude: ['usr_2'] },
    },
    { requireLabels: ['agent-build'], excludeLabels: ['needs-human'] },
  ],
]

test('matchIssue agrees with the rendered filter on every fixture, in all three issue shapes', () => {
  let checked = 0
  for (const [slots, rules] of SELECTIONS) {
    const selection = sel(slots)
    const filter = renderLinearIssueFilter(selection, rules)
    for (const issue of ISSUES) {
      const expected = evaluate(filter, issue)
      for (const [shape, payload] of Object.entries(shapes(issue))) {
        const { matches, reason } = matchIssue(payload, selection, rules)
        assert.equal(
          matches,
          expected,
          `${issue.id} ${shape} ${JSON.stringify(slots)} ${JSON.stringify(rules)}: ${reason}`,
        )
        assert.equal(reason === null, matches, 'reason is null exactly when it matches')
        checked += 1
      }
    }
  }
  assert.equal(checked, SELECTIONS.length * ISSUES.length * 3)
})

test('matchIssue never passes a slot whose field is absent from the fetched issue', () => {
  const bare = { id: 'T-9', labels: ['agent-build'] } // MCP payload fetched without relation fields
  for (const dimension of ['assignees', 'creators', 'projects']) {
    for (const polarity of ['include', 'exclude']) {
      const result = matchIssue(bare, sel({ [dimension]: { [polarity]: ['x'] } }))
      assert.deepEqual(result, {
        matches: false,
        reason: `${dimension}: field absent on the fetched issue`,
      })
    }
  }
  assert.deepEqual(matchIssue({ id: 'T-9' }, sel({ labels: { exclude: ['x'] } })), {
    matches: false,
    reason: 'labels: field absent on the fetched issue',
  })
  // An absent field a configured slot does NOT need is irrelevant.
  assert.equal(matchIssue(bare, sel({ labels: { include: ['agent-build'] } })).matches, true)
  // normalizeTicket of a payload with no relation fields keeps them absent, not null.
  const normalized = normalizeTicket(bare)
  assert.equal(normalized.assigneeId, undefined)
  assert.equal(matchIssue(normalized, sel({ assignees: { exclude: ['x'] } })).matches, false)
})

test('matchIssue names the first failing slot', () => {
  const issue = shapes(ISSUES[1]).mcp
  assert.equal(
    matchIssue(issue, sel({ labels: { exclude: ['infra'] } })).reason,
    'labels.exclude: carries "infra"',
  )
  assert.equal(
    matchIssue(shapes(ISSUES[0]).graphql, sel({ assignees: { include: ['usr_2'] } })).reason,
    'assignees.include: assignee usr_1 not in [usr_2]',
  )
  assert.equal(
    matchIssue(issue, sel({ creators: { exclude: ['usr_bot'] } })).reason,
    'creators.exclude: creator usr_bot is excluded',
  )
  assert.equal(
    matchIssue(issue, sel(), { requireLabels: ['agent-plan'] }).reason,
    'requireLabels: missing "agent-plan"',
  )
})

// --- describeEmptyScan ------------------------------------------------------------------------

test('describeEmptyScan: a narrowed skip reads differently from an empty backlog', () => {
  const base = { state: 'Todo', requireLabels: ['agent-build'], excludeLabels: ['needs-human'] }
  const unnarrowed = describeEmptyScan(
    'boss-build',
    { ...base, selection: sel() },
    { unblocked: true },
  )
  const narrowed = describeEmptyScan(
    'boss-build',
    {
      ...base,
      selection: sel({
        labels: { exclude: ['infra'] },
        creators: { exclude: ['bot@example.com'] },
      }),
    },
    { unblocked: true },
  )
  assert.equal(
    unnarrowed,
    'boss-build gate: no unblocked Todo issues labelled agent-build without needs-human',
  )
  assert.equal(
    narrowed,
    'boss-build gate: no unblocked Todo issues labelled agent-build without needs-human matching selection labels.exclude=[infra] creators.exclude=[bot@example.com]',
  )
})

// --- split-args CLI ---------------------------------------------------------------------------

const SELECTION_CLI = fileURLToPath(new URL('./selection.mjs', import.meta.url))

function splitArgsCli(args) {
  const result = spawnSync(process.execPath, [SELECTION_CLI, 'split-args', '--', ...args], {
    encoding: 'utf8',
  })
  return { status: result.status, out: result.stdout ? JSON.parse(result.stdout) : null }
}

test('split-args: a filter is never a ticket id, and selection tokens are kept verbatim', () => {
  const { status, out } = splitArgsCli([
    '--exclude-label',
    'infra',
    'BOS-12',
    '--creator=me',
    '--headless',
  ])
  assert.equal(status, 0)
  assert.deepEqual(out, {
    tickets: ['BOS-12'],
    selectionArgs: ['--exclude-label', 'infra', '--creator=me'],
    other: ['--headless'],
  })
})

test('split-args: a ticket URL is a ticket; a filter alone yields no ticket', () => {
  const url = 'https://linear.app/acme/issue/ABC-7/slug'
  assert.deepEqual(splitArgsCli([url]).out.tickets, [url])
  assert.deepEqual(splitArgsCli(['--exclude-label', 'infra']).out.tickets, [])
})

test('split-args: a malformed flag exits 2 with the error', () => {
  const { status, out } = splitArgsCli(['--label'])
  assert.equal(status, 2)
  assert.match(out.error, /--label requires a non-empty value/)
})

test('split-args agrees with splitArgs in-process', () => {
  const args = ['BOS-1', '--project', 'P,Q', '--limit', '3']
  assert.deepEqual(splitArgsCli(args).out, splitArgs(args))
})

test('every stage is a known selection stage', () => {
  assert.deepEqual(SELECTION_STAGES, ['plan', 'build', 'epic', 'verify', 'retro', 'release'])
})
