import assert from 'node:assert/strict'
import { test } from 'node:test'

import { main, resolveBaseBranch } from './base-branch.mjs'

const sessionBranch = 'work/topic'

test('resolveBaseBranch chooses the first usable source and preserves upstream remotes', () => {
  const candidates = {
    sessionBranch,
    sessionBase: 'integration',
    prBase: 'release',
    upstream: 'upstream/stable',
    defaultBranch: 'main',
  }
  assert.deepEqual(resolveBaseBranch(candidates), {
    remote: 'origin',
    branch: 'integration',
    source: 'session',
  })
  assert.deepEqual(resolveBaseBranch({ ...candidates, sessionBase: sessionBranch }), {
    remote: 'origin',
    branch: 'release',
    source: 'pr',
  })
  assert.deepEqual(resolveBaseBranch({ ...candidates, sessionBase: '', prBase: '' }), {
    remote: 'upstream',
    branch: 'stable',
    source: 'upstream',
  })
  assert.deepEqual(
    resolveBaseBranch({
      sessionBranch,
      upstream: `origin/${sessionBranch}`,
      defaultBranch: 'main',
    }),
    {
      remote: 'origin',
      branch: 'main',
      source: 'default',
    },
  )
  assert.equal(
    resolveBaseBranch({
      sessionBranch,
      sessionBase: sessionBranch,
      prBase: sessionBranch,
      upstream: `origin/${sessionBranch}`,
      defaultBranch: sessionBranch,
    }),
    null,
  )
})

function runner(responses) {
  const calls = []
  return {
    calls,
    exec: async (cmd, args) => {
      calls.push([cmd, ...args])
      const result = responses.shift()
      if (result instanceof Error) throw result
      return typeof result === 'string' ? result : JSON.stringify(result)
    },
  }
}

const args = [
  'resolve',
  '--session-branch',
  sessionBranch,
  '--boss',
  '/test/boss',
  '--gh',
  '/test/gh',
]

test('a stored session base wins before GitHub or a self-tracking upstream', async () => {
  const deps = runner([
    { session: { base_branch: 'integration' } },
    'abc123\trefs/heads/integration\n',
  ])
  assert.deepEqual(
    await main(
      [...args, '--session-id', 'session-1', '--upstream', `origin/${sessionBranch}`],
      deps,
    ),
    {
      exitCode: 0,
      stdout: 'origin\tintegration\tsession\n',
      stderr: '',
    },
  )
  assert.deepEqual(deps.calls, [
    ['/test/boss', 'show', 'session-1', '--json'],
    ['git', 'ls-remote', '--heads', 'origin', 'refs/heads/integration'],
  ])
})

test('a deleted or unreadable stored session base falls through to the open PR', async () => {
  for (const remote of ['', new Error('remote unavailable')]) {
    const deps = runner([
      { session: { base_branch: 'parent/topic' } },
      remote,
      { baseRefName: 'main', state: 'OPEN' },
    ])
    assert.equal(
      (await main([...args, '--session-id', 'session-1'], deps)).stdout,
      'origin\tmain\tpr\n',
    )
  }
})

test('without a session an open PR supplies the base', async () => {
  const deps = runner([{ baseRefName: 'integration', state: 'OPEN' }])
  assert.equal((await main(args, deps)).stdout, 'origin\tintegration\tpr\n')
  assert.deepEqual(deps.calls, [
    ['/test/gh', 'pr', 'view', sessionBranch, '--json', 'baseRefName,state'],
  ])
})

test('an open fork PR keeps the matching non-self upstream remote', async () => {
  const deps = runner([{ baseRefName: 'main', state: 'OPEN' }])
  assert.equal(
    (await main([...args, '--upstream', 'upstream/main'], deps)).stdout,
    'upstream\tmain\tpr\n',
  )
  assert.deepEqual(
    resolveBaseBranch({ sessionBranch, prBase: 'main', upstream: 'upstream/main' }),
    { remote: 'upstream', branch: 'main', source: 'pr' },
  )
})

test('closed and merged PRs fall through to a genuine upstream without reading the default', async () => {
  for (const state of ['CLOSED', 'MERGED']) {
    const deps = runner([{ baseRefName: 'integration', state }])
    assert.equal(
      (await main([...args, '--upstream', 'upstream/release/next'], deps)).stdout,
      'upstream\trelease/next\tupstream\n',
    )
    assert.equal(deps.calls.length, 1)
  }
})

test('a self-tracking upstream falls through to the default branch', async () => {
  const deps = runner([new Error('no PR'), { defaultBranchRef: { name: 'main' } }])
  assert.equal(
    (await main([...args, '--upstream', `origin/${sessionBranch}`], deps)).stdout,
    'origin\tmain\tdefault\n',
  )
  assert.deepEqual(deps.calls[1], ['/test/gh', 'repo', 'view', '--json', 'defaultBranchRef'])
})

test('failed, malformed and unusable session reads fall back to an open PR', async () => {
  for (const session of [
    new Error('old boss'),
    'not JSON',
    null,
    { session: { base_branch: sessionBranch } },
  ]) {
    const deps = runner([session, { baseRefName: 'integration', state: 'OPEN' }])
    assert.equal(
      (await main([...args, '--session-id', 's'], deps)).stdout,
      'origin\tintegration\tpr\n',
    )
  }
})

test('unusable PR payloads and self bases fall through to default', async () => {
  for (const pr of ['broken JSON', null, { state: 'OPEN', baseRefName: sessionBranch }]) {
    const deps = runner([pr, { defaultBranchRef: { name: 'main' } }])
    assert.equal((await main(args, deps)).stdout, 'origin\tmain\tdefault\n')
  }
})

test('failure names every attempted source without producing a base', async () => {
  const deps = runner([new Error('boss failed'), new Error('gh failed'), 'bad JSON'])
  const result = await main(
    [...args, '--session-id', 's', '--upstream', `origin/${sessionBranch}`],
    deps,
  )
  assert.equal(result.exitCode, 1)
  assert.equal(result.stdout, '')
  for (const source of ['session', 'pr', 'upstream', 'default'])
    assert.match(result.stderr, new RegExp(source))
})

test('strict CLI parsing refuses unknown flags, missing branches and unsupported verbs', async () => {
  for (const argv of [
    [],
    ['resolve'],
    [...args, '--unknown'],
    ['other', '--session-branch', sessionBranch],
  ]) {
    const deps = runner([])
    assert.equal((await main(argv, deps)).exitCode, 64)
    assert.equal(deps.calls.length, 0)
  }
})
