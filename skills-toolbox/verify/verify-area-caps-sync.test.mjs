import assert from 'node:assert/strict'
import { test } from 'node:test'
import { writeFileSync } from 'node:fs'
import { main } from './verify-area-caps-sync.mjs'
import { parseClearComment } from './verify-area-caps.mjs'

const NOW = '2026-11-08T00:00:00.000Z'
const statePath = '/clone/git/boss-verify/area-caps.json'
const escape = (number, day) => ({
  original: { number },
  fix: { number: number + 10, mergedAt: `2026-11-0${day}T00:00:00.000Z` },
  levelAtMerge: 'high',
  areas: [{ area: 'ui', paths: ['src/View.tsx'] }],
})
function fixture(overrides = {}) {
  const files = new Map([[statePath, 'previous state']])
  const calls = [],
    writes = [],
    output = [],
    errors = []
  const deps = {
    now: () => NOW,
    stdout: (text) => output.push(text),
    stderr: (text) => errors.push(text),
    fs: {
      mkdirSync() {},
      writeFileSync: (path, text) => {
        files.set(path, text)
        writes.push(path)
      },
      renameSync: (from, to) => {
        assert.ok(files.has(from))
        files.set(to, files.get(from))
        files.delete(from)
      },
      rmSync: (path) => files.delete(path),
    },
    run: (command, args) => {
      calls.push([command, args])
      return {
        status: 0,
        stdout:
          command === 'git'
            ? '/clone/git\n'
            : args[0] === 'api'
              ? 'operator\n'
              : args[0] === 'repo'
                ? 'acme/project\n'
                : '',
      }
    },
    trace: async (options) => {
      calls.push(['trace', options])
      return { failures: [], truncated: false }
    },
    list: async (options) => {
      calls.push(['list', options])
      return { escapes: [escape(1, 2), escape(2, 5)], clears: [], unreadable: 0 }
    },
    ...overrides,
  }
  return { deps, files, calls, writes, output, errors }
}

test('refresh traces three days, lists all records, and replaces state atomically', async () => {
  const f = fixture()
  assert.equal(await main(['refresh', '--repo', 'acme/project'], f.deps), 0)
  const state = JSON.parse(f.files.get(statePath))
  assert.equal(state.caps[0].key, 'ui')
  assert.deepEqual(state.trace, { ok: true })
  assert.equal(state.refreshedAt, NOW)
  assert.equal(f.calls.find(([cmd]) => cmd === 'trace')[1].since, '2026-11-05T00:00:00.000Z')
  assert.equal(f.calls.find(([cmd]) => cmd === 'list')[1].since, '1970-01-01T00:00:00.000Z')
  assert.equal(f.writes.length, 2)
  assert.notEqual(f.writes[0], statePath)
})

test('explicit mixed-case repository names use GitHub canonical identity throughout refresh', async () => {
  const f = fixture()
  assert.equal(await main(['refresh', '--repo', 'Acme/Project'], f.deps), 0)
  assert.equal(JSON.parse(f.files.get(statePath)).repo, 'acme/project')
  assert.equal(f.calls.find(([cmd]) => cmd === 'trace')[1].repo, 'acme/project')
  assert.equal(f.calls.find(([cmd]) => cmd === 'list')[1].repo, 'acme/project')
})

test('trace failure keeps standing caps and records failure; list failure leaves prior state', async () => {
  for (const trace of [
    async () => {
      throw Error('offline')
    },
    async () => ({ failures: [{ reason: 'rate-limited' }] }),
    async () => ({ failures: [], truncated: true }),
  ]) {
    const f = fixture({ trace })
    assert.equal(await main(['refresh', '--repo', 'acme/project'], f.deps), 1)
    const state = JSON.parse(f.files.get(statePath))
    assert.equal(state.caps.length, 1)
    assert.equal(state.trace.ok, false)
    assert.ok(state.trace.reason)
  }
  const f = fixture({
    list: async () => {
      throw Error('offline')
    },
  })
  assert.equal(await main(['refresh', '--repo', 'acme/project'], f.deps), 1)
  assert.equal(f.files.get(statePath), 'previous state')
  assert.deepEqual(JSON.parse(f.files.get(`${statePath}.refresh-result`)), { completedAt: NOW })
})

test('clear refuses inactive keys without writes and names active keys', async () => {
  const f = fixture()
  assert.equal(
    await main(
      ['clear', '--repo', 'acme/project', '--area', 'other:src', '--reason', 'reviewed'],
      f.deps,
    ),
    1,
  )
  assert.equal(f.writes.length, 0)
  assert.match(f.errors.join(''), /ui/)
  assert.equal(
    f.calls.some(([cmd, args]) => cmd === 'gh' && args[0] === 'pr'),
    false,
  )
})

test('clear publishes on newest original, records operator, and immediately removes cap', async () => {
  const f = fixture()
  assert.equal(
    await main(
      ['clear', '--repo', 'acme/project', '--area', 'ui', '--reason', 'guard repaired'],
      f.deps,
    ),
    0,
  )
  const posted = f.calls.filter(([cmd, args]) => cmd === 'gh' && args[0] === 'pr')
  assert.equal(posted.length, 1)
  const args = posted[0][1]
  assert.equal(args[2], '2')
  const clear = parseClearComment(args[args.indexOf('--body') + 1])
  assert.equal(clear.clearedBy, 'operator')
  assert.equal(clear.reason, 'guard repaired')
  assert.equal(clear.through, 12)
  assert.deepEqual(JSON.parse(f.files.get(statePath)).caps, [])
})

