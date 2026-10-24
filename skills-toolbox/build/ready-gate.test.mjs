import test from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  READY_GATE_REASONS,
  READY_GATE_WAIVERS,
  commandReader,
  UsageError,
  driftReasons,
  main,
  readyGate,
  readyGateReport,
  reviewReasons,
  reviewedTreeDrift,
} from './ready-gate.mjs'

const HEAD = 'a'.repeat(40),
  MERGED = 'b'.repeat(40),
  OTHER = 'c'.repeat(40),
  MOVED = 'd'.repeat(40),
  TREE = 'e'.repeat(40),
  TREE2 = 'f'.repeat(40)
const GATE_SCRIPT = fileURLToPath(new URL('./ready-gate.mjs', import.meta.url))

// ---------------------------------------------------------------------------
// The shared vocabulary

test('READY_GATE_REASONS is the frozen closed set', () => {
  assert.equal(Object.isFrozen(READY_GATE_REASONS), true)
  assert.deepEqual([...READY_GATE_REASONS].sort(), [
    'review-not-clean',
    'review-verdict-unknown',
    'reviewed-head-unknown',
    'tree-changed-since-review',
    'tree-drift-unknown',
  ])
})
for (const [verdict, reasons] of [
  ['clean', []],
  [' clean\n', []],
  ['capped', ['review-not-clean']],
  ['disabled', ['review-not-clean']],
  ['none', ['review-verdict-unknown']],
  ['', ['review-verdict-unknown']],
  [undefined, ['review-verdict-unknown']],
  [{}, ['review-verdict-unknown']],
  ['CLEAN-ish', ['review-verdict-unknown']],
])
  test(`reviewReasons(${JSON.stringify(verdict)})`, () =>
    assert.deepEqual(reviewReasons(verdict), reasons))
for (const [drift, reasons] of [
  ['none', []],
  ['changed', ['tree-changed-since-review']],
  ['unknown', ['tree-drift-unknown']],
  [undefined, ['tree-drift-unknown']],
  [true, ['tree-drift-unknown']],
])
  test(`driftReasons(${JSON.stringify(drift)})`, () =>
    assert.deepEqual(driftReasons(drift), reasons))

// ---------------------------------------------------------------------------
// readyGate

