import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'
import { renderReport } from '../review-verdict.mjs'
import { buildBlockers, encodeBlockerTrailer } from '../verify/verify-blockers.mjs'
import { main, outcomesComment } from './repair-blockers.mjs'
import { reviewRange } from './receipt-carry.mjs'

const finding = {
  id: 'rewrite-finding',
  severity: 'P2',
  file: 'bug.txt',
  line: 1,
  title: 'Bug remains',
  evidence: 'broken',
  introduced: true,
  verified: true,
  fixed: false,
}
const other = { ...finding, id: 'other', file: 'other.txt', title: 'Another bug' }

function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'repair-rewrites-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const raw = (args, options = {}) =>
    spawnSync('git', args, {
      cwd: dir,
      encoding: 'utf8',
      ...options,
    })
  const git = (...args) => {
    const r = raw(args)
    assert.equal(r.status, 0, r.stderr)
    return r.stdout.trim()
  }
  const readGit = (args) => {
    const r = raw(args)
    return { ok: !r.error && r.status === 0, status: r.status, output: r.stdout.trim() }
  }
  const commit = (file, value, message) => {
    writeFileSync(join(dir, file), value)
    git('add', file)
    git('commit', '-qm', message)
    return git('rev-parse', 'HEAD')
  }
  git('init', '-q', '--initial-branch=main')
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.name', 'Test')
  git('config', 'user.email', 'test@example.invalid')
  const base = commit('base.txt', 'base\n', 'base')
  git('update-ref', 'refs/remotes/origin/main', base)
  git('checkout', '-qb', 'feature')
  const asserted = commit('bug.txt', 'bug\n', 'feature')
  const blockers = {
    version: 1,
    pr: 7,
    head: asserted,
    blockers: buildBlockers({ reviewFindings: [finding, other] }),
  }
  const id = blockers.blockers[0].id
  const outcomes = (commit, blockersHead = asserted) => ({
    version: 1,
    blockersHead,
    pushedHead: git('rev-parse', 'HEAD'),
    outcomes: [
      { id, outcome: 'fixed', commit },
      { id: blockers.blockers[1].id, outcome: 'not-fixable', reason: 'separate defect' },
    ],
  })
  const rebase = () => {
    git('checkout', '-q', 'main')
    commit('base2.txt', 'advanced\n', 'advance main')
    git('update-ref', 'refs/remotes/origin/main', 'HEAD')
    git('checkout', '-q', 'feature')
    git('rebase', 'main')
  }
  const carry = async ({
    fixed,
    local = false,
    marker = blockers,
    writer = true,
    assertionHead = asserted,
    extra = [],
  }) => {
    const report = renderReport({
      status: 'pass',
      verdict: {
        status: 'pass',
        base,
        head: asserted,
        findings: [finding, other].map((f) => ({ ...f, source: 'carried', assertionHead })),
      },
    })
    const bodies = [{ body: report, writer: true }]
    if (marker) bodies.push({ body: encodeBlockerTrailer(marker), writer })
    if (!local) bodies.push({ body: outcomesComment(fixed), writer: true })
    bodies.push(...extra)
    const comments = bodies.map((c, i) => ({
      id: i + 1,
      body: c.body,
      user: { login: c.writer ? 'writer' : 'reader' },
      author_association: 'MEMBER',
      created_at: `2026-01-${String(i + 1).padStart(2, '0')}T00:00:00Z`,
    }))
    const read = (_cmd, args) =>
      args[1].includes('/permission')
        ? { ok: true, payload: { permission: args[1].includes('/writer/') ? 'write' : 'read' } }
        : { ok: true, payload: [comments] }
    const out = join(dir, 'carry.json')
    rmSync(out, { force: true })
    const args = ['carry', '--pr', '7', '--repo', 'o/r', '--out', out]
    if (local) {
      const file = join(dir, 'outcomes.json')
      writeFileSync(file, JSON.stringify(fixed))
      args.push('--outcomes', file)
    }
    await main(args, { read, git: readGit })
    return JSON.parse(readFileSync(out, 'utf8'))
  }
  return { dir, git, raw, readGit, commit, base, asserted, blockers, outcomes, rebase, carry }
}

