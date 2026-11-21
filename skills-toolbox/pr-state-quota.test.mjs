import assert from 'node:assert/strict'
import test from 'node:test'
import { main, UsageError } from './pr-state.mjs'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

const url = 'https://github.com/acme/widgets/pull/7'
const sha = 'a'.repeat(40)
const json = (value) => ({ status: 0, stdout: JSON.stringify(value) })
const refused = { status: 1, stderr: 'API rate limit exceeded' }
const graph = { number: 7, url, state: 'MERGED', mergeCommit: { oid: sha } }
const rest = { number: 7, html_url: url, state: 'closed', merged: true, merge_commit_sha: sha }

function run(
  argv,
  answers,
  env = {},
  origin = 'git@github.com:acme/widgets.git',
  selected = '',
  hosts = 'github.com:\n  user: test\nghe.example.test:\n  oauth_token: fixture-secret\n',
) {
  const directory = mkdtempSync(join(tmpdir(), 'gh-context-'))
  writeFileSync(join(directory, 'hosts.yml'), hosts)
  env = { GH_CONFIG_DIR: directory, ...env }
  const calls = []
  try {
    const result = main(argv, {
      env,
      resolveBoss: () => ({ ok: false }),
      spawn: (cmd, args, opts) => {
        calls.push({ cmd, args, opts })
        if (cmd === 'git') return jsonGit(args, origin)
        if (cmd === 'ssh') {
          const aliases = {
            'public-alias': 'ssh.github.com',
            'enterprise-alias': 'ghe.example.test',
          }
          const host = aliases[args.at(-1)]
          return host
            ? { status: 0, stdout: `hostname ${host}\nidentityfile private-path\n` }
            : { error: { code: 'ENOENT' } }
        }
        if (args[0] === 'repo') return { status: 0, stdout: selected }
        assert.equal(opts.env.BOSS_GH_USAGE_LABEL, 'fallback:pr-state')
        return answers.shift()
      },
    })
    return {
      result,
      spawns: calls,
      calls: calls.filter((call) => call.cmd === 'gh' && call.args[0] !== 'repo'),
    }
  } finally {
    rmSync(directory, { recursive: true, force: true, maxRetries: 3 })
  }
}
function jsonGit(args, origin) {
  return {
    status: 0,
    stdout:
      args[0] === 'branch'
        ? 'feat\n'
        : args[1] === '-v'
          ? origin.includes('\t')
            ? origin
            : `origin\t${origin} (fetch)\norigin\t${origin} (push)\n`
          : `${origin.includes('\t') ? (/^origin\s+(\S+)/m.exec(origin)?.[1] ?? '') : origin}\n`,
  }
}

test('landed normalizes REST and rate-limit GraphQL fallback merge evidence', () => {
  for (const [answers, transport, degraded] of [
    [[json(rest)], 'core', null],
    [[refused, json(graph)], 'graphql', { transport: 'graphql', reason: 'core-rate-limited' }],
  ]) {
    const { result } = run(['landed', '--url', url], answers)
    assert.deepEqual(result, {
      exit: 0,
      out: { number: 7, url, state: 'MERGED', merged: true, mergeCommit: sha, transport, degraded },
    })
  }
  for (const state of ['open', 'closed']) {
    const { result } = run(
      ['landed', '--pr', '7', '--repo', 'acme/widgets'],
      [json({ ...rest, state, merged: false, merge_commit_sha: null })],
    )
    assert.equal(result.out.merged, false)
    assert.equal(result.out.state, state.toUpperCase())
  }
})

test('landed refuses non-pull URLs and failed reads have no negative merge answer', () => {
  for (const bad of [
    'https://github.com/acme/widgets/issues/7',
    'https://evil.test/acme/widgets/pull/7',
  ])
    assert.throws(() => main(['landed', '--url', bad]), UsageError)
  for (const answers of [[{ status: 1, stderr: 'HTTP 404' }], [refused, refused], [json({})]]) {
    const { result } = run(['landed', '--url', url], answers)
    assert.equal(result.exit, 1)
    assert.equal('merged' in result.out, false)
  }
})

