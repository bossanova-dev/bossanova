import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import path from 'node:path'
import { readBranchContext } from './verify-branch-context.mjs'
import { gitFixture } from './verify-git-fixture.mjs'

const REPO = 'acme/widgets'
const url = (/** @type {number} */ n) => `https://github.com/${REPO}/pull/${n}`

/**
 * A throwaway repository that is its own `origin`, and a forge whose open PRs are `pulls`. Every
 * git read runs for real in the repository.
 * @param {import('node:test').TestContext} t
 */
function forge(t) {
  const fx = gitFixture(t)
  fx.git('remote', 'add', 'origin', fx.dir)
  /** @type {any[]} */
  const pulls = []
  /** @param {string} name */
  const commit = (name) => {
    writeFileSync(path.join(fx.dir, `${name}.txt`), `${name}\n`)
    fx.git('add', '--', `${name}.txt`)
    return fx.commit(name)
  }
  /** @param {number} number @param {string} head @param {string} [base] @param {string} [title] */
  const open = (number, head, base = 'main', title = `PR ${number}`) =>
    pulls.push({
      number,
      title,
      html_url: url(number),
      head: { ref: head, sha: fx.git('rev-parse', head) },
      base: { ref: base },
    })
  /** @type {import('./verify-branch-context.mjs').Read} */
  const read = (command, args, { input, json = false } = {}) => {
    if (command === 'gh') {
      const route = args[1]
      if (route === `repos/${REPO}`) return { ok: true, payload: { default_branch: 'main' } }
      if (route.startsWith(`repos/${REPO}/pulls?`)) return { ok: true, payload: pulls }
      return { ok: false, payload: json ? { message: 'Not Found' } : undefined }
    }
    const out = spawnSync(command, args, { cwd: fx.dir, input, encoding: 'utf8' })
    return { ok: out.status === 0, output: out.stdout }
  }
  const context = (/** @type {number} */ pr) => readBranchContext({ read, repo: REPO, pr })
  return { ...fx, commit, open, context }
}

const parent = (/** @type {Record<string, unknown>} */ fields) => ({
  title: `PR ${fields.pr}`,
  url: url(Number(fields.pr)),
  how: 'content',
  ...fields,
})

test('a copy of a parent that was rebased since counts the parent commits it still carries', (t) => {
  const f = forge(t)
  f.commit('root')
  f.git('checkout', '-q', '-b', 'parent')
  for (const n of [1, 2, 3, 4]) f.commit(`p${n}`)
  f.git('checkout', '-q', '-b', 'child')
  f.commit('c1')
  const copied = f.git('rev-parse', 'child~1')
  f.git('checkout', '-q', 'main')
  f.commit('m1')
  f.git('checkout', '-q', 'parent')
  f.git('rebase', '-q', 'main')
  for (const n of [5, 6]) f.commit(`p${n}`)
  assert.notEqual(f.git('rev-parse', 'parent~2'), copied, 'the rebase gave p4 a new SHA')
  f.open(10, 'parent')
  f.open(12, 'child')

  assert.deepEqual(f.context(12), {
    base: 'main',
    default: 'main',
    stackedOn: [parent({ pr: 10, branch: 'parent', shared: 4, total: 6 })],
  })
  assert.equal(f.context(10), null, 'a younger PR that carries its commits is no parent')
})

test('copies of a parent merged under new SHAs stack nobody on it or on each other', (t) => {
  const f = forge(t)
  f.commit('root')
  f.git('checkout', '-q', '-b', 'parent')
  for (const n of [1, 2, 3, 4]) f.commit(`p${n}`)
  f.git('checkout', '-q', '-b', 'older')
  f.commit('o1')
  f.git('checkout', '-q', 'parent')
  f.git('checkout', '-q', '-b', 'child')
  f.commit('c1')
  f.git('checkout', '-q', 'main')
  f.commit('m1')
  f.git('checkout', '-q', 'parent')
  f.git('rebase', '-q', 'main')
  f.git('checkout', '-q', 'main')
  f.git('merge', '-q', '--ff-only', 'parent')
  f.open(11, 'older')
  f.open(12, 'child')

  assert.equal(f.context(12), null, '#11 still carries the old copies, but main has the change')
  assert.equal(f.context(11), null)
})

