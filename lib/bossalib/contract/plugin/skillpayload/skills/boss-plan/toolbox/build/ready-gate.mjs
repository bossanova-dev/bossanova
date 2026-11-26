#!/usr/bin/env node

// ready-gate.mjs — may this head be marked finished (PR readied, ticket In Review, `boss/build`
// success)? Only when its review is clean (or the operator's BOSS_BS_REVIEW=0 opt-out, `disabled`)
// and the head's tree is what the reviewed head becomes when replayed onto the head's base: a pure
// rebase or tag injection keeps it, a commit after review or a conflict resolution does not.
//
//   node ready-gate.mjs [--head <40-hex>] [--base <branch>] [--review-verdict-file <path>]
//                       [--reviewed-head-file <path>] [--route-file <path>] [--partial]
//     prints {ready, reasons, reviewVerdict, reviewedHead, head, treeDrift[, admittedFor]}
//     exit 0 ready · 3 not ready · 2 usage error (stderr, nothing printed) · 1 internal error
//
//   node ready-gate.mjs remap-reviewed-head [--head <40-hex>] [--base <branch>]
//                       [--reviewed-head-file <path>]
//     prints {outcome, reviewedHead, mappedHead, head}; exit 3 unknown, 0 otherwise
//
// A waiver admits review-quality reasons only, never provenance or an unknown review verdict.
// The partial exception admits a capped review (`review-not-clean`) for boss-build's PARTIAL route;
// every other reason still refuses, and the verdict says `admittedFor: 'partial'` when it applied.
// The CLI applies it when `--partial` is given or the route file (default
// `<git-dir>/boss-build-ready-route`, written by boss-build's Step 9) reads `partial`, so the route
// survives between shell invocations. In-process callers get it only by asking: commit-status.mjs
// never does, because a `boss/build` receipt is REVIEW_READY-only.
//
// The PR base comes from the daemon-first reader (../pr-state.mjs), so commit-status.mjs, which
// imports this gate in-process, ships that reader's closure too. completion-gate.mjs shares the
// reason vocabulary and the tree-drift reader from here.