test('dry-run publishes and writes nothing; status does not trace; retain fixture evidence', async () => {
  const f = fixture()
  assert.equal(
    await main(
      [
        'clear',
        '--repo',
        'acme/project',
        '--area',
        'ui',
        '--reason',
        'guard repaired',
        '--dry-run',
      ],
      f.deps,
    ),
    0,
  )
  assert.equal(f.writes.length, 0)
  assert.equal(
    f.calls.some(([cmd, args]) => cmd === 'gh' && args[0] === 'pr'),
    false,
  )
  const status = fixture()
  assert.equal(await main(['status', '--repo', 'acme/project'], status.deps), 0)
  assert.equal(
    status.calls.some(([cmd]) => cmd === 'trace'),
    false,
  )
  if (process.env.AREA_CAPS_FIXTURE_EVIDENCE)
    writeFileSync(
      process.env.AREA_CAPS_FIXTURE_EVIDENCE,
      JSON.stringify(
        { status: JSON.parse(status.output.join('')), clearDryRun: JSON.parse(f.output.join('')) },
        null,
        2,
      ),
    )
})

test('history reports episodes and filters since without writing', async () => {
  const f = fixture()
  assert.equal(await main(['history', '--repo', 'acme/project'], f.deps), 0)
  assert.equal(JSON.parse(f.output.join('')).episodes[0].key, 'ui')
  assert.equal(f.writes.length, 0)
  assert.ok(!f.calls.some(([command]) => command === 'trace'))
  assert.ok(!f.calls.some(([command, args]) => command === 'gh' && args[0] === 'pr'))
  f.output.length = 0
  assert.equal(await main(['history', '--repo', 'acme/project', '--since', NOW], f.deps), 0)
  assert.deepEqual(JSON.parse(f.output.join('')).episodes, [])
})

test('strict usage errors exit 64 before any I/O', async () => {
  for (const args of [
    [],
    ['unknown'],
    ['status', '--unknown'],
    ['clear', '--area', 'ui'],
    ['clear', '--reason', 'done'],
    ['clear', '--area', 'ui', '--reason', '  '],
    ['status', '--dry-run'],
    ['history', '--since', 'invalid'],
    ['refresh', '--area', 'ui'],
  ]) {
    const f = fixture()
    assert.equal(await main(args, f.deps), 64, JSON.stringify(args))
    assert.equal(f.calls.length, 0)
    assert.equal(f.writes.length, 0)
  }
})

test('history preserves clear attribution alongside the next cap episode', async () => {
  const clear = {
    v: 1,
    repo: 'acme/project',
    key: 'ui',
    clearedAt: '2026-11-06T00:00:00.000Z',
    clearedBy: 'operator',
    reason: 'guard repaired',
    through: 12,
  }
  const f = fixture({
    list: async () => ({
      escapes: [escape(1, 2), escape(2, 5), escape(3, 7), escape(4, 8)],
      clears: [clear],
      unreadable: 0,
    }),
  })
  assert.equal(
    await main(
      ['history', '--repo', 'acme/project', '--since', '2026-11-06T00:00:00.000Z'],
      f.deps,
    ),
    0,
  )
  const { episodes } = JSON.parse(f.output.join(''))
  assert.equal(episodes.length, 2)
  assert.equal(episodes[0].clearedBy, 'operator')
  assert.equal(episodes[0].reason, 'guard repaired')
  assert.equal(episodes[0].clearedAt, clear.clearedAt)
  assert.equal(episodes[1].clearedAt, undefined)
})

test('a failed clear publication leaves the standing local state intact', async () => {
  const f = fixture()
  const run = f.deps.run
  f.deps.run = (command, args) =>
    command === 'gh' && args[0] === 'pr' ? { status: 1, stderr: 'offline' } : run(command, args)
  assert.equal(
    await main(
      ['clear', '--repo', 'acme/project', '--area', 'ui', '--reason', 'guard repaired'],
      f.deps,
    ),
    1,
  )
  assert.equal(f.files.get(statePath), 'previous state')
  assert.equal(f.writes.length, 0)
})

test('a failed atomic state replacement keeps old state and reports failure', async () => {
  const f = fixture()
  f.deps.fs.renameSync = () => {
    throw Error('disk unavailable')
  }
  assert.equal(await main(['status', '--repo', 'acme/project'], f.deps), 1)
  assert.equal(f.files.get(statePath), 'previous state')
  assert.equal(f.files.size, 1)
})

test('clear drops secret-shaped reasons from both publication and JSON output', async () => {
  const reason = 'api_key=private-example-token'
  for (const dry of [[], ['--dry-run']]) {
    const f = fixture()
    assert.equal(
      await main(
        ['clear', '--repo', 'acme/project', '--area', 'ui', '--reason', reason, ...dry],
        f.deps,
      ),
      0,
    )
    const output = f.output.join('')
    assert.ok(!output.includes('private-example-token'))
    assert.equal(JSON.parse(output).clear.reason, '')
    for (const [, args] of f.calls.filter(
      ([command, args]) => command === 'gh' && args[0] === 'pr',
    ))
      assert.ok(!args.join(' ').includes('private-example-token'))
  }
})