const cleanGate = {
  reviewVerdict: 'clean',
  reviewedHead: HEAD,
  pushedHead: HEAD,
  treeDrift: 'none',
}
for (const [label, changes, expected] of [
  ['clean, reviewed head shipped unchanged', {}, { ready: true, reasons: [] }],
  [
    'clean after a rebase that kept the patch',
    { pushedHead: OTHER, treeDrift: 'none' },
    { ready: true, reasons: [] },
  ],
  [
    'disabled by the operator, nothing else known',
    { reviewVerdict: 'disabled', reviewedHead: '', pushedHead: '', treeDrift: 'unknown' },
    { ready: false, reasons: ['reviewed-head-unknown'], admittedFor: 'disabled' },
  ],
  ['capped review', { reviewVerdict: 'capped' }, { ready: false, reasons: ['review-not-clean'] }],
  [
    'verdict none',
    { reviewVerdict: 'none' },
    { ready: false, reasons: ['review-verdict-unknown'] },
  ],
  ['verdict missing', { reviewVerdict: '' }, { ready: false, reasons: ['review-verdict-unknown'] }],
  [
    'verdict unrecognised',
    { reviewVerdict: 'CLEAN-ish' },
    { ready: false, reasons: ['review-verdict-unknown'] },
  ],
  [
    'reviewed head missing',
    { reviewedHead: '' },
    { ready: false, reasons: ['reviewed-head-unknown'] },
  ],
  [
    'reviewed head malformed',
    { reviewedHead: 'abc' },
    { ready: false, reasons: ['reviewed-head-unknown'] },
  ],
  [
    'commits after review changed the patch',
    { pushedHead: OTHER, treeDrift: 'changed' },
    { ready: false, reasons: ['tree-changed-since-review'] },
  ],
  [
    'drift could not be read',
    { pushedHead: OTHER, treeDrift: 'unknown' },
    { ready: false, reasons: ['tree-drift-unknown'] },
  ],
  [
    'pushed head unknown, even with a claimed none',
    { pushedHead: '', treeDrift: 'none' },
    { ready: false, reasons: ['tree-drift-unknown'] },
  ],
  [
    'capped with drift',
    { reviewVerdict: 'capped', pushedHead: OTHER, treeDrift: 'changed' },
    { ready: false, reasons: ['review-not-clean', 'tree-changed-since-review'] },
  ],
  [
    'nothing known',
    {
      reviewVerdict: undefined,
      reviewedHead: undefined,
      pushedHead: undefined,
      treeDrift: undefined,
    },
    { ready: false, reasons: ['review-verdict-unknown', 'reviewed-head-unknown'] },
  ],
  // --partial admits a capped review and nothing else.
  [
    'partial admits a capped review',
    { reviewVerdict: 'capped', partial: true },
    { ready: true, reasons: [], admittedFor: 'partial' },
  ],
  [
    'partial still refuses drift on a capped review',
    { reviewVerdict: 'capped', pushedHead: OTHER, treeDrift: 'changed', partial: true },
    { ready: false, reasons: ['tree-changed-since-review'], admittedFor: 'partial' },
  ],
  [
    'partial still refuses unknown drift',
    { pushedHead: OTHER, treeDrift: 'unknown', partial: true },
    { ready: false, reasons: ['tree-drift-unknown'] },
  ],
  [
    'partial still refuses an unknown verdict',
    { reviewVerdict: 'none', partial: true },
    { ready: false, reasons: ['review-verdict-unknown'] },
  ],
  [
    'partial still refuses an unknown reviewed head',
    { reviewVerdict: 'capped', reviewedHead: '', partial: true },
    { ready: false, reasons: ['reviewed-head-unknown'], admittedFor: 'partial' },
  ],
  ['partial on a clean review admits nothing', { partial: true }, { ready: true, reasons: [] }],
  [
    'partial must be exactly true',
    { reviewVerdict: 'capped', partial: 'yes' },
    { ready: false, reasons: ['review-not-clean'] },
  ],
])
  test(`readyGate: ${label}`, () => {
    const verdict = readyGate({ ...cleanGate, ...changes })
    assert.deepEqual(verdict, expected)
    for (const reason of verdict.reasons) assert.ok(READY_GATE_REASONS.includes(reason), reason)
  })

// ---------------------------------------------------------------------------
// reviewedTreeDrift with a fake reader