test('head-pr produces ownership-compatible arrays on both quotas and filters fork branches', () => {
  const normalized = [
    { number: 7, title: 'ticket', body: 'body', headRefName: 'feat/name', state: 'OPEN', url },
  ]
  const restList = [
    {
      number: 7,
      title: 'ticket',
      body: 'body',
      head: { ref: 'feat/name' },
      state: 'open',
      html_url: url,
    },
  ]
  for (const answers of [[json(normalized)], [refused, json([restList])]]) {
    const { result, calls } = run(['head-pr', '--branch', 'feat/name'], answers)
    assert.equal(result.exit, 0)
    assert.deepEqual(result.out, normalized)
    assert.equal(
      execFileSync(
        process.execPath,
        [
          'skills-toolbox/build/pr-ownership.mjs',
          'number',
          '--pr-json',
          JSON.stringify(result.out),
        ],
        { encoding: 'utf8' },
      ).trim(),
      '7',
    )
    if (calls.length === 2)
      assert.deepEqual(calls[1].args.slice(2), [
        '--hostname',
        'github.com',
        '--paginate',
        '--slurp',
      ])
  }
  const empty = run(['head-pr', '--branch', 'feat', '--repo', 'acme/widgets'], [json([])])
  assert.deepEqual(empty.result.out, [])
  const failed = run(
    ['head-pr', '--branch', 'feat', '--repo', 'acme/widgets'],
    [{ status: 1, stderr: 'HTTP 403' }],
  )
  assert.equal(failed.result.exit, 1)
})

test('session-pr shares head lookup and recovers GraphQL refusal through REST', () => {
  const { result } = run(
    ['session-pr'],
    [refused, json([[{ number: 7, html_url: url, state: 'open', head: { ref: 'feat' } }]])],
  )
  assert.equal(result.exit, 0)
  assert.equal(result.out.number, 7)
  assert.equal(result.out.url, url)
  assert.deepEqual(result.out.degraded, { transport: 'core', reason: 'graphql-rate-limited' })
})

test('head-pr uses the gh repository context for both quotas in fork checkouts', () => {
  const item = { number: 7, title: 'fork', body: '', headRefName: 'feat', state: 'OPEN', url }
  for (const env of [{ GH_REPO: 'upstream/widgets' }, {}]) {
    for (const fallback of [false, true]) {
      const answers = fallback
        ? [
            refused,
            json([
              [{ number: 8, html_url: url, state: 'open', head: { ref: 'other' } }],
              [
                {
                  number: 7,
                  title: 'fork',
                  html_url: url,
                  state: 'open',
                  head: { ref: 'feat', user: { login: 'contributor' } },
                },
              ],
            ]),
          ]
        : [json([item])]
      const { result, calls } = run(
        ['head-pr', '--branch', 'feat'],
        answers,
        env,
        'git@github.com:contributor/widgets.git',
        'upstream/widgets',
      )
      assert.equal(result.exit, 0)
      assert.deepEqual(result.out, [item])
      assert.equal(calls[0].args[calls[0].args.indexOf('--repo') + 1], 'upstream/widgets')
      if (fallback) assert.match(calls[1].args[1], /^repos\/upstream\/widgets\/pulls\?/)
    }
  }
  const explicit = run(
    ['head-pr', '--branch', 'feat', '--repo', 'explicit/widgets'],
    [json([])],
    { GH_REPO: 'upstream/widgets' },
    '',
    'selected/widgets',
  )
  assert.equal(explicit.calls[0].args.at(-1), 'explicit/widgets')
})