test('local rebase outcomes require the exact trusted posted blocker set', async (t) => {
  const f = fixture(t)
  f.commit('bug.txt', 'fixed\n', 'fix')
  f.rebase()
  assert.equal(f.readGit(['merge-base', '--is-ancestor', f.asserted, 'HEAD']).ok, false)
  const fixed = f.outcomes(f.git('rev-parse', 'HEAD'))
  assert.deepEqual(
    (await f.carry({ fixed, local: true })).map((x) => x.id),
    ['other'],
  )
  const wrongIds = { ...f.blockers, blockers: f.blockers.blockers.slice(1) }
  for (const change of [
    { marker: null },
    { writer: false },
    { marker: { ...f.blockers, pr: 8 } },
    { marker: wrongIds },
  ]) {
    await assert.rejects(f.carry({ fixed, local: true, ...change }), /blockersHead/)
    assert.equal(existsSync(join(f.dir, 'carry.json')), false)
  }
})

test('local folded outcomes discharge the fix with trusted pre-fold provenance', async (t) => {
  const f = fixture(t)
  f.commit('bug.txt', 'fixed\n', `fixup! feature`)
  const r = f.raw(['rebase', '-i', '--autosquash', f.base], {
    env: { ...process.env, GIT_SEQUENCE_EDITOR: 'true' },
  })
  assert.equal(r.status, 0, r.stderr)
  assert.equal(f.readGit(['merge-base', '--is-ancestor', f.asserted, 'HEAD']).ok, false)
  const fixed = f.outcomes(f.git('rev-parse', 'HEAD'))
  assert.deepEqual(
    (await f.carry({ fixed, local: true })).map((x) => x.id),
    ['other'],
  )
})

for (const rewrite of ['rebase', 'message', 'fold'])
  test(`posted fixes survive ${rewrite} only with patch equivalence`, async (t) => {
    const f = fixture(t)
    const original = f.commit('bug.txt', 'fixed\n', 'fix')
    const fixed = f.outcomes(original)
    if (rewrite === 'rebase') f.rebase()
    else if (rewrite === 'message') f.git('commit', '--amend', '-qm', 'tagged fix')
    else {
      f.git('reset', '--soft', f.base)
      f.git('commit', '-qm', 'folded feature and fix')
    }
    assert.equal(f.readGit(['merge-base', '--is-ancestor', original, 'HEAD']).ok, false)
    const result = await f.carry({ fixed })
    assert.deepEqual(
      result.map((x) => x.id),
      rewrite === 'fold' ? ['rewrite-finding', 'other'] : ['other'],
    )
  })

test('posted fixes discharge rewritten assertions but retain a later recurrence', async (t) => {
  const f = fixture(t)
  f.rebase()
  const blockersHead = f.git('rev-parse', 'HEAD')
  const fix = f.commit('bug.txt', 'fixed\n', 'fix')
  const fixed = f.outcomes(fix, blockersHead)
  assert.deepEqual(
    (await f.carry({ fixed })).map((x) => x.id),
    ['other'],
  )
  const recurrence = f.commit('bug.txt', 'bug again\n', 'recurrence')
  assert.deepEqual(
    (await f.carry({ fixed, assertionHead: recurrence })).map((x) => x.id),
    ['rewrite-finding', 'other'],
  )
})

test('synthetic assertions resolve only through a matching real pushed tree', async (t) => {
  const f = fixture(t)
  f.rebase()
  const pushed = f.git('rev-parse', 'HEAD')
  const range = reviewRange((_cmd, args) => f.readGit(args), 'main', f.asserted, pushed)
  assert.equal(range.replayed, true)
  const fix = f.commit('bug.txt', 'fixed\n', 'fix')
  const fixed = f.outcomes(fix, pushed)
  assert.deepEqual(
    (await f.carry({ fixed, assertionHead: range.head })).map((x) => x.id),
    ['other'],
  )
  for (const [tree, parent, named] of [
    ['HEAD^{tree}', f.base, pushed.slice(0, 12)],
    ['HEAD^{tree}', f.base, '0'.repeat(12)],
    [`${pushed}^{tree}`, 'origin/main', fix.slice(0, 12)],
  ]) {
    const mismatch = f.git(
      'commit-tree',
      tree,
      '-p',
      parent,
      '-m',
      `boss-repair: tree of pushed head ${named}`,
    )
    assert.deepEqual(
      (await f.carry({ fixed, assertionHead: mismatch })).map((x) => x.id),
      ['rewrite-finding', 'other'],
    )
  }
})
