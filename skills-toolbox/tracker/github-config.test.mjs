import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  parseRepository,
  githubRepositoryFromOrigin,
  githubStates,
  GITHUB_DEFAULT_STATES,
  GITHUB_CLOSING_ROLES,
  githubRequiredLabels,
  GITHUB_ESTIMATE_POINTS,
} from './github-config.mjs'
import {
  withTrackerDefaults,
  trackerReadiness,
  validateConfig,
  DEFAULT_CONFIG,
  DEFAULT_PIPELINE_LABELS,
} from '../skill-config.mjs'

test('repository identity and state defaults are independent of MCP', () => {
  assert.deepEqual(parseRepository('acme/tasks'), {
    host: 'github.com',
    owner: 'acme',
    repo: 'tasks',
    repository: 'acme/tasks',
  })
  assert.equal(parseRepository('git.example/acme/tasks').host, 'git.example')
  for (const value of ['', 'owner', 'a/b/c/d', 'https://github.com/a/b', 'a/b?x'])
    assert.throws(() => parseRepository(value))
  assert.equal(githubStates({ planned: 'queue' }).planned, 'queue')
  assert.throws(() => githubStates({ planned: 'same', inReview: 'same' }), /duplicate/)
  assert.equal(GITHUB_CLOSING_ROLES.canceled, 'not planned')
  const config = withTrackerDefaults({
    adapters: { tracker: 'github' },
    trackerConfig: { github: { repository: 'acme/tasks' } },
  })
  assert.deepEqual(config.trackerConfig.github.states, GITHUB_DEFAULT_STATES)
  assert.equal(config.trackerConfig.github.mcpServer, undefined)
  assert.equal(trackerReadiness(config), 'ready')
  assert.equal(
    trackerReadiness({ adapters: { tracker: 'github' }, trackerConfig: { github: {} } }),
    'unconfigured',
  )
})

test('schema requires valid repository only for github and MCP for other trackers', () => {
  for (const repository of ['acme/tasks', 'git.example/acme/tasks']) {
    assert.doesNotThrow(() =>
      validateConfig({ ...DEFAULT_CONFIG, trackerConfig: { github: { repository } } }),
    )
  }
  for (const block of [{}, { repository: 'bad' }])
    assert.throws(() => validateConfig({ ...DEFAULT_CONFIG, trackerConfig: { github: block } }))
  assert.throws(() => validateConfig({ ...DEFAULT_CONFIG, trackerConfig: { linear: {} } }))
})

test('required GitHub labels include overrides and all ranking values without case duplicates', () => {
  assert.deepEqual(
    Object.fromEntries(
      githubRequiredLabels()
        .filter((entry) => entry.kind === 'pipeline')
        .map(({ role, name }) => [role, name]),
    ),
    DEFAULT_PIPELINE_LABELS,
  )
  const labels = githubRequiredLabels({
    states: { planned: 'Queue' },
    labels: { agentBuild: 'Build', epic: 'BUILD' },
  })
  assert.deepEqual(
    labels.find((entry) => entry.role === 'planned'),
    { kind: 'state', role: 'planned', name: 'Queue' },
  )
  assert.deepEqual(
    labels.find((entry) => entry.role === 'agentBuild'),
    { kind: 'pipeline', role: 'agentBuild', name: 'Build' },
  )
  assert.equal(
    labels.some((entry) => entry.name === 'boss:todo'),
    false,
  )
  assert.equal(
    labels.some((entry) => entry.name === 'agent-plan'),
    true,
  )
  assert.equal(
    labels.some((entry) => entry.role === 'done'),
    false,
  )
  assert.equal(labels.filter((entry) => entry.name.toLowerCase() === 'build').length, 1)
  assert.deepEqual(
    labels.filter((entry) => entry.kind === 'ranking').map((entry) => entry.name),
    [
      ...[1, 2, 3, 4].map((value) => `boss:priority-${value}`),
      ...[0, 1, 2, 3, 5, 8].map((value) => `boss:estimate-${value}`),
    ],
  )
})

test('GitHub ranking estimates match the plan library normalization set', async () => {
  const { normalizeDecomposition } = await import('../plan/plan-epic-lib.mjs')
  const spec = { children: Array.from({ length: 10 }, (_, estimate) => ({ estimate })) }
  normalizeDecomposition(spec)
  assert.deepEqual(
    [...new Set(spec.children.map((child) => child.estimate))],
    GITHUB_ESTIMATE_POINTS,
  )
})

test('githubRepositoryFromOrigin parses GitHub remotes and rejects other hosts or malformed URLs', () => {
  const valid = [
    ['https://github.com/acme/widgets', 'acme/widgets'],
    ['https://github.com/acme/widgets.git', 'acme/widgets'],
    ['https://github.com/acme/widgets/', 'acme/widgets'],
    ['https://github.com/acme/widgets.git/', 'acme/widgets'],
    ['https://user:pass@github.com/acme/widgets.git', 'acme/widgets'],
    ['https://token@github.com/acme/widgets', 'acme/widgets'],
    ['ssh://git@github.com/acme/widgets.git', 'acme/widgets'],
    ['ssh://git@github.com:22/acme/widgets.git', 'acme/widgets'],
    ['git@github.com:acme/widgets.git', 'acme/widgets'],
    ['git@github.com:acme/widgets', 'acme/widgets'],
    ['git@GITHUB.COM:Acme/Widgets.git', 'Acme/Widgets'],
    ['https://GitHub.Example.COM/acme/widgets.git', 'github.example.com/acme/widgets'],
    [
      'ssh://git@github.enterprise.local:2222/acme/widgets.git',
      'github.enterprise.local/acme/widgets',
    ],
    ['git@github.enterprise.local:acme/widgets.git', 'github.enterprise.local/acme/widgets'],
    ['  https://github.com/acme/my.repo_1.git  ', 'acme/my.repo_1'],
  ]
  for (const [input, expected] of valid) {
    assert.equal(githubRepositoryFromOrigin(input), expected, `expected ${expected} for ${input}`)
  }

  const invalid = [
    '',
    '   ',
    null,
    undefined,
    123,
    'https://gitlab.com/acme/widgets.git',
    'git@gitlab.com:acme/widgets.git',
    'https://bitbucket.org/acme/widgets.git',
    'https://mygithub.com/acme/widgets.git',
    'https://notgithub.com/acme/widgets.git',
    'https://github-enterprise.com/acme/widgets.git',
    'git@github.com-work:acme/widgets.git',
    'git@github.personal:acme/widgets.git',
    'https://github./acme/widgets.git',
    'https://github..com/acme/widgets.git',
    'https://github.-bad.com/acme/widgets.git',
    'https://github.com/acme/widgets/extra',
    'https://github.com/acme',
    'https://github.com/acme/widgets?tab=readme',
    'https://github.com/acme/widgets?',
    'https://github.com/acme/widgets#top',
    'https://github.com/acme/widgets#',
    'http://github.com/acme/widgets.git',
    'git://github.com/acme/widgets.git',
    'git@github.com:/acme/widgets.git',
    'https://github.com/acme/..',
    'not-a-url',
  ]
  for (const input of invalid) {
    assert.equal(githubRepositoryFromOrigin(input), null, `expected null for ${String(input)}`)
  }
})