for (const [remotes, env, expected, hosts] of [
  [
    'origin\tgit@github.com:fork/widgets.git (fetch)\nupstream\tgit@github.com:upstream/widgets.git (fetch)',
    {},
    'github.com/upstream/widgets',
  ],
  [
    'origin\tgit@github.com:fork/widgets.git (fetch)\ngithub\tgit@github.com:preferred/widgets.git (fetch)\nupstream\tgit@gitlab.example.test:unrelated/widgets.git (fetch)',
    {},
    'github.com/preferred/widgets',
  ],
  ['git@public-alias:acme/widgets.git', {}, 'github.com/acme/widgets'],
  ['ssh://git@enterprise-alias/acme/widgets.git', {}, 'ghe.example.test/acme/widgets'],
  ['https://user:secret@ghe.example.test/acme/widgets.git', {}, 'ghe.example.test/acme/widgets'],
  [
    'origin\tgit@ghe.example.test:acme/widgets.git (fetch)\nupstream\tgit@github.com:other/widgets.git (fetch)',
    { GH_HOST: 'ghe.example.test' },
    'ghe.example.test/acme/widgets',
  ],
  [
    'origin\tgit@github.com:other/widgets.git (fetch)\nupstream\tgit@ghe.example.test:acme/widgets.git (fetch)',
    {},
    'ghe.example.test/acme/widgets',
    'off-checkout.example.test:\n  oauth_token: fixture-secret\nghe.example.test:\n  oauth_token: fixture-secret\n',
  ],
  [
    'upstream\tgit@github.com:other/widgets.git (fetch)\norigin\tgit@ghe.example.test:acme/widgets.git (fetch)',
    {},
    'ghe.example.test/acme/widgets',
    'ghe.example.test:\n  oauth_token: fixture-secret\n',
  ],
  [
    'git@github.com:acme/widgets.git',
    {},
    'github.com/acme/widgets',
    'off-checkout.example.test:\n  oauth_token: fixture-secret\n',
  ],
]) {
  test(`implicit gh context ${expected} with configured hosts ${hosts?.split('\n')[0] ?? 'multiple'}`, () => {
    for (const argv of [['head-pr', '--branch', 'feat'], ['session-pr']]) {
      for (const fallback of [false, true]) {
        const item = { number: 7, title: '', body: '', headRefName: 'feat', state: 'OPEN', url }
        const restItem = { number: 7, html_url: url, state: 'open', head: { ref: 'feat' } }
        const { result, calls } = run(
          argv,
          fallback ? [refused, json([[restItem]])] : [json([item])],
          env,
          remotes,
          '',
          hosts,
        )
        assert.equal(result.exit, 0)
        assert.equal(calls[0].args.at(-1), expected)
        if (fallback) {
          assert.match(
            calls[1].args[1],
            new RegExp(`^repos/${expected.split('/').slice(-2).join('/')}/pulls`),
          )
          if (expected.split('/').length === 3)
            assert.equal(
              calls[1].args[calls[1].args.indexOf('--hostname') + 1],
              expected.split('/')[0],
            )
        }
        assert.ok(!JSON.stringify(result).includes('secret'))
      }
    }
  })
}

test('origin lookup accepts URL userinfo and ports on both quota paths', () => {
  for (const origin of [
    'git@github.com:acme/widgets.git',
    'https://github.com/acme/widgets.git',
    'ssh://git@github.com/acme/widgets.git',
    'ssh://git@github.com:22/acme/widgets.git',
    'https://user:token@github.com:443/acme/widgets',
    'https://user:token@github.com:443/acme/widgets.git/',
  ]) {
    for (const argv of [['head-pr', '--branch', 'feat'], ['session-pr'], ['landed', '--pr', '7']]) {
      for (const fallback of [false, true]) {
        const landed = argv[0] === 'landed'
        const answer = landed ? (fallback ? graph : rest) : []
        const { result, calls } = run(
          argv,
          fallback ? [refused, json(landed ? answer : [answer])] : [json(answer)],
          {},
          origin,
        )
        assert.equal(result.exit, 0, `${argv[0]} ${origin}`)
        assert.equal(calls.length, fallback ? 2 : 1)
        for (const { args } of calls) {
          if (args[0] === 'api') assert.match(args[1], /^repos\/acme\/widgets\/pulls/)
          else assert.equal(args[args.indexOf('--repo') + 1], 'github.com/acme/widgets')
          assert.ok(!args.join(' ').includes('token'))
        }
      }
    }
  }
})

test('host-qualified gh repository context reaches the same host on both quotas', () => {
  const item = { number: 7, title: 'host', body: '', headRefName: 'feat', state: 'OPEN', url }
  const restItem = {
    number: 7,
    title: 'host',
    body: '',
    head: { ref: 'feat' },
    state: 'open',
    html_url: url,
  }
  for (const host of ['github.com', 'ghe.example.test']) {
    for (const fromEnv of [false, true]) {
      const qualified = `${host}/acme/widgets`
      const env = { GH_HOST: 'other.example.test', ...(fromEnv ? { GH_REPO: qualified } : {}) }
      for (const argv of [
        ['head-pr', '--branch', 'feat'],
        ['session-pr'],
        ['landed', '--pr', '7'],
      ]) {
        for (const fallback of [false, true]) {
          const landed = argv[0] === 'landed'
          const primary = landed ? rest : [item]
          const alternate = landed ? graph : [[restItem]]
          const { result, calls } = run(
            argv,
            fallback ? [refused, json(alternate)] : [json(primary)],
            env,
            'git@github.com:contributor/widgets.git',
            fromEnv ? '' : qualified,
          )
          assert.equal(result.exit, 0, `${argv[0]} ${qualified}`)
          if (landed) assert.equal(result.out.merged, true)
          else assert.equal(argv[0] === 'session-pr' ? result.out.number : result.out[0].number, 7)
          for (const { args } of calls) {
            if (args[0] === 'api') {
              assert.match(args[1], /^repos\/acme\/widgets\/pulls/)
              assert.equal(args[args.indexOf('--hostname') + 1], host)
            } else assert.equal(args[args.indexOf('--repo') + 1], qualified)
          }
        }
      }
    }
  }
})

