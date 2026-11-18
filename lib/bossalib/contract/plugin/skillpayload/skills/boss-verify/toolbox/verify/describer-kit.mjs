// The shared harness of a proof-describe extension. A describer supplies one
// `collectItems({git, base, head, changes, allChanges})` function returning internal facts: text
// items `{area, text, paths, breaking}`, typed blocks `{area, block, paths}`, banner signals
// `{area, signal, paths}` and located risks `{area, risk, paths}`. This module turns it into the
// role's describe step, where a fact lands under an area only when one of its source files is in
// that area's `paths`, and into a CLI for running it by hand against a PR or a range.
//
// The core sets BOSS_VERIFY_TOOLBOX for every describer it starts, so a describer imports this
// module from the same core that validates its result:
//
//   const kit = await import(pathToFileURL(`${process.env.BOSS_VERIFY_TOOLBOX}/verify/describer-kit.mjs`).href)
//   await kit.describerMain(kit.envelopeDescriber(collectItems))
//
// Git plumbing only: every read is `git` against an explicit working directory, and nothing
// touches the network except `--pr`, which asks `gh` for the PR and may fetch its head.

import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'
import {
  MAX_DESCRIBE_OUTPUT_BYTES,
  boundDescription as coreBoundDescription,
  describerMain as coreDescriberMain,
} from './verify-describe.mjs'
import { MAX_BLOCKS, MAX_RISKS, MAX_SIGNALS, refineDescribed } from './verify-describe-blocks.mjs'
import { MAX_ITEMS, MAX_TEXT_CHARS } from './verify-proof.mjs'

export { MAX_BLOCKS, MAX_ITEMS, MAX_RISKS, MAX_SIGNALS, MAX_TEXT_CHARS }

/** Symbol lists inside one item's text hold this many names, then "+N more". */
export const MAX_LISTED = 5

/**
 * The core reads at most MAX_DESCRIBE_OUTPUT_BYTES of a describer's stdout. Blocks past this many
 * bytes of result are dropped, last first, so an oversized result never costs the area all of its
 * facts.
 */
export const MAX_RESULT_BYTES = MAX_DESCRIBE_OUTPUT_BYTES - 4 * 1024

/**
 * @typedef {{path: string, previousPath?: string, status: 'A' | 'M' | 'D' | 'R'}} Change
 * @typedef {ReturnType<typeof gitAt>} Git
 * @typedef {{area: string, paths: string[], text?: string, breaking?: boolean, block?: any,
 *   signal?: any, risk?: any}} Fact
 * @typedef {(input: {git: Git, base: string, head: string, changes: Change[],
 *   allChanges: Change[]}) => Fact[]} CollectItems
 * @typedef {{items?: string[], breaking?: boolean, blocks?: any[], signals?: any[],
 *   risks?: any[]}} Facts
 * @typedef {(number: number, options: {cwd: string}) => {headRefOid: string,
 *   baseRefName: string}} ReadPullRequest
 */

/**
 * A git reader bound to one working directory.
 * @param {string} [cwd]
 */