import { spawnSync } from 'node:child_process'
import { readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { createHash, randomUUID } from 'node:crypto'
import { resolve } from 'node:path'
import { parseArgs } from 'node:util'

import { generatedPathMatchers, isGenerated } from '../generated-paths.mjs'
import { isMainModule } from '../main-module.mjs'
import { viewCommand } from '../pr-state.mjs'

const SHA = /^[0-9a-f]{40}$/i
const text = (value) => typeof value === 'string' && value.trim() !== ''
const sha = (value) => typeof value === 'string' && SHA.test(value)

function readText(path) {
  try {
    return readFileSync(path, 'utf8').trim()
  } catch {
    return ''
  }
}

// A failed read is `{ok: false, status, output}`, never an empty successful payload; `status` is
// the exit code (null when the command did not run to an exit). `output` keeps stdout, because a
// conflicted `git merge-tree --write-tree` exits 1 and still prints the tree it wrote.
export function commandReader({ spawn = spawnSync, cwd = process.cwd(), bossBin = 'boss' } = {}) {
  return (command, args, { json = false, timeout = 30000 } = {}) => {
    let response
    try {
      response = spawn(command === 'boss' ? bossBin : command, args, {
        cwd,
        encoding: 'utf8',
        timeout,
        maxBuffer: 64 * 1024 * 1024,
        killSignal: 'SIGKILL',
      })
    } catch (error) {
      return { ok: false, status: null, error: error.code ?? 'spawn-error' }
    }
    let payload
    if (json) {
      try {
        payload = JSON.parse(response.stdout)
      } catch {
        /* Failure remains unknown. */
      }
    }
    if (response.error || response.status !== 0)
      return {
        ok: false,
        status: response.status ?? null,
        error:
          payload?.error?.code ??
          payload?.code ??
          response.error?.code ??
          `exit-${response.status}`,
        payload,
        output: String(response.stdout ?? '').trim(),
      }
    if (json && payload === undefined) return { ok: false, status: 0, error: 'unreadable-json' }
    return { ok: true, payload, output: String(response.stdout ?? '').trim() }
  }
}

// The reason vocabulary the ready gate and completion-gate.mjs's merge eligibility share.
export const READY_GATE_REASONS = Object.freeze([
  'review-verdict-unknown',
  'review-not-clean',
  'reviewed-head-unknown',
  'tree-changed-since-review',
  'tree-drift-unknown',
])

// A missing, `none` or unrecognised verdict is unknown; `capped` and `disabled` are not clean.
// Waivers admit review quality only; reviewed-head and tree provenance still apply.
export function reviewReasons(verdict) {
  const value = text(verdict) ? verdict.trim() : ''
  if (value === 'clean') return []
  if (value === 'capped' || value === 'disabled') return ['review-not-clean']
  return ['review-verdict-unknown']
}

export function driftReasons(treeDrift) {
  if (treeDrift === 'none') return []
  return [treeDrift === 'changed' ? 'tree-changed-since-review' : 'tree-drift-unknown']
}

// 'none' when pushedHead's tree is the reviewed head replayed onto pushedHead's base, 'changed'
// when it is not (or the replay conflicts: a resolution no review saw), 'unknown' when that cannot
// be read. Paths the base declares generated (generated-paths.mjs) are left out of both the
// conflicts and the comparison: a rebase that only regenerated them keeps the review. Same merge base: the trees must be equal. Different merge bases: a three-way merge of the
// reviewed head onto the new base (`git merge-tree --merge-base`, git >= 2.40; an older git fails,
// which reads unknown). `read(command, args)` answers `{ok: true, output}` or `{ok: false, status}`.
// `base` is a branch (an `origin/` prefix is tolerated) or a function returning one, called only
// when the heads differ.
export function reviewedTreeDrift(read, base, reviewedHead, pushedHead) {
  if (!sha(reviewedHead) || !sha(pushedHead)) return 'unknown'
  if (reviewedHead.toLowerCase() === pushedHead.toLowerCase()) return 'none'
  const pushedTree = readOid(read, ['rev-parse', '--verify', '--quiet', `${pushedHead}^{tree}`])
  if (!pushedTree) return 'unknown'
  const expected = expectedReviewedTree(read, base, reviewedHead, pushedHead)
  if (expected.outcome === 'unknown' || expected.outcome === 'changed') return expected.outcome
  if (expected.outcome === 'ok' && expected.tree === pushedTree) return 'none'
  return generatedOnlyDrift(read, expected, pushedTree)
}

// 'none' when the replay conflicted only on generated paths and differs from the pushed tree only
// on generated paths, 'changed' otherwise, 'unknown' when the difference cannot be read.
function generatedOnlyDrift(read, expected, pushedTree) {
  const matchers = generatedPathMatchers(read, expected.headBase)
  if (matchers.length === 0) return 'changed'
  if (!expected.conflicted.every((path) => isGenerated(matchers, path))) return 'changed'
  const names = read('git', [
    'diff',
    '--name-only',
    '-z',
    '--no-renames',
    expected.tree,
    pushedTree,
  ])
  if (!names.ok || typeof names.output !== 'string') return 'unknown'
  const paths = names.output.split('\0').filter(Boolean)
  return paths.every((path) => isGenerated(matchers, path)) ? 'none' : 'changed'
}

function readOid(read, args) {
  const result = read('git', args)
  const value = result.ok && typeof result.output === 'string' ? result.output.trim() : ''
  return sha(value) ? value.toLowerCase() : null
}

function expectedReviewedTree(read, base, reviewedHead, head) {
  const branch = typeof base === 'function' ? base() : base
  if (!text(branch)) return { outcome: 'unknown' }
  const remote = `origin/${branch.trim().replace(/^origin\//, '')}`
  const reviewedBase = readOid(read, ['merge-base', remote, reviewedHead])
  const headBase = readOid(read, ['merge-base', remote, head])
  if (!reviewedBase || !headBase) return { outcome: 'unknown' }
  if (reviewedBase === headBase) {
    const tree = readOid(read, ['rev-parse', '--verify', '--quiet', `${reviewedHead}^{tree}`])
    return tree
      ? { outcome: 'ok', tree, reviewedBase, headBase, conflicted: [] }
      : { outcome: 'unknown' }
  }
  const replay = read('git', [
    'merge-tree',
    '--write-tree',
    '-z',
    '--name-only',
    '--no-messages',
    `--merge-base=${reviewedBase}`,
    headBase,
    reviewedHead,
  ])
  if (!replay.ok && replay.status !== 1) return { outcome: 'unknown' }
  const [tree, ...conflicted] = String(replay.output ?? '').split('\0')
  if (!sha(tree?.trim())) return { outcome: replay.ok ? 'unknown' : 'changed', conflicted: [] }
  return {
    outcome: replay.ok ? 'ok' : 'conflicted',
    tree: tree.trim().toLowerCase(),
    reviewedBase,
    headBase,
    conflicted: [...new Set(conflicted.filter(Boolean))],
  }
}

// One key per commit for its own change outside generated paths: the diff from its parent with
// blob ids and hunk line numbers left out, so the same patch over moved lines keeps its key. A
// commit that changed only generated paths has key null. Each commit also carries its author and
// author date, which a rebase keeps, so a rewritten commit can be told from a new one. null for
// the whole range when a commit is a merge or a read fails.
function commitChangeKeys(read, range, matchers) {
  const listed = read('git', ['rev-list', '--reverse', '--parents', range])
  if (!listed.ok || typeof listed.output !== 'string') return null
  const commits = []
  for (const line of listed.output.split('\n').filter(Boolean)) {
    const [commit, parent, extra] = line.trim().split(/\s+/)
    if (!sha(commit) || !sha(parent) || extra) return null
    const names = read('git', ['diff', '--name-only', '-z', '--no-renames', parent, commit])
    if (!names.ok || typeof names.output !== 'string') return null
    const kept = names.output.split('\0').filter((path) => path && !isGenerated(matchers, path))
    let key = null
    if (kept.length > 0) {
      const diff = read('git', [
        'diff',
        '--binary',
        '--no-renames',
        '--no-color',
        '--no-ext-diff',
        '--no-textconv',
        parent,
        commit,
        '--',
        ...kept.map((path) => `:(literal)${path}`),
      ])
      if (!diff.ok || typeof diff.output !== 'string') return null
      const normalised = diff.output
        .split('\n')
        .filter((l) => !l.startsWith('index '))
        .map((l) => (l.startsWith('@@') ? '@@' : l))
        .join('\n')
      key = createHash('sha256').update(normalised).digest('hex')
    }
    commits.push({ commit: commit.toLowerCase(), parent: parent.toLowerCase(), key })
  }
  return commits
}

// One `git range-diff` pair: left and right commit ids ('-' for none) and the relation.
const RANGE_PAIR =
  /^\s*(?:\d+|-):\s+([0-9a-f]{40,64}|-+)\s+([<>=!])\s+(?:\d+|-):\s+([0-9a-f]{40,64}|-+)(?:\s|$)/

// The rebased commits whose own change no reviewed commit made, and the newest rebased commit
// before the first of them. `git range-diff` pairs each reviewed commit with its rebased form; the
// change keys then decide whether a pair still makes the same hand-written change (range-diff also
// flags a message-only rewrite). null when that cannot be read, no commit is new, or a reviewed
// commit with a hand-written change has no rebased counterpart: the commits kept were reviewed
// beside a change that is now gone, and only a whole review covers that.
function changedCommits(read, expected, reviewedHead, head) {
  const matchers = generatedPathMatchers(read, expected.headBase)
  const before = commitChangeKeys(read, `${expected.reviewedBase}..${reviewedHead}`, matchers)
  const after = commitChangeKeys(read, `${expected.headBase}..${head}`, matchers)
  if (!before || !after) return null
  const keys = new Map([...before, ...after].map(({ commit, key }) => [commit, key]))
  const pairs = read('git', [
    'range-diff',
    '--no-color',
    '--abbrev=40',
    '--no-patch',
    `${expected.reviewedBase}..${reviewedHead}`,
    `${expected.headBase}..${head}`,
  ])
  if (!pairs.ok || typeof pairs.output !== 'string') return null
  const changed = new Set()
  const seen = { left: 0, right: 0 }
  for (const line of pairs.output.split('\n')) {
    const match = line.match(RANGE_PAIR)
    if (!match) continue
    const [, left, relation, right] = match
    let leftKey = null
    let rightKey = null
    if (relation !== '>') {
      seen.left += 1
      leftKey = keys.get(left)
    }
    if (relation !== '<') {
      seen.right += 1
      rightKey = keys.get(right)
    }
    if (leftKey === undefined || rightKey === undefined) return null
    if (leftKey && !rightKey) return null
    if (rightKey && rightKey !== leftKey) changed.add(right)
  }
  if (seen.left !== before.length || seen.right !== after.length) return null
  const ordered = after.filter(({ commit }) => changed.has(commit))
  if (ordered.length === 0) return null
  const lastKept = ordered[0].parent === expected.headBase ? null : ordered[0].parent
  return { lastKept, commits: ordered.map(({ commit }) => commit) }
}

function persistReviewedHead(file, mappedHead) {
  const temporary = `${file}.${randomUUID()}.tmp`
  try {
    writeFileSync(temporary, `${mappedHead}\n`, { flag: 'wx' })
    renameSync(temporary, file)
  } finally {
    try {
      unlinkSync(temporary)
    } catch (error) {
      if (error.code !== 'ENOENT') throw error
    }
  }
}

export function remapReviewedHead(read, { base, reviewedHeadFile, head } = {}, deps = {}) {
  const files = reviewFiles(read, { reviewedHeadFile })
  const reviewedHead = files.reviewedHead
  if (!text(head)) {
    const current = read('git', ['rev-parse', 'HEAD'])
    head = current.ok ? current.output : ''
  }
  const result = (outcome, mappedHead = null) => ({ outcome, reviewedHead, mappedHead, head })
  if (!sha(reviewedHead) || !sha(head)) return result('unknown')
  if (reviewedHead.toLowerCase() === head.toLowerCase()) return result('kept', reviewedHead)
  const ancestor = read('git', ['merge-base', '--is-ancestor', reviewedHead, head])
  if (ancestor.ok) return result('kept', reviewedHead)
  if (ancestor.status !== 1) return result('unknown')
  const expected = expectedReviewedTree(
    read,
    () => (text(base) ? base : baseBranch(read, deps)),
    reviewedHead,
    head,
  )
  if (expected.outcome === 'unknown') return result('unknown')
  if (expected.outcome === 'changed') return result('unmatched')
  if (expected.outcome === 'ok') {
    const candidates = read('git', ['log', '--format=%H %T', `${expected.headBase}..${head}`])
    if (!candidates.ok || typeof candidates.output !== 'string') return result('unknown')
    let mappedHead = null
    for (const line of candidates.output.split('\n').filter(Boolean)) {
      const [candidate, tree, extra] = line.trim().split(/\s+/)
      if (!sha(candidate) || !sha(tree) || extra) return result('unknown')
      if (!mappedHead && tree.toLowerCase() === expected.tree) mappedHead = candidate
    }
    if (mappedHead) {
      persistReviewedHead(files.reviewedHeadFile, mappedHead)
      return result('remapped', mappedHead)
    }
  }
  // No commit carries the replayed tree. Carry the review when only generated paths differ;
  // otherwise name the rebased commits whose own change differs, so only they are reviewed.
  const headTree = readOid(read, ['rev-parse', '--verify', '--quiet', `${head}^{tree}`])
  const drift = headTree ? generatedOnlyDrift(read, expected, headTree) : 'unknown'
  if (drift === 'unknown') return result('unknown')
  if (drift === 'none') {
    persistReviewedHead(files.reviewedHeadFile, head)
    return result('carried', head)
  }
  const changed = changedCommits(read, expected, reviewedHead, head)
  if (!changed) return result('unmatched')
  if (!changed.lastKept) return { ...result('unmatched'), reviewCommits: changed.commits }
  persistReviewedHead(files.reviewedHeadFile, changed.lastKept)
  return { ...result('narrowed', changed.lastKept), reviewCommits: changed.commits }
}

// The gate. A head that cannot be named cannot be shown to carry the reviewed tree, so its drift
// is unknown. `partial` admits `review-not-clean` and nothing else.
export const READY_GATE_WAIVERS = Object.freeze({
  disabled: Object.freeze(['review-not-clean']),
  partial: Object.freeze(['review-not-clean']),
})

export function readyGate({ reviewVerdict, reviewedHead, pushedHead, treeDrift, partial } = {}) {
  const verdict = text(reviewVerdict) ? reviewVerdict.trim() : ''
  let reasons = reviewReasons(verdict)
  if (!sha(reviewedHead)) reasons.push('reviewed-head-unknown')
  else reasons.push(...driftReasons(sha(pushedHead) ? treeDrift : 'unknown'))
  let admittedFor
  for (const waiver of [verdict === 'disabled' && 'disabled', partial === true && 'partial']) {
    if (!waiver) continue
    const admitted = READY_GATE_WAIVERS[waiver]
    if (reasons.some((reason) => admitted.includes(reason))) {
      reasons = reasons.filter((reason) => !admitted.includes(reason))
      admittedFor ??= waiver
    }
  }
  return { ready: reasons.length === 0, reasons, ...(admittedFor ? { admittedFor } : {}) }
}

// The review files boss-build's Step 6/7 write in the git dir, and the route file its Step 9
// writes, unless the caller names others.
export function reviewFiles(read, options = {}) {
  const gitDirRead = read('git', ['rev-parse', '--absolute-git-dir'])
  const gitDir = gitDirRead.ok ? gitDirRead.output : ''
  const inGitDir = (name) => (gitDir ? resolve(gitDir, name) : '')
  const reviewVerdictFile = options.reviewVerdictFile ?? inGitDir('boss-build-review-verdict')
  const reviewedHeadFile = options.reviewedHeadFile ?? inGitDir('boss-build-reviewed-head')
  const routeFile = options.routeFile ?? inGitDir('boss-build-ready-route')
  return {
    reviewVerdictFile,
    reviewedHeadFile,
    routeFile,
    reviewVerdict:
      readText(reviewVerdictFile)
        .match(/^REVIEW_VERDICT=(.*)$/m)?.[1]
        ?.trim() ?? '',
    reviewedHead: readText(reviewedHeadFile),
    route: readText(routeFile),
  }
}

// The session PR's base, else the remote default branch; '' when neither reads. The PR is read
// daemon-first (`pr-state.mjs view`); gh runs only on that reader's labelled fallback. `deps` are
// the spawn deps ({spawn, cwd, env, bossBin}) the PR read runs with.
export function baseBranch(read, deps = {}) {
  const { bossBin, ...rest } = deps
  const resolveBoss = bossBin ? () => ({ ok: true, path: bossBin }) : undefined
  const pr = viewCommand({ deps: { ...rest, ...(resolveBoss ? { resolveBoss } : {}) } })
  if (pr.exit === 0 && text(pr.out.baseRefName)) return pr.out.baseRefName
  const remote = read('git', ['symbolic-ref', '--short', 'refs/remotes/origin/HEAD'])
  return remote.ok && text(remote.output) ? remote.output : ''
}

// options: {head, base, reviewVerdictFile, reviewedHeadFile, routeFile, partial, honourRoute};
// deps: {spawn, cwd}. The partial exception applies when `partial` is true, or when `honourRoute`
// is true and the route file reads `partial`; a caller that sets neither never gets it.
export function readyGateReport(options = {}, deps = {}) {
  const read = commandReader(deps)
  const { reviewVerdict, reviewedHead, route } = reviewFiles(read, options)
  let head = options.head
  if (!text(head)) {
    const local = read('git', ['rev-parse', 'HEAD'])
    head = local.ok ? local.output : ''
  }
  const base = () => (text(options.base) ? options.base : baseBranch(read, deps))
  const treeDrift = reviewedTreeDrift(read, base, reviewedHead, head)
  const verdict = readyGate({
    reviewVerdict,
    reviewedHead,
    pushedHead: head,
    treeDrift,
    partial:
      options.partial === true ||
      (options.honourRoute === true && route.toLowerCase() === 'partial'),
  })
  return { ...verdict, reviewVerdict, reviewedHead, head, treeDrift }
}

export class UsageError extends Error {}

const OPTIONS = {
  head: { type: 'string' },
  base: { type: 'string' },
  'review-verdict-file': { type: 'string' },
  'reviewed-head-file': { type: 'string' },
  'route-file': { type: 'string' },
  partial: { type: 'boolean' },
}

export function main(args, deps = {}) {
  const command = args[0] === 'remap-reviewed-head' ? args[0] : null
  if (command) args = args.slice(1)
  const options = command
    ? {
        head: OPTIONS.head,
        base: OPTIONS.base,
        'reviewed-head-file': OPTIONS['reviewed-head-file'],
      }
    : OPTIONS
  let values
  try {
    ;({ values } = parseArgs({ args, options, strict: true, allowPositionals: false }))
  } catch (error) {
    throw new UsageError(`ready-gate: ${error.message}`)
  }
  if (values.head !== undefined && !sha(values.head))
    throw new UsageError('ready-gate: --head must be a 40-character hex SHA')
  if (command === 'remap-reviewed-head')
    return remapReviewedHead(
      commandReader(deps),
      {
        head: values.head,
        base: values.base,
        reviewedHeadFile: values['reviewed-head-file'],
      },
      deps,
    )
  return readyGateReport(
    {
      head: values.head,
      base: values.base,
      reviewVerdictFile: values['review-verdict-file'],
      reviewedHeadFile: values['reviewed-head-file'],
      routeFile: values['route-file'],
      partial: values.partial === true,
      honourRoute: true,
    },
    deps,
  )
}

if (isMainModule(import.meta.url)) {
  try {
    const result = main(process.argv.slice(2))
    process.stdout.write(`${JSON.stringify(result)}\n`)
    process.exitCode =
      'outcome' in result ? (result.outcome === 'unknown' ? 3 : 0) : result.ready ? 0 : 3
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