test('origin lookup rejects foreign or malformed hosts without leaking credentials', () => {
  for (const origin of [
    'https://github.com.evil.test/acme/widgets',
    'https://github.com@evil.test/acme/widgets',
    'https://evil.test/github.com/acme/widgets',
    'ssh://git@evil.test:22/acme/widgets.git',
    'https://user:token@github.com:invalid/acme/widgets',
    'https://user:token@github.com/acme',
  ]) {
    const { result, calls } = run(['head-pr', '--branch', 'feat'], [], {}, origin)
    assert.equal(result.exit, 1)
    assert.equal(calls.length, 0)
    assert.ok(!JSON.stringify(result).includes('token'))
  }
})

test('SSH aliases are cached, option-terminated and fall back when ssh is unavailable', () => {
  const { result, spawns } = run(
    ['head-pr', '--branch', 'feat'],
    [json([])],
    {},
    'origin\tgit@public-alias:fork/widgets.git (fetch)\nupstream\tgit@public-alias:acme/widgets.git (fetch)',
  )
  assert.equal(result.exit, 0)
  const ssh = spawns.filter(({ cmd }) => cmd === 'ssh')
  assert.equal(ssh.length, 1)
  assert.deepEqual(ssh[0].args, ['-G', '--', 'public-alias'])
  assert.ok(ssh[0].opts.timeout > 0)
  assert.equal(ssh[0].opts.killSignal, 'SIGKILL')
  const unavailable = run(['head-pr', '--branch', 'feat'], [json([])])
  assert.equal(unavailable.result.exit, 0)
  assert.equal(unavailable.spawns.filter(({ cmd }) => cmd === 'ssh').length, 1)
})

test('empty host configuration preserves implicit github.com selection', () => {
  const { result } = run(
    ['head-pr', '--branch', 'feat'],
    [json([])],
    {},
    'git@github.com:acme/widgets.git',
    '',
    '{}\n',
  )
  assert.equal(result.exit, 0)
})

test('GH_HOST refuses unrelated remotes without querying either quota', () => {
  const { result, calls } = run(['head-pr', '--branch', 'feat'], [], {
    GH_HOST: 'missing.example.test',
  })
  assert.equal(result.exit, 1)
  assert.equal(calls.length, 0)
})

test('new verbs reject missing inputs and stray options', () => {
  for (const argv of [
    ['head-pr'],
    ['landed'],
    ['landed', '--url', url, '--pr', '7'],
    ['head-pr', '--branch', 'feat', '--fresh'],
    ['landed', '--url', url, '--branch', 'feat'],
  ])
    assert.throws(() => main(argv), UsageError)
})

test('landed absolute github.com URLs pin both transports despite Enterprise defaults', () => {
  for (const env of [{ GH_HOST: 'ghe.example.test' }, {}]) {
    for (const fallback of [false, true]) {
      const { result, calls } = run(
        ['landed', '--url', url],
        fallback ? [refused, json(graph)] : [json(rest)],
        env,
        'https://ghe.example.test/acme/widgets.git',
        '',
        'ghe.example.test:\n  oauth_token: fixture-secret\n',
      )
      assert.equal(result.exit, 0)
      for (const { args } of calls) {
        if (args[0] === 'api') assert.equal(args[args.indexOf('--hostname') + 1], 'github.com')
        else assert.equal(args[args.indexOf('--repo') + 1], 'github.com/acme/widgets')
      }
    }
  }
})

