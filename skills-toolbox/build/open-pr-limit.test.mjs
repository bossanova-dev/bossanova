import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import {
  checkOpenPrLimit,
  createOpenPrLister,
  decideOpenPrLimit,
  readOpenPrLimit,
  runCli,
} from './open-pr-limit.mjs'

const CLI = fileURLToPath(new URL('./open-pr-limit.mjs', import.meta.url))

const numbers = (n) => Array.from({ length: n }, (_, i) => 100 + i)

test('readOpenPrLimit takes the flag in both spellings and leaves the rest verbatim', () => {
  assert.deepEqual(readOpenPrLimit(['headless', '--max-open-prs', '8', '--label', 'x']), {
    limit: 8,
    rest: ['headless', '--label', 'x'],
  })
  assert.deepEqual(readOpenPrLimit(['--max-open-prs=12']), { limit: 12, rest: [] })
  assert.deepEqual(readOpenPrLimit(['ABC-1', '--headless']), {
    limit: null,
    rest: ['ABC-1', '--headless'],
  })
})

test('readOpenPrLimit refuses a missing, repeated or non-positive-integer value', () => {
  for (const argv of [
    ['--max-open-prs'],
    ['--max-open-prs', '--headless'],
    ['--max-open-prs='],
    ['--max-open-prs', '0'],
    ['--max-open-prs', '-1'],
    ['--max-open-prs', '2.5'],
    ['--max-open-prs', '1e3'],
    ['--max-open-prs', '99999999999999999999'],
    ['--max-open-prs', '1', '--max-open-prs=2'],
  ]) {
    assert.throws(() => readOpenPrLimit(argv), /--max-open-prs/, JSON.stringify(argv))
  }
})

test('decideOpenPrLimit blocks exactly at the limit and names count, limit and PRs', () => {
  assert.deepEqual(decideOpenPrLimit({ limit: 3, prs: [1, 2], truncated: false }), {
    blocked: false,
    reason: null,
  })
  assert.deepEqual(decideOpenPrLimit({ limit: 3, prs: [1, 2, 3], truncated: false }), {
    blocked: true,
    reason: '3 open PRs reach --max-open-prs 3 (#1, #2, #3)',
  })
  assert.equal(
    decideOpenPrLimit({ limit: 8, prs: numbers(16), truncated: false }).reason,
    '16 open PRs reach --max-open-prs 8 (#100, #101, #102, #103, #104, #105, #106, #107, #108, #109 and 6 more)',
  )
  assert.equal(
    decideOpenPrLimit({ limit: 8, prs: numbers(100), truncated: true }).reason,
    '100+ open PRs reach --max-open-prs 8 (#100, #101, #102, #103, #104, #105, #106, #107, #108, #109 and 90+ more)',
  )
})

test('checkOpenPrLimit never lists without a limit and fails closed on a read error', async () => {
  let calls = 0
  const failing = async () => {
    calls += 1
    return { error: 'command-failed' }
  }
  assert.deepEqual(await checkOpenPrLimit({ limit: null, listOpenPrs: failing }), {
    blocked: false,
    reason: null,
  })
  assert.equal(calls, 0)
  await assert.rejects(checkOpenPrLimit({ limit: 2, listOpenPrs: failing }), /fails closed/)
  assert.equal(calls, 1)
})

test('the gh lister asks past the limit, includes drafts and reports a saturated page', async () => {
  const seen = []
  const run = (cmd, args) => {
    seen.push([cmd, ...args])
    const fetch = Number(args.at(-1))
    return {
      status: 0,
      stdout: JSON.stringify(numbers(Math.min(fetch, 150)).map((number) => ({ number }))),
    }
  }
  const lister = createOpenPrLister({ run })
  assert.deepEqual(await lister(9), { prs: numbers(9), truncated: true })
  assert.deepEqual(seen[0], [
    'gh',
    'pr',
    'list',
    '--state',
    'open',
    '--json',
    'number',
    '--limit',
    '9',
  ])
  const small = await checkOpenPrLimit({ limit: 8, listOpenPrs: lister })
  assert.equal(seen[1].at(-1), '100', 'one full gh page for an ordinary limit')
  assert.match(small.reason, /^100\+ open PRs reach --max-open-prs 8/)
  const large = await checkOpenPrLimit({ limit: 120, listOpenPrs: lister })
  assert.equal(seen[2].at(-1), '121', 'a limit past one page still reads one PR beyond it')
  assert.match(large.reason, /^121\+ open PRs reach --max-open-prs 120/)
})

test('the gh lister returns failures as values', async () => {
  const cases = [
    [{ error: { code: 'ETIMEDOUT' } }, 'ETIMEDOUT'],
    [{ status: 1, stdout: '' }, 'command-failed'],
    [{ status: 0, stdout: 'not json' }, 'lookup-failed'],
    [{ status: 0, stdout: '[{"number":"7"}]' }, 'invalid-pr-list'],
  ]
  for (const [result, error] of cases) {
    assert.deepEqual(await createOpenPrLister({ run: () => result })(5), { error })
  }
  const hung = createOpenPrLister({ run: () => new Promise(() => {}), timeoutMs: 20 })
  assert.deepEqual(await hung(5), { error: 'timeout' })
})

test('runCli maps the decision to exit codes and always carries a reason on non-zero', async () => {
  const lister = (prs) => async () => ({ prs, truncated: false })
  assert.deepEqual(await runCli(['--', 'headless'], { listOpenPrs: lister(numbers(50)) }), {
    code: 0,
    out: { limit: null, blocked: false, reason: null },
  })
  const under = await runCli(['--', '--max-open-prs', '3'], { listOpenPrs: lister([1, 2]) })
  assert.equal(under.code, 0)
  const over = await runCli(['--', '--max-open-prs', '3'], { listOpenPrs: lister([1, 2, 3]) })
  assert.equal(over.code, 1)
  assert.match(over.out.reason, /^3 open PRs reach --max-open-prs 3/)
  const unreadable = await runCli(['--', '--max-open-prs=3'], {
    listOpenPrs: async () => ({ error: 'timeout' }),
  })
  assert.equal(unreadable.code, 69)
  assert.match(unreadable.out.reason, /fails closed/)
  for (const argv of [
    ['--', '--max-open-prs', 'x'],
    ['--max-open-prs', '3'],
  ]) {
    const refused = await runCli(argv, { listOpenPrs: lister([]) })
    assert.equal(refused.code, 64, JSON.stringify(argv))
    assert.ok(refused.out.reason, JSON.stringify(argv))
  }
})

test('the CLI without the flag exits 0 without calling gh', () => {
  const result = spawnSync(process.execPath, [CLI, '--', 'headless', '--label', 'x'], {
    encoding: 'utf8',
    env: { ...process.env, PATH: '/nonexistent' },
  })
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), { limit: null, blocked: false, reason: null })
})