// bases: head -> merge base; trees: head -> tree; replay: the merge-tree answer.
function fakeRead({ bases = {}, trees = {}, replay = null, failOn = null } = {}) {
  const calls = []
  const read = (command, args) => {
    calls.push([command, ...args])
    if (failOn && args[0] === failOn) return { ok: false, status: 128 }
    if (args[0] === 'merge-base') return { ok: true, output: bases[args[2]] ?? MERGED }
    if (args[0] === 'rev-parse')
      return { ok: true, output: trees[args.at(-1).replace('^{tree}', '')] ?? TREE }
    if (args[0] === 'merge-tree') return replay ?? assert.fail('merge-tree on a shared merge base')
    assert.fail(`unexpected ${command} ${args.join(' ')}`)
  }
  return { read, calls }
}
test('reviewedTreeDrift: equal heads are none without reading the base or git', () => {
  const { read, calls } = fakeRead()
  const base = () => assert.fail('base read for equal heads')
  assert.equal(reviewedTreeDrift(read, base, HEAD, HEAD.toUpperCase()), 'none')
  assert.equal(calls.length, 0)
})
test('reviewedTreeDrift: a shared merge base compares the two trees, never replaying', () => {
  assert.equal(reviewedTreeDrift(fakeRead().read, 'main', HEAD, OTHER), 'none')
  const changed = fakeRead({ trees: { [OTHER]: TREE2 } })
  assert.equal(reviewedTreeDrift(changed.read, 'main', HEAD, OTHER), 'changed')
  assert.equal(
    changed.calls.some(([, verb]) => verb === 'merge-tree'),
    false,
  )
})
test('reviewedTreeDrift: a moved base replays the reviewed head onto the pushed base', () => {
  const bases = { [HEAD]: MERGED, [OTHER]: MOVED }
  const same = fakeRead({ bases, replay: { ok: true, output: `${TREE}\n` } })
  assert.equal(reviewedTreeDrift(same.read, 'main', HEAD, OTHER), 'none')
  assert.deepEqual(
    same.calls.find(([, verb]) => verb === 'merge-tree'),
    ['git', 'merge-tree', '--write-tree', `--merge-base=${MERGED}`, MOVED, HEAD],
  )
  for (const [replay, drift] of [
    [{ ok: true, output: TREE2 }, 'changed'],
    [{ ok: false, status: 1 }, 'changed'],
    [{ ok: false, status: 128 }, 'unknown'],
    [{ ok: false, status: 129 }, 'unknown'],
    [{ ok: false, status: null }, 'unknown'],
    [{ ok: true, output: 'not a tree' }, 'unknown'],
    [{ ok: true, output: '' }, 'unknown'],
  ])
    assert.equal(
      reviewedTreeDrift(fakeRead({ bases, replay }).read, 'main', HEAD, OTHER),
      drift,
      JSON.stringify(replay),
    )
})
test('reviewedTreeDrift: an origin/ base prefix is normalised; the base thunk is read once', () => {
  const { read, calls } = fakeRead()
  let asked = 0
  const base = () => (asked++, 'origin/release')
  assert.equal(reviewedTreeDrift(read, base, HEAD, OTHER), 'none')
  assert.equal(asked, 1)
  const mergeBases = calls.filter(([, verb]) => verb === 'merge-base')
  assert.deepEqual(
    mergeBases.map(([, , ref]) => ref),
    ['origin/release', 'origin/release'],
  )
})
test('reviewedTreeDrift: unknown heads, a missing base or a failed read are unknown', () => {
  const { read } = fakeRead()
  assert.equal(reviewedTreeDrift(read, 'main', undefined, HEAD), 'unknown')
  assert.equal(reviewedTreeDrift(read, 'main', HEAD, 'abc'), 'unknown')
  assert.equal(reviewedTreeDrift(read, '', HEAD, OTHER), 'unknown')
  assert.equal(
    reviewedTreeDrift(read, () => '', HEAD, OTHER),
    'unknown',
  )
  for (const failOn of ['merge-base', 'rev-parse'])
    assert.equal(reviewedTreeDrift(fakeRead({ failOn }).read, 'main', HEAD, OTHER), 'unknown')
  const garbled = fakeRead({ trees: { [OTHER]: 'tree?' } })
  assert.equal(reviewedTreeDrift(garbled.read, 'main', HEAD, OTHER), 'unknown')
})

// ---------------------------------------------------------------------------
// readyGateReport with a fake git/gh; unexpected commands fail the test.

