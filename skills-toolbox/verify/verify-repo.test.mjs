import assert from 'node:assert/strict'
import test from 'node:test'
import { originRepo, repoFor } from './verify-repo.mjs'

const okOut = (output) => ({ ok: true, output })
const okJson = (payload) => ({ ok: true, payload, output: JSON.stringify(payload) })
const failed = (stderr = 'fatal: No such remote') => ({ ok: false, stderr, output: '' })
const rateLimited = { ok: false, stderr: 'GraphQL: API rate limit already exceeded' }

test('repoFor returns explicit repo without calling read', () => {
  const calls = []
  const read = (cmd, args) => {
    calls.push([cmd, args])
    return okOut('git@github.com:Other/Repo.git')
  }
  assert.equal(repoFor(read, 'Explicit/Repo'), 'Explicit/Repo')
  assert.deepEqual(calls, [])
})

test('repoFor resolves case-preserved owner/name from origin remote with zero gh calls', () => {
  const cases = [
    ['git@github.com:Acme/My-Repo.git\n', 'Acme/My-Repo'],
    ['git@github.com:Acme/My-Repo', 'Acme/My-Repo'],
    ['git@github.com:Acme/My-Repo.git/', 'Acme/My-Repo'],
    ['git@github.com:Acme/My-Repo/', 'Acme/My-Repo'],
    ['user@git.example.com:Acme_Org/My.Repo-1.git', 'Acme_Org/My.Repo-1'],
    ['ssh://git@github.com/Acme/My-Repo.git', 'Acme/My-Repo'],
    ['ssh://git@github.com:22/Acme/My-Repo/', 'Acme/My-Repo'],
    ['ssh://github.com/Acme/My-Repo.git/', 'Acme/My-Repo'],
    ['https://github.com/Acme/My-Repo.git', 'Acme/My-Repo'],
    ['https://github.com/Acme/My-Repo/', 'Acme/My-Repo'],
    ['https://github.com/Acme/My-Repo.git/', 'Acme/My-Repo'],
    ['https://token@github.com:443/Acme/My-Repo.git', 'Acme/My-Repo'],
  ]
  for (const [remoteUrl, expected] of cases) {
    const calls = []
    const read = (cmd, args) => {
      calls.push([cmd, args])
      assert.equal(cmd, 'git', `unexpected ${cmd} call for ${remoteUrl}`)
      return okOut(remoteUrl)
    }
    assert.equal(repoFor(read, undefined), expected, remoteUrl)
    assert.deepEqual(calls, [['git', ['remote', 'get-url', 'origin']]], remoteUrl)
  }
})

test('originRepo rejects unreadable, non-URL, traversal, extra-segment, and query/fragment values', () => {
  const badOutputs = [
    '',
    '   \n',
    '/Users/dev/Acme/My-Repo.git',
    '../Acme/My-Repo',
    'file:///Users/dev/Acme/My-Repo.git',
    'http://github.com/Acme/My-Repo.git',
    'Acme/My-Repo',
    '{"full_name":"Acme/My-Repo"}',
    'https://github.com/Acme/My-Repo/extra',
    'git@github.com:Acme/My-Repo/extra.git',
    'https://github.com/Acme/My-Repo/.git',
    'https://github.com/Acme/My-Repo//',
    'https://github.com/./My-Repo',
    'https://github.com/../My-Repo',
    'https://github.com/Acme/.',
    'https://github.com/Acme/..',
    'https://github.com/Acme/..git',
    'https://github.com/Acme/...git',
    'https://github.com/Acme/.git',
    'git@github.com:../My-Repo.git',
    'git@github.com:Acme/..',
    'https://github.com/Acme/My-Repo?tab=readme',
    'https://github.com/Acme/My-Repo?',
    'https://github.com/Acme/My-Repo#section',
    'git@github.com:Acme/My-Repo#section',
    'https://github.com/Acme/My Repo',
    'https://github.com/Acme/My$Repo',
    'https://github.com/Acme/My-Repo\nhttps://github.com/Other/Repo',
  ]
  for (const output of badOutputs) {
    assert.equal(
      originRepo(() => okOut(output)),
      '',
      `expected empty for ${JSON.stringify(output)}`,
    )
  }
  assert.equal(
    originRepo(() => failed()),
    '',
  )
  assert.equal(
    originRepo(() => ({ ok: true, output: 123 })),
    '',
  )
  assert.equal(
    originRepo(() => undefined),
    '',
  )
  assert.equal(
    originRepo(() => {
      throw new Error('git unavailable')
    }),
    '',
  )
})

test('repoFor falls back to gh repo view when origin is unreadable or unparseable', () => {
  const badRemotes = [
    ['failed git read', () => failed()],
    ['local path', () => okOut('/tmp/local/repo')],
    ['extra segments', () => okOut('https://github.com/Acme/My-Repo/tree/main')],
    ['JSON payload', () => okOut('{"full_name":"Acme/My-Repo"}')],
  ]
  for (const [label, gitAnswer] of badRemotes) {
    const calls = []
    const read = (cmd, args) => {
      calls.push([cmd, args])
      if (cmd === 'git') return gitAnswer()
      if (cmd === 'gh' && args[0] === 'repo') return okJson({ nameWithOwner: 'Acme/From-Api' })
      return failed('unexpected')
    }
    assert.equal(repoFor(read), 'Acme/From-Api', label)
    assert.deepEqual(
      calls,
      [
        ['git', ['remote', 'get-url', 'origin']],
        ['gh', ['repo', 'view', '--json', 'nameWithOwner']],
      ],
      label,
    )
  }
})

test('repoFor falls back to REST quota and records degraded when origin fails and GraphQL is rate-limited', () => {
  const calls = []
  const degraded = []
  const read = (cmd, args) => {
    calls.push([cmd, args])
    if (cmd === 'git') return failed()
    if (cmd === 'gh' && args[0] === 'repo') return rateLimited
    if (cmd === 'gh' && args[0] === 'api' && args[1] === 'repos/{owner}/{repo}')
      return okJson({ full_name: 'Acme/From-Rest' })
    return failed('unexpected')
  }
  assert.equal(repoFor(read, '', degraded), 'Acme/From-Rest')
  assert.deepEqual(calls, [
    ['git', ['remote', 'get-url', 'origin']],
    ['gh', ['repo', 'view', '--json', 'nameWithOwner']],
    ['gh', ['api', 'repos/{owner}/{repo}']],
  ])
  assert.deepEqual(degraded, [
    { read: 'repo-name', transport: 'core', reason: 'graphql-rate-limited' },
  ])
})

test('repoFor returns empty string when both origin and gh fail', () => {
  assert.equal(
    repoFor(() => failed()),
    '',
  )
  assert.equal(
    repoFor(() => rateLimited),
    '',
  )
})
