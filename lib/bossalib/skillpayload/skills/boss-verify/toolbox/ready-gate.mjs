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
// A waiver admits review-quality reasons only, never provenance or an unknown review verdict.
// The partial exception admits a capped review (`review-not-clean`) for boss-build's PARTIAL route;
// every other reason still refuses, and the verdict says `admittedFor: 'partial'` when it applied.
// The CLI applies it when `--partial` is given or the route file (default
// `<git-dir>/boss-build-ready-route`, written by boss-build's Step 9) reads `partial`, so the route
// survives between shell invocations. In-process callers get it only by asking: commit-status.mjs
// never does, because a `boss/build` receipt is REVIEW_READY-only.
//
// Node builtins plus ./main-module.mjs only: commit-status.mjs imports this gate in-process, so its
// import closure stays these three files. completion-gate.mjs shares the reason vocabulary and the
// tree-drift reader from here.

import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from './main-module.mjs'

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

// A failed read is `{ok: false, status}`, never an empty successful payload; `status` is the exit
// code (null when the command did not run to an exit).
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
// be read. Same merge base: the trees must be equal. Different merge bases: a three-way merge of the
// reviewed head onto the new base (`git merge-tree --merge-base`, git >= 2.40; an older git fails,
// which reads unknown). `read(command, args)` answers `{ok: true, output}` or `{ok: false, status}`.
// `base` is a branch (an `origin/` prefix is tolerated) or a function returning one, called only
// when the heads differ.
export function reviewedTreeDrift(read, base, reviewedHead, pushedHead) {
  if (!sha(reviewedHead) || !sha(pushedHead)) return 'unknown'
  if (reviewedHead.toLowerCase() === pushedHead.toLowerCase()) return 'none'
  const branch = typeof base === 'function' ? base() : base
  if (!text(branch)) return 'unknown'
  const remote = `origin/${branch.trim().replace(/^origin\//, '')}`
  const oid = (args) => {
    const result = read('git', args)
    const value = result.ok && typeof result.output === 'string' ? result.output.trim() : ''
    return sha(value) ? value.toLowerCase() : null
  }
  const tree = (head) => oid(['rev-parse', '--verify', '--quiet', `${head}^{tree}`])
  const reviewedBase = oid(['merge-base', remote, reviewedHead])
  const pushedBase = oid(['merge-base', remote, pushedHead])
  const pushedTree = tree(pushedHead)
  if (!reviewedBase || !pushedBase || !pushedTree) return 'unknown'
  if (reviewedBase === pushedBase) {
    const reviewedTree = tree(reviewedHead)
    if (!reviewedTree) return 'unknown'
    return reviewedTree === pushedTree ? 'none' : 'changed'
  }
  const replay = read('git', [
    'merge-tree',
    '--write-tree',
    `--merge-base=${reviewedBase}`,
    pushedBase,
    reviewedHead,
  ])
  if (!replay.ok) return replay.status === 1 ? 'changed' : 'unknown'
  const replayed = String(replay.output ?? '')
    .split('\n')[0]
    .trim()
  if (!sha(replayed)) return 'unknown'
  return replayed.toLowerCase() === pushedTree ? 'none' : 'changed'
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

// The PR base, else the remote default branch; '' when neither reads.
export function baseBranch(read) {
  const pr = read('gh', ['pr', 'view', '--json', 'baseRefName'], { json: true })
  if (pr.ok && text(pr.payload?.baseRefName)) return pr.payload.baseRefName
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
  const base = () => (text(options.base) ? options.base : baseBranch(read))
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
  let values
  try {
    ;({ values } = parseArgs({ args, options: OPTIONS, strict: true, allowPositionals: false }))
  } catch (error) {
    throw new UsageError(`ready-gate: ${error.message}`)
  }
  if (values.head !== undefined && !sha(values.head))
    throw new UsageError('ready-gate: --head must be a 40-character hex SHA')
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
    process.exitCode = result.ready ? 0 : 3
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