test('a PR whose base is another open PR head branch is stacked on it by base', (t) => {
  const f = forge(t)
  f.commit('root')
  f.git('checkout', '-q', '-b', 'feature-a')
  f.commit('a1')
  f.commit('a2')
  f.git('checkout', '-q', '-b', 'feature-b')
  f.commit('b1')
  f.open(20, 'feature-a')
  f.open(21, 'feature-b', 'feature-a', '[#21] [FRE-9] Build on A')

  assert.deepEqual(f.context(21), {
    base: 'feature-a',
    default: 'main',
    stackedOn: [parent({ pr: 20, branch: 'feature-a', shared: 2, total: 2, how: 'base' })],
  })
  f.git('checkout', '-q', 'main')
  f.git('checkout', '-q', '-b', 'feature-c')
  f.commit('c1')
  f.open(22, 'feature-c', 'feature-a')
  assert.deepEqual(
    f.context(22)?.stackedOn,
    [parent({ pr: 20, branch: 'feature-a', shared: 0, total: 2, how: 'base' })],
    'the base alone stacks a PR that carries none of the parent commits',
  )
})

test('a PR into another branch says where it merges and is stacked on nothing', (t) => {
  const f = forge(t)
  f.commit('root')
  f.git('checkout', '-q', '-b', 'staging')
  f.commit('s1')
  f.git('checkout', '-q', '-b', 'hotfix')
  f.commit('h1')
  f.open(30, 'hotfix', 'staging')

  assert.deepEqual(f.context(30), { base: 'staging', default: 'main', stackedOn: [] })
})

test('a plain PR into the default branch has no branch context', (t) => {
  const f = forge(t)
  f.commit('root')
  f.git('checkout', '-q', '-b', 'other')
  f.commit('o1')
  f.git('checkout', '-q', 'main')
  f.git('checkout', '-q', '-b', 'plain')
  f.commit('x1')
  f.open(39, 'other')
  f.open(40, 'plain')

  assert.equal(f.context(40), null)
})

test('each commit belongs to the oldest open PR that carries it', (t) => {
  const f = forge(t)
  f.commit('root')
  f.git('checkout', '-q', '-b', 'p')
  f.commit('p1')
  f.commit('p2')
  f.git('checkout', '-q', '-b', 'q')
  f.commit('q1')
  f.git('checkout', '-q', 'p')
  f.git('checkout', '-q', '-b', 'r')
  f.commit('r1')
  f.git('checkout', '-q', 'q')
  f.git('checkout', '-q', '-b', 's')
  f.commit('s1')
  f.open(50, 'p')
  f.open(51, 'q')
  f.open(52, 'r')
  f.open(53, 's')

  assert.deepEqual(
    f.context(52)?.stackedOn,
    [parent({ pr: 50, branch: 'p', shared: 2, total: 2 })],
    '#51 also carries p1 and p2, but #50 is older',
  )
  assert.deepEqual(f.context(53)?.stackedOn, [
    parent({ pr: 50, branch: 'p', shared: 2, total: 2 }),
    parent({ pr: 51, branch: 'q', shared: 1, total: 3 }),
  ])
})

test('an unreadable default branch or PR gives no context rather than a guess', (t) => {
  const f = forge(t)
  f.commit('root')
  assert.equal(f.context(99), undefined, 'a PR the forge does not return')
  assert.equal(
    readBranchContext({ read: () => ({ ok: false }), repo: REPO, pr: 1 }),
    undefined,
    'a forge that cannot be read',
  )
})