function gateHarness(
  t,
  {
    verdict = 'clean',
    reviewed = HEAD,
    head = HEAD,
    trees = {},
    route = null,
    prBase = 'main',
    remoteHead = 'origin/main',
  } = {},
) {
  const dir = mkdtempSync(join(tmpdir(), 'ready-gate-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  if (verdict !== null)
    writeFileSync(join(dir, 'boss-build-review-verdict'), `REVIEW_VERDICT=${verdict}\n`)
  if (reviewed !== null) writeFileSync(join(dir, 'boss-build-reviewed-head'), `${reviewed}\n`)
  if (route !== null) writeFileSync(join(dir, 'boss-build-ready-route'), route)
  const calls = []
  const spawn = (command, args) => {
    calls.push([command, ...args])
    if (command === 'git' && args[0] === 'rev-parse') {
      if (args[1] === '--absolute-git-dir') return { status: 0, stdout: `${dir}\n` }
      if (args.at(-1).endsWith('^{tree}'))
        return { status: 0, stdout: `${trees[args.at(-1).replace('^{tree}', '')] ?? TREE}\n` }
      return { status: 0, stdout: `${head}\n` }
    }
    if (command === 'gh' && args[0] === 'pr')
      return prBase
        ? { status: 0, stdout: JSON.stringify({ baseRefName: prBase }) }
        : { status: 1, stdout: '' }
    if (command === 'git' && args[0] === 'symbolic-ref')
      return remoteHead ? { status: 0, stdout: remoteHead } : { status: 1, stdout: '' }
    if (command === 'git' && args[0] === 'merge-base') return { status: 0, stdout: MERGED }
    assert.fail(`unexpected command ${command} ${args.join(' ')}`)
  }
  return { dir, calls, deps: { spawn, cwd: dir } }
}
test('readyGateReport: reviewed head is HEAD — ready without reading the base', (t) => {
  const h = gateHarness(t)
  assert.deepEqual(readyGateReport({}, h.deps), {
    ready: true,
    reasons: [],
    reviewVerdict: 'clean',
    reviewedHead: HEAD,
    head: HEAD,
    treeDrift: 'none',
  })
  assert.equal(
    h.calls.some(([c, a]) => c === 'gh' || a === 'merge-base'),
    false,
  )
})
test('readyGateReport: a rewrite that kept the tree is ready; the base comes from the PR', (t) => {
  const h = gateHarness(t, { head: OTHER })
  const report = readyGateReport({}, h.deps)
  assert.equal(report.ready, true)
  assert.equal(report.treeDrift, 'none')
  assert.ok(h.calls.some(([c, a, b]) => c === 'git' && a === 'merge-base' && b === 'origin/main'))
})
test('readyGateReport: a commit after review is tree-changed-since-review', (t) => {
  const h = gateHarness(t, { head: OTHER, trees: { [OTHER]: TREE2 } })
  const report = readyGateReport({}, h.deps)
  assert.deepEqual(report.reasons, ['tree-changed-since-review'])
  assert.equal(report.treeDrift, 'changed')
})
test('readyGateReport: --base wins, an origin/ prefix is tolerated, no PR read', (t) => {
  const h = gateHarness(t, { head: OTHER, prBase: '' })
  assert.equal(readyGateReport({ base: 'origin/release' }, h.deps).ready, true)
  assert.ok(h.calls.some(([, a, b]) => a === 'merge-base' && b === 'origin/release'))
  assert.equal(
    h.calls.some(([c]) => c === 'gh'),
    false,
  )
})
test('readyGateReport: no PR base falls back to origin/HEAD; neither is drift unknown', (t) => {
  const fallback = gateHarness(t, { head: OTHER, prBase: '', remoteHead: 'origin/trunk' })
  assert.equal(readyGateReport({}, fallback.deps).ready, true)
  assert.ok(fallback.calls.some(([, a, b]) => a === 'merge-base' && b === 'origin/trunk'))
  const none = gateHarness(t, { head: OTHER, prBase: '', remoteHead: '' })
  const report = readyGateReport({}, none.deps)
  assert.equal(report.treeDrift, 'unknown')
  assert.deepEqual(report.reasons, ['tree-drift-unknown'])
})
test('readyGateReport: --head pins the head instead of HEAD', (t) => {
  const h = gateHarness(t, { head: HEAD })
  const report = readyGateReport({ head: OTHER, base: 'main' }, h.deps)
  assert.equal(report.head, OTHER)
  assert.equal(
    h.calls.some(([c, a, b]) => c === 'git' && a === 'rev-parse' && b === 'HEAD'),
    false,
  )
})
test('readyGateReport: missing files are unknown verdict and unknown reviewed head', (t) => {
  const h = gateHarness(t, { verdict: null, reviewed: null })
  assert.deepEqual(readyGateReport({}, h.deps).reasons, [
    'review-verdict-unknown',
    'reviewed-head-unknown',
  ])
})
test('readyGateReport: capped, partial and disabled verdicts; override files win', (t) => {
  const h = gateHarness(t, { verdict: 'clean' })
  const verdictFile = join(h.dir, 'other-verdict')
  writeFileSync(verdictFile, 'REVIEW_VERDICT=capped\n')
  assert.deepEqual(readyGateReport({ reviewVerdictFile: verdictFile }, h.deps).reasons, [
    'review-not-clean',
  ])
  const partial = readyGateReport({ reviewVerdictFile: verdictFile, partial: true }, h.deps)
  assert.deepEqual([partial.ready, partial.admittedFor], [true, 'partial'])
  writeFileSync(verdictFile, 'REVIEW_VERDICT=disabled\n')
  const disabled = readyGateReport(
    { reviewVerdictFile: verdictFile, reviewedHeadFile: join(h.dir, 'absent') },
    h.deps,
  )
  assert.deepEqual(
    [disabled.ready, disabled.reasons, disabled.reviewVerdict],
    [false, ['reviewed-head-unknown'], 'disabled'],
  )
})

test('readyGateReport: the route file grants partial only to a caller that honours it', (t) => {
  for (const route of ['partial\n', 'PARTIAL', ' partial ']) {
    const h = gateHarness(t, { verdict: 'capped', route })
    const unasked = readyGateReport({}, h.deps)
    assert.deepEqual([unasked.ready, unasked.reasons], [false, ['review-not-clean']], route)
    assert.equal('admittedFor' in unasked, false)
    const honoured = readyGateReport({ honourRoute: true }, h.deps)
    assert.deepEqual([honoured.ready, honoured.admittedFor], [true, 'partial'], route)
  }
  for (const route of [null, '', 'full\n', 'partial-ish']) {
    const h = gateHarness(t, { verdict: 'capped', route })
    assert.deepEqual(
      readyGateReport({ honourRoute: true }, h.deps).reasons,
      ['review-not-clean'],
      String(route),
    )
  }
  const other = gateHarness(t, { verdict: 'capped' })
  const routeFile = join(other.dir, 'elsewhere')
  writeFileSync(routeFile, 'partial\n')
  assert.equal(readyGateReport({ honourRoute: true, routeFile }, other.deps).ready, true)
  assert.equal(readyGateReport({ routeFile }, other.deps).ready, false)
})
test('main honours the git-dir route file and --route-file; the route admits drift nowhere', (t) => {
  const h = gateHarness(t, { verdict: 'capped', route: 'partial\n' })
  assert.equal(main([], h.deps).admittedFor, 'partial')
  const routeFile = join(h.dir, 'full-route')
  writeFileSync(routeFile, 'full\n')
  assert.deepEqual(main(['--route-file', routeFile], h.deps).reasons, ['review-not-clean'])
  assert.equal(main(['--route-file', routeFile, '--partial'], h.deps).ready, true)
  const drifted = gateHarness(t, {
    verdict: 'capped',
    route: 'partial',
    head: OTHER,
    trees: { [OTHER]: TREE2 },
  })
  assert.deepEqual(main([], drifted.deps).reasons, ['tree-changed-since-review'])
})

// ---------------------------------------------------------------------------
// CLI

test('main: strict flags and a 40-hex --head, else UsageError', (t) => {
  const h = gateHarness(t)
  assert.equal(main([], h.deps).ready, true)
  for (const argv of [
    ['--nope', 'x'],
    ['--head'],
    ['--head', 'abc'],
    ['positional'],
    ['--partial=yes'],
  ])
    assert.throws(() => main(argv, h.deps), UsageError, argv.join(' '))
})
test('CLI: exit 0 ready, 3 not ready, 2 usage error; --partial admits a capped review', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'ready-gate-cli-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const verdictFile = join(dir, 'verdict'),
    reviewedFile = join(dir, 'reviewed')
  writeFileSync(reviewedFile, `${HEAD}\n`)
  const run = (...args) =>
    spawnSync(
      process.execPath,
      [
        GATE_SCRIPT,
        '--review-verdict-file',
        verdictFile,
        '--reviewed-head-file',
        reviewedFile,
        '--head',
        HEAD,
        ...args,
      ],
      { cwd: dir, encoding: 'utf8' },
    )
  writeFileSync(verdictFile, 'REVIEW_VERDICT=clean\n')
  const ready = run()
  assert.equal(ready.status, 0, ready.stderr)
  assert.deepEqual(JSON.parse(ready.stdout), {
    ready: true,
    reasons: [],
    reviewVerdict: 'clean',
    reviewedHead: HEAD,
    head: HEAD,
    treeDrift: 'none',
  })
  writeFileSync(verdictFile, 'REVIEW_VERDICT=capped\n')
  const capped = run()
  assert.equal(capped.status, 3)
  assert.deepEqual(JSON.parse(capped.stdout).reasons, ['review-not-clean'])
  const partial = run('--partial')
  assert.equal(partial.status, 0, partial.stderr)
  assert.equal(JSON.parse(partial.stdout).admittedFor, 'partial')
  const routeFile = join(dir, 'route')
  writeFileSync(routeFile, 'partial\n')
  const routed = run('--route-file', routeFile)
  assert.equal(routed.status, 0, routed.stderr)
  assert.equal(JSON.parse(routed.stdout).admittedFor, 'partial')
  const usage = run('--bogus', 'x')
  assert.equal(usage.status, 2)
  assert.equal(usage.stdout, '')
  assert.match(usage.stderr, /ready-gate/)
})

// ---------------------------------------------------------------------------
// Real git

const LINES = ['l1', 'l2', 'l3', 'l4', 'l5', 'l6', 'l7', 'l8', 'l9'].join('\n') + '\n'

// A repo whose `main` (mirrored to origin/main) holds `lib` = base, and whose `feature` branch adds
// one reviewed commit with `lib` = feature, recorded as the clean reviewed head.
function gitRepo(t, { base = LINES, feature = LINES.replace('l5', 'hello world') } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'ready-gate-git-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const run = (...args) =>
    spawnSync(
      'git',
      [
        '-c',
        'user.name=Gate test',
        '-c',
        'user.email=test@example.invalid',
        '-c',
        'commit.gpgsign=false',
        ...args,
      ],
      { cwd: dir, encoding: 'utf8' },
    )
  const git = (...args) => {
    const result = run(...args)
    assert.equal(result.status, 0, result.stderr)
    return result.stdout.trim()
  }
  const commit = (file, content, message) => {
    writeFileSync(join(dir, file), content)
    git('add', file)
    git('commit', '-m', message)
    return git('rev-parse', 'HEAD')
  }
  git('init', '--initial-branch=main')
  // Keep every object loose: the missing-tree test deletes one by path, and a background
  // auto-gc that packs it first turns that delete into ENOENT (seen in CI on #3049).
  git('config', 'gc.auto', '0')
  git('config', 'maintenance.auto', 'false')
  commit('lib', base, 'base')
  git('update-ref', 'refs/remotes/origin/main', 'HEAD')
  git('checkout', '-b', 'feature')
  const reviewed = commit('lib', feature, 'feature')
  const gitDir = git('rev-parse', '--absolute-git-dir')
  writeFileSync(join(gitDir, 'boss-build-review-verdict'), 'REVIEW_VERDICT=clean\n')
  writeFileSync(join(gitDir, 'boss-build-reviewed-head'), `${reviewed}\n`)
  // Advance the base (main and origin/main) with one commit, leaving `feature` checked out.
  const advanceBase = (file, content) => {
    git('checkout', 'main')
    commit(file, content, 'advance base')
    git('update-ref', 'refs/remotes/origin/main', 'HEAD')
    git('checkout', 'feature')
  }
  // Advance the base, then rebase the feature onto it.
  const advance = (file, content) => {
    advanceBase(file, content)
    git('rebase', 'main')
  }
  const gate = (...args) => main(['--base', 'main', ...args], { cwd: dir })
  const drift = (gated) => [gated.ready, gated.reasons, gated.treeDrift]
  return { dir, gitDir, git, run, commit, reviewed, advanceBase, advance, gate, drift }
}
const READY = [true, [], 'none'],
  CHANGED = [false, ['tree-changed-since-review'], 'changed'],
  UNKNOWN = [false, ['tree-drift-unknown'], 'unknown']

test('real git: a rebase over an unrelated base change keeps the tree', (t) => {
  const r = gitRepo(t)
  r.advance('unrelated', 'base advanced\n')
  assert.deepEqual(r.drift(r.gate()), READY)
})
test('real git: a rebase over a base edit beside (not inside) the hunk keeps the tree', (t) => {
  const r = gitRepo(t)
  // l3 sits inside the feature hunk's default 3-line context; the inserted first line shifts every
  // hunk and changes the file's pre-image blob.
  r.advance('lib', `base header\n${LINES.replace('l3', 'base edited l3')}`)
  assert.deepEqual(r.drift(r.gate()), READY)
})
test('real git: a message-only rewrite (tag injection) keeps the tree, rebased or not', (t) => {
  const r = gitRepo(t)
  r.git('commit', '--amend', '-m', 'feature [#3035]')
  assert.notEqual(r.git('rev-parse', 'HEAD'), r.reviewed)
  assert.deepEqual(r.drift(r.gate()), READY)
  r.advance('unrelated', 'base advanced\n')
  r.git('commit', '--amend', '-m', 'feature [#3035] again')
  assert.deepEqual(r.drift(r.gate()), READY)
})
test('real git: a commit after review that changes content is tree-changed-since-review', (t) => {
  const r = gitRepo(t)
  r.commit('lib', LINES.replace('l5', 'hello there'), 'unreviewed')
  assert.deepEqual(r.drift(r.gate()), CHANGED)
})
test('real git: a whitespace-only change after review is tree-changed-since-review', (t) => {
  for (const changed of ['helloworld', 'hello  world', 'hello world ']) {
    const r = gitRepo(t)
    r.commit('lib', LINES.replace('l5', changed), `whitespace ${JSON.stringify(changed)}`)
    assert.deepEqual(r.drift(r.gate()), CHANGED, JSON.stringify(changed))
  }
})
test('real git: moving a branch-added line after review is tree-changed-since-review', (t) => {
  const added = LINES.replace('l2\n', 'l2\nadded\n')
  const moved = LINES.replace('l7\n', 'l7\nadded\n')
  const r = gitRepo(t, { feature: added })
  r.commit('lib', moved, 'move the added line')
  assert.deepEqual(r.drift(r.gate()), CHANGED)
  // The same move on top of a rebase reaches the replay path.
  const rebased = gitRepo(t, { feature: added })
  rebased.advance('unrelated', 'base advanced\n')
  rebased.commit('lib', moved, 'move the added line')
  assert.deepEqual(rebased.drift(rebased.gate()), CHANGED)
})
test('real git: the same edit moved to another function is tree-changed-since-review', (t) => {
  const base = 'function a() {\n  return 1\n}\n\nfunction b() {\n  return 1\n}\n'
  const inA = base.replace('a() {\n  return 1', 'a() {\n  return 2')
  const inB = base.replace('b() {\n  return 1', 'b() {\n  return 2')
  const r = gitRepo(t, { base, feature: inA })
  r.commit('lib', inB, 'move the edit to b')
  assert.deepEqual(r.drift(r.gate()), CHANGED)
})
test('real git: a rebase whose replay conflicts is tree-changed-since-review', (t) => {
  const r = gitRepo(t)
  r.advanceBase('lib', LINES.replace('l5', 'base l5'))
  // The resolved rebase: the feature's line wins over the base's edit of the same line.
  r.git('checkout', '-B', 'feature', 'main')
  r.commit('lib', LINES.replace('l5', 'hello world'), 'feature')
  assert.deepEqual(r.drift(r.gate()), CHANGED)
  // Through completion-gate.mjs's reader too: its failed read carries the exit status.
  assert.equal(
    reviewedTreeDrift(
      commandReader({ cwd: r.dir }),
      'main',
      r.reviewed,
      r.git('rev-parse', 'HEAD'),
    ),
    'changed',
  )
})
test('real git: a missing reviewed head, pushed head or reviewed tree is tree-drift-unknown', (t) => {
  const r = gitRepo(t)
  writeFileSync(join(r.gitDir, 'boss-build-reviewed-head'), `${'d'.repeat(40)}\n`)
  assert.deepEqual(r.drift(r.gate()), UNKNOWN)
  writeFileSync(join(r.gitDir, 'boss-build-reviewed-head'), `${r.reviewed}\n`)
  assert.deepEqual(r.drift(r.gate('--head', 'e'.repeat(40))), UNKNOWN)
  // The replay needs the reviewed tree's objects; with one gone the merge fails, never conflicts.
  r.advance('unrelated', 'base advanced\n')
  const tree = r.git('rev-parse', `${r.reviewed}^{tree}`)
  rmSync(join(r.gitDir, 'objects', tree.slice(0, 2), tree.slice(2)))
  assert.deepEqual(r.drift(r.gate()), UNKNOWN)
})
test('real git: trailing whitespace on the last line is seen through either reader', (t) => {
  const r = gitRepo(t)
  const last = r.commit('zz-last', 'tail\n', 'add a last file')
  writeFileSync(join(r.gitDir, 'boss-build-reviewed-head'), `${last}\n`)
  const shipped = r.commit('zz-last', 'tail \n', 'trailing space')
  // completion-gate.mjs's reader (buildEnvelope) and this gate's own (readyGateReport).
  assert.equal(reviewedTreeDrift(commandReader({ cwd: r.dir }), 'main', last, shipped), 'changed')
  const viaGate = r.gate()
  assert.deepEqual([viaGate.head, viaGate.treeDrift], [shipped, 'changed'])
})

for (const [label, changes, expected] of [
  ['unchanged', {}, { ready: true, reasons: [], admittedFor: 'disabled' }],
  [
    'changed',
    { pushedHead: OTHER, treeDrift: 'changed' },
    { ready: false, reasons: ['tree-changed-since-review'], admittedFor: 'disabled' },
  ],
  [
    'unknown drift',
    { treeDrift: 'unknown' },
    { ready: false, reasons: ['tree-drift-unknown'], admittedFor: 'disabled' },
  ],
  ['partial precedence', { partial: true }, { ready: true, reasons: [], admittedFor: 'disabled' }],
])
  test(`disabled waiver: ${label}`, () =>
    assert.deepEqual(readyGate({ ...cleanGate, reviewVerdict: 'disabled', ...changes }), expected))

test('waivers are frozen and admit review quality only', () => {
  assert.ok(Object.isFrozen(READY_GATE_WAIVERS))
  for (const [waiver, reasons] of Object.entries(READY_GATE_WAIVERS)) {
    assert.ok(Object.isFrozen(reasons))
    assert.deepEqual(reasons, ['review-not-clean'])
    for (const [changes, expected] of [
      [{}, []],
      [{ reviewedHead: '' }, ['reviewed-head-unknown']],
      [{ treeDrift: 'changed' }, ['tree-changed-since-review']],
      [{ treeDrift: 'unknown' }, ['tree-drift-unknown']],
    ]) {
      const actual = readyGate({
        ...cleanGate,
        reviewVerdict: waiver === 'disabled' ? 'disabled' : 'capped',
        partial: waiver === 'partial',
        ...changes,
      })
      assert.deepEqual(actual.reasons, expected)
      assert.equal(actual.admittedFor, waiver)
    }
  }
})
test('commandReader maps boss and preserves failed JSON payload error precedence', () => {
  const payload = { error: { code: 'api-error' }, code: 'other' }
  const calls = []
  const read = commandReader({
    bossBin: '/custom/boss',
    spawn: (...args) => {
      calls.push(args)
      return { status: 1, stdout: JSON.stringify(payload), error: { code: 'spawn-error' } }
    },
  })
  assert.deepEqual(read('boss', [], { json: true }), {
    ok: false,
    status: 1,
    error: 'api-error',
    payload,
  })
  assert.equal(calls[0][0], '/custom/boss')
  assert.equal(calls[0][2].maxBuffer, 64 * 1024 * 1024)
  const plain = commandReader({ spawn: () => ({ status: 1, stdout: '{"code":"plain-error"}' }) })
  assert.equal(plain('git', [], { json: true }).error, 'plain-error')
})
test('commandReader refuses unreadable JSON and thrown spawns', () => {
  assert.deepEqual(
    commandReader({ spawn: () => ({ status: 0, stdout: 'oops' }) })('gh', [], { json: true }),
    { ok: false, status: 0, error: 'unreadable-json' },
  )
  assert.deepEqual(
    commandReader({
      spawn: () => {
        throw new Error('failed')
      },
    })('git', []),
    { ok: false, status: null, error: 'spawn-error' },
  )
})
test('real git: disabled note requires a reviewed head matching the tree', (t) => {
  const r = gitRepo(t)
  writeFileSync(join(r.gitDir, 'boss-build-review-verdict'), 'REVIEW_VERDICT=disabled\n')
  rmSync(join(r.gitDir, 'boss-build-reviewed-head'))
  assert.deepEqual(r.gate().reasons, ['reviewed-head-unknown'])
  writeFileSync(join(r.gitDir, 'boss-build-reviewed-head'), r.reviewed)
  assert.equal(r.gate().ready, true)
})