export function gitAt(cwd = process.cwd()) {
  /** @param {string[]} args @returns {string} */
  const run = (args) =>
    execFileSync('git', args, {
      cwd,
      encoding: 'utf8',
      maxBuffer: 64 * 1024 * 1024,
      stdio: ['ignore', 'pipe', 'pipe'],
    })

  /**
   * `git grep` output, '' when nothing matches (exit 1); any other failure throws.
   * @param {string[]} args
   */
  const grep = (args) => {
    try {
      return run(['grep', ...args])
    } catch (error) {
      if (error?.status === 1 && !String(error.stderr ?? '').trim()) return ''
      throw error
    }
  }

  /**
   * The file at `ref:path`, or '' when the path does not exist at that commit. Any other read
   * failure throws, so the core records a failed describer instead of facts from empty input.
   * @param {string} ref @param {string} file
   */
  const fileAt = (ref, file) => {
    if (!file || !run(['ls-tree', '--name-only', ref, '--', file]).trim()) return ''
    return run(['show', `${ref}:${file}`])
  }

  /**
   * Every tracked path under `dirs` at `ref`.
   * @param {string} ref @param {string[]} dirs
   */
  const filesAt = (ref, dirs) =>
    run(['ls-tree', '-r', '--name-only', ref, '--', ...dirs])
      .split('\n')
      .filter(Boolean)

  /**
   * Many files at `ref` in one `git cat-file --batch`: path -> text. Every path must exist at
   * `ref` (take them from filesAt); one that does not throws.
   * @param {string} ref @param {string[]} files
   */
  const readFiles = (ref, files) => {
    /** @type {Map<string, string>} */
    const contents = new Map()
    if (files.length === 0) return contents
    const output = execFileSync('git', ['cat-file', '--batch'], {
      cwd,
      input: `${files.map((file) => `${ref}:${file}`).join('\n')}\n`,
      maxBuffer: 256 * 1024 * 1024,
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    let offset = 0
    for (const file of files) {
      const end = output.indexOf(0x0a, offset)
      const header = output.subarray(offset, end).toString('utf8')
      const size = Number(header.match(/^\S+ blob (\d+)$/)?.[1] ?? NaN)
      if (!Number.isFinite(size)) throw new Error(`cannot read ${ref}:${file} (${header})`)
      contents.set(file, output.subarray(end + 1, end + 1 + size).toString('utf8'))
      offset = end + 1 + size + 1
    }
    return contents
  }

  return { cwd, fileAt, filesAt, grep, readFiles, run }
}

/**
 * `git diff --name-status -M` between two commits, as `{path, previousPath?, status}` with status
 * one of A, M, D, R (copies read as A, type changes as M).
 * @param {Git} git @param {string} base @param {string} head
 * @returns {Change[]}
 */
export function changedFiles(git, base, head) {
  const fields = git.run(['diff', '--name-status', '-M', '-z', base, head]).split('\0')
  /** @type {Change[]} */
  const changes = []
  for (let index = 0; index < fields.length - 1;) {
    const code = fields[index]
    if (!code) break
    if (code.startsWith('R') || code.startsWith('C')) {
      const renamed = code.startsWith('R')
      changes.push({
        path: fields[index + 2],
        ...(renamed ? { previousPath: fields[index + 1] } : {}),
        status: renamed ? 'R' : 'A',
      })
      index += 3
    } else {
      changes.push({
        path: fields[index + 1],
        status: code[0] === 'A' || code[0] === 'D' ? code[0] : 'M',
      })
      index += 2
    }
  }
  return changes
}

/**
 * `head` and the merge base of `base` and `head`, both as commits. The core's envelope carries the
 * base-branch tip, so diffing against it directly would read work merged after branching as
 * reversed by this change. A ref git cannot read throws, naming it.
 * @param {Git} git @param {string} base @param {string} head
 */
export function resolveRange(git, base, head) {
  /** @param {string} ref */
  const commit = (ref) => {
    try {
      return git.run(['rev-parse', '--verify', '--quiet', `${ref}^{commit}`]).trim()
    } catch {
      throw new Error(`cannot read commit ${ref}`)
    }
  }
  const baseSha = commit(base)
  const headSha = commit(head)
  try {
    return { base: git.run(['merge-base', baseSha, headSha]).trim(), head: headSha }
  } catch {
    throw new Error(`no merge base for ${base} and ${head}`)
  }
}

/**
 * The role's describe step for one describer: changes over the envelope's range, kept to the
 * area's paths, through `collectItems`, then scoped by roleFacts so a fact lands only under an
 * area that holds one of its source files. `allChanges` is the whole range, for facts about the
 * change as a whole (a banner signal, say).
 * @param {CollectItems} collectItems
 * @returns {(envelope: any, options?: {cwd?: string}) => Facts}
 */
export function envelopeDescriber(collectItems) {
  return (envelope, { cwd } = {}) => {
    /** @type {string[]} */
    const paths = envelope?.area?.paths ?? []
    if (paths.length === 0) return { breaking: false, items: [] }
    if (!envelope.baseSha || !envelope.headSha)
      throw new Error('proof-describe envelope is missing baseSha or headSha')
    const git = gitAt(cwd)
    const { base, head } = resolveRange(git, envelope.baseSha, envelope.headSha)
    const inArea = new Set(paths)
    const allChanges = changedFiles(git, base, head)
    const changes = allChanges.filter(
      (change) =>
        inArea.has(change.path) || (change.previousPath && inArea.has(change.previousPath)),
    )
    return roleFacts(collectItems({ allChanges, base, changes, git, head }), paths)
  }
}

// ---------------------------------------------------------------------------
// Small text helpers and fact builders shared by describers
// ---------------------------------------------------------------------------

/**
 * At most `max` names, then "(+N more)".
 * @param {string[]} names @param {number} [max]
 */
export function listed(names, max = MAX_LISTED) {
  const shown = names.slice(0, max).join(', ')
  const hidden = names.length - max
  return hidden > 0 ? `${shown} (+${hidden} more)` : shown
}

/** @param {number} count @param {string} one @param {string} [many] */
export const plural = (count, one, many = `${one}s`) => `${count} ${count === 1 ? one : many}`

/** @param {string} file */
export const basename = (file) => file.split('/').pop() ?? ''

/** @param {(string | null | undefined)[] | undefined} paths @returns {string[]} */
const uniquePaths = (paths) => [
  .../** @type {Set<string>} */ (new Set((paths ?? []).filter(Boolean))),
]

/**
 * A text item.
 * @param {string} area @param {string} text @param {string[]} paths @param {boolean} [breaking]
 * @returns {Fact}
 */
export const item = (area, text, paths, breaking = false) => ({
  area,
  breaking,
  paths: uniquePaths(paths),
  text,
})

/**
 * A typed proof-page block (`{kind, ...}`, the shapes verify-describe-blocks.mjs validates).
 * @param {string} area @param {any} block @param {string[]} paths @returns {Fact}
 */
export const blockFact = (area, block, paths) => ({ area, block, paths: uniquePaths(paths) })

/**
 * A banner signal `{key, label, state, summary}`.
 * @param {any} signal @param {string[]} paths @returns {Fact}
 */
export const signalFact = (signal, paths) => ({ area: 'signal', paths: uniquePaths(paths), signal })

/**
 * A risk `{level, title, impact, guard?, at: {path, line}}`, located in the diff.
 * @param {string} area @param {any} risk @param {string[]} [paths] @returns {Fact}
 */
export const riskFact = (area, risk, paths) => ({
  area,
  paths: uniquePaths(paths ?? [risk.at.path]),
  risk,
})

// ---------------------------------------------------------------------------
// The proof-describe role
// ---------------------------------------------------------------------------

/**
 * Scope internal facts to one proof-page area: a fact is in scope when one of its source files is
 * in `areaPaths`. An in-scope text item contributes its text, and blocks, signals and risks travel
 * as themselves. An empty typed list is left out.
 * @param {Fact[]} facts @param {string[] | undefined} areaPaths @returns {Facts}
 */
export function roleFacts(facts, areaPaths) {
  const inArea = new Set(areaPaths ?? [])
  const inScope = facts.filter((fact) => (fact.paths ?? []).some((file) => inArea.has(file)))
  const texts = inScope.filter((fact) => typeof fact.text === 'string')
  const typed = {
    blocks: inScope.filter((fact) => fact.block).map((fact) => fact.block),
    risks: inScope.filter((fact) => fact.risk).map((fact) => fact.risk),
    signals: inScope.filter((fact) => fact.signal).map((fact) => fact.signal),
  }
  return {
    breaking: texts.some((fact) => fact.breaking),
    items: texts.map((fact) => /** @type {string} */ (fact.text)),
    ...Object.fromEntries(Object.entries(typed).filter(([, list]) => list.length > 0)),
  }
}

/**
 * The entries the core would accept under `key`, at most `max`, in order.
 * @param {'blocks' | 'signals' | 'risks'} key @param {any[] | undefined} list @param {number} max
 */
const accepted = (key, list, max) =>
  (Array.isArray(list) ? list : [])
    .filter((entry) => refineDescribed({ [key]: [entry] }).length === 0)
    .slice(0, max)

/**
 * A describer's facts as a valid, bounded proof-describe result. A block, signal or risk the core
 * would reject is dropped on its own, because one invalid entry makes the core reject the whole
 * result; each signal key is kept once. The core's boundDescription assembles the result, then
 * blocks are dropped, last first, while it is over MAX_RESULT_BYTES.
 * @param {string} extension @param {Facts} [facts]
 */
export function boundDescription(
  extension,
  { blocks, breaking = false, items, risks, signals } = {},
) {
  // Validate before de-duplicating, so an invalid signal never hides a valid one with its key.
  const firstPerKey = accepted('signals', signals, Infinity).filter(
    (signal, index, all) => all.findIndex((other) => other.key === signal.key) === index,
  )
  const typed = {
    blocks: accepted('blocks', blocks, MAX_BLOCKS),
    signals: firstPerKey.slice(0, MAX_SIGNALS),
    risks: accepted('risks', risks, MAX_RISKS),
  }
  /** @type {ReturnType<typeof coreBoundDescription> & Facts} */
  const result = coreBoundDescription(extension, {
    items,
    breaking,
    ...Object.fromEntries(Object.entries(typed).filter(([, list]) => list.length > 0)),
  })
  while (result.blocks?.length && Buffer.byteLength(JSON.stringify(result)) > MAX_RESULT_BYTES) {
    result.blocks.pop()
    if (result.blocks.length === 0) delete result.blocks
  }
  return result
}

/**
 * The describer script's entry point: the core's describerMain (no flags, the envelope on stdin,
 * one result on stdout), bounded through this module's boundDescription. A throw exits non-zero,
 * which the core records as a failed describer.
 * @param {(envelope: any, options: {cwd: string}) => Facts | Promise<Facts>} describe
 * @param {Parameters<typeof coreDescriberMain>[1]} [io]
 */
export function describerMain(describe, io) {
  return coreDescriberMain(
    async (envelope, options) =>
      boundDescription(envelope?.extension, await describe(envelope, options)),
    io,
  )
}

// ---------------------------------------------------------------------------
// The by-hand CLI
// ---------------------------------------------------------------------------

/**
 * The CLI flags: `--base <ref>`, `--head <ref>`, `--pr <n>` and `--json`. Unknown flags throw.
 * @param {string[]} argv
 * @returns {{base?: string, head?: string, pr?: number, json?: boolean}}
 */
export function parseArgv(argv) {
  const { values } = parseArgs({
    args: argv,
    options: {
      base: { type: 'string' },
      head: { type: 'string' },
      pr: { type: 'string' },
      json: { type: 'boolean' },
    },
    strict: true,
    allowPositionals: false,
  })
  /** @type {{base?: string, head?: string, pr?: number, json?: boolean}} */
  const options = {}
  if (values.base !== undefined) options.base = values.base
  if (values.head !== undefined) options.head = values.head
  if (values.pr !== undefined) {
    const pr = Number(values.pr)
    if (!Number.isInteger(pr) || pr <= 0)
      throw new Error(`--pr needs a PR number, got ${values.pr}`)
    options.pr = pr
  }
  if (values.json) options.json = true
  return options
}

/** @type {ReadPullRequest} */
const ghPullRequest = (number, { cwd }) =>
  JSON.parse(
    execFileSync('gh', ['pr', 'view', String(number), '--json', 'headRefOid,baseRefName'], {
      cwd,
      encoding: 'utf8',
      maxBuffer: 64 * 1024 * 1024,
    }),
  )

/**
 * A PR's range: `origin/<base branch>` and its head commit. The head is fetched into
 * refs/remotes/pr/<n> only when that commit is missing.
 * @param {Git} git @param {number} number @param {ReadPullRequest} readPullRequest
 */
function resolvePullRequest(git, number, readPullRequest) {
  const view = readPullRequest(number, { cwd: git.cwd })
  if (!view?.headRefOid || !view?.baseRefName)
    throw new Error(`cannot read the head and base of PR ${number}`)
  const present = (() => {
    try {
      return git.run(['cat-file', '-t', view.headRefOid]).trim() === 'commit'
    } catch {
      return false
    }
  })()
  if (!present)
    git.run(['fetch', '--quiet', 'origin', `+refs/pull/${number}/head:refs/remotes/pr/${number}`])
  return { base: `origin/${view.baseRefName}`, head: view.headRefOid }
}

/**
 * The by-hand CLI: every item over the whole range, unscoped, with its source files.
 * @param {string} name @param {CollectItems} collectItems @param {string[]} argv
 * @param {{cwd?: string, readPullRequest?: ReadPullRequest}} [options]
 */
export function cliMain(name, collectItems, argv, { cwd, readPullRequest = ghPullRequest } = {}) {
  const options = parseArgv(argv)
  const git = gitAt(cwd)
  const target =
    options.pr !== undefined
      ? resolvePullRequest(git, options.pr, readPullRequest)
      : { base: options.base, head: options.head ?? 'HEAD' }
  if (!target.base)
    throw new Error(`usage: ${name} --base <ref> [--head <ref>] [--json] | --pr <n> [--json]`)
  const { base, head } = resolveRange(git, target.base, target.head)
  const changes = changedFiles(git, base, head)
  const items = collectItems({ allChanges: changes, base, changes, git, head })
  return options.json ? `${JSON.stringify({ base, head, items }, null, 2)}\n` : renderItems(items)
}

/**
 * Run a describer's CLI when its module is the entry point.
 * @param {string} moduleUrl @param {string} name @param {CollectItems} collectItems
 */
export function runCli(moduleUrl, name, collectItems) {
  if (!isMainModule(moduleUrl)) return
  try {
    process.stdout.write(cliMain(name, collectItems, process.argv.slice(2)))
  } catch (error) {
    process.stderr.write(`${name}: ${error.message}\n`)
    process.exitCode = 1
  }
}

/**
 * A typed block as one CLI line.
 * @param {any} block
 */
function blockLine(block) {
  switch (block.kind) {
    case 'schema':
      return `schema: ${block.types
        .map(
          (/** @type {any} */ type) =>
            `${type.change} ${type.keyword} ${type.name} (${type.fields.length})`,
        )
        .join(', ')}`
    case 'table':
      return `table ${block.name} ${block.change}: ${block.columns
        .map((/** @type {any} */ column) => `${column.name} ${column.type}`)
        .join(', ')}`
    case 'route':
      return `route ${block.change} ${block.method} ${block.path} auth=${block.auth}`
    case 'client-call':
      return `client call ${block.name}${block.path ? ` (${block.path})` : ''}`
    case 'code':
      return `code ${block.path}:${block.start} ${block.title ?? ''}`.trim()
    case 'steps':
      return `steps: ${block.steps.join(' | ')}`
    default:
      return String(block.kind)
  }
}

/**
 * The CLI's plain rendering: one line per fact, its area in brackets.
 * @param {Fact[]} facts
 */
export function renderItems(facts) {
  if (facts.length === 0) return 'No describable change.\n'
  return `${facts
    .map((fact) => {
      const text = fact.block
        ? blockLine(fact.block)
        : fact.signal
          ? `${fact.signal.label}: ${fact.signal.state}; ${fact.signal.summary}`
          : fact.risk
            ? `${fact.risk.level} risk at ${fact.risk.at.path}:${fact.risk.at.line}: ${fact.risk.title}`
            : fact.text
      return `- [${fact.area}]${fact.breaking ? ' BREAKING' : ''} ${text}`
    })
    .join('\n')}\n`
}