test('Windows config fallback uses USERPROFILE before HOME', () => {
  const root = mkdtempSync(join(tmpdir(), 'gh-profile-'))
  const platform = Object.getOwnPropertyDescriptor(process, 'platform')
  const wrong = join(root, 'wrong'),
    profile = join(root, 'profile')
  try {
    for (const [home, host] of [
      [wrong, 'github.com'],
      [profile, 'ghe.example.test'],
    ]) {
      mkdirSync(join(home, '.config', 'gh'), { recursive: true })
      writeFileSync(
        join(home, '.config', 'gh', 'hosts.yml'),
        `${host}:\n  oauth_token: fixture-secret\n`,
      )
    }
    Object.defineProperty(process, 'platform', { value: 'win32' })
    const { result, calls } = run(
      ['head-pr', '--branch', 'feat'],
      [json([])],
      { GH_CONFIG_DIR: '', HOME: wrong, USERPROFILE: profile },
      'https://ghe.example.test/acme/widgets.git',
    )
    assert.equal(result.exit, 0)
    assert.equal(calls[0].args.at(-1), 'ghe.example.test/acme/widgets')
  } finally {
    Object.defineProperty(process, 'platform', platform)
    rmSync(root, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('missing home configuration is relative to the command cwd', () => {
  const root = mkdtempSync(join(tmpdir(), 'gh-cwd-'))
  const originalCwd = process.cwd()
  try {
    mkdirSync(join(root, '.config', 'gh'), { recursive: true })
    writeFileSync(
      join(root, '.config', 'gh', 'hosts.yml'),
      'ghe.example.test:\n  oauth_token: fixture-secret\n',
    )
    process.chdir(root)
    const { result, calls } = run(
      ['head-pr', '--branch', 'feat'],
      [json([])],
      { GH_CONFIG_DIR: '', HOME: '', USERPROFILE: '' },
      'https://ghe.example.test/acme/widgets.git',
    )
    assert.equal(result.exit, 0)
    assert.equal(calls[0].args.at(-1), 'ghe.example.test/acme/widgets')
  } finally {
    process.chdir(originalCwd)
    rmSync(root, { recursive: true, force: true, maxRetries: 3 })
  }
})

for (const [name, hosts, origin, expected] of [
  [
    'document marker comment',
    '--- # managed configuration\ngithub.com:\n  user: agent\n',
    'https://github.com/acme/widgets.git',
    'github.com/acme/widgets',
  ],
  [
    'flow mapping',
    '{github.com: {user: agent}}\n',
    'https://github.com/acme/widgets.git',
    'github.com/acme/widgets',
  ],
  [
    'Enterprise flow mapping',
    '{ghe.example.test: {user: agent, oauth_token: "fixture-secret: {,} # data"}}\n',
    'https://ghe.example.test/acme/widgets.git',
    'ghe.example.test/acme/widgets',
  ],
  [
    'quoted keys, directives and anchors',
    '\uFEFF%YAML 1.2\n--- # managed\n"gith\\u0075b.com": &account\n  user: agent\n  oauth_token: |\n    fixture-secret\n    ghe.other.test: only credential text\n"ghe.example.test": *account\n...\n',
    'origin\thttps://github.com/acme/widgets.git (fetch)\nupstream\thttps://ghe.other.test/other/widgets.git (fetch)',
    'github.com/acme/widgets',
  ],
  [
    'explicit mapping key',
    '---\n? "github.com"\n: {user: agent}\n',
    'https://github.com/acme/widgets.git',
    'github.com/acme/widgets',
  ],
]) {
  test(`implicit repository accepts gh hosts YAML with ${name}`, () => {
    for (const argv of [['head-pr', '--branch', 'feat'], ['session-pr'], ['landed', '--pr', '7']]) {
      for (const fallback of [false, true]) {
        const landed = argv[0] === 'landed'
        const primary = landed ? rest : []
        const alternate = landed ? graph : [[]]
        const { result, calls } = run(
          argv,
          fallback ? [refused, json(alternate)] : [json(primary)],
          {},
          origin,
          '',
          hosts,
        )
        assert.equal(result.exit, 0)
        for (const { args } of calls) {
          if (args[0] === 'api')
            assert.equal(args[args.indexOf('--hostname') + 1], expected.split('/')[0])
          else assert.equal(args[args.indexOf('--repo') + 1], expected)
        }
        assert.ok(!JSON.stringify(result).includes('fixture-secret'))
      }
    }
  })
}
