// Built-in proof-describe script: lists an area's deleted files and deleted exports and marks the
// area breaking when the head tree still mentions one. The scan is a whole-word `git grep`, so a
// false positive only adds an ask; a search that cannot run counts as a reference.

import { spawnSync } from 'node:child_process'
import path from 'node:path'

import { isMainModule } from '../main-module.mjs'
import { readChangedFiles, runTool } from './verify-diff.mjs'
import { describerMain } from './verify-describe.mjs'
import { stripModulePath } from './verify-moves.mjs'

/**
 * @typedef {import('./verify-diff.mjs').Run} Run
 * @typedef {import('./verify-moves.mjs').ChangedFile} ChangedFile
 * @typedef {{module: string, directory: boolean}} Relative
 * @typedef {{key: string, label: string, exclude?: string, relative?: Relative, movedFrom?: string}} Target
 */

/** Searches per run, and the wall-clock budget for all of them, inside the 60-second call. */
export const MAX_SEARCHES = 40
export const SEARCH_BUDGET_MS = 40_000
const MAX_REFERENCE_LINES = 5
const MAX_LISTED_FILES = 3

const posix = path.posix
const plural = (/** @type {number} */ n, /** @type {string} */ one) =>
  `${n} ${one}${n === 1 ? '' : 's'}`

/** @param {string} file @returns {'js' | 'go' | 'py' | null} */
function language(file) {
  if (/\.(?:js|mjs|cjs|jsx|ts|tsx|mts|cts)$/.test(file)) return 'js'
  if (file.endsWith('.go')) return 'go'
  if (file.endsWith('.py')) return 'py'
  return null
}

/** Names a changed line defines as an export. @param {'js' | 'go' | 'py'} lang @param {string} line */
export function exportedNames(lang, line) {
  if (lang === 'go') {
    const match = /^(?:func\s+|type\s+|var\s+|const\s+)([A-Z]\w*)\b/.exec(line)
    return match ? [match[1]] : []
  }
  if (lang === 'py') {
    const match = /^(?:async\s+)?(?:def|class)\s+([A-Za-z]\w*)\b/.exec(line)
    return match ? [match[1]] : []
  }
  if (/^\s*export\s+default\b/.test(line)) return []
  const declared =
    /^\s*export\s+(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?(?:function\*?|class|const|let|var|interface|type|enum|namespace)\s+([A-Za-z_$][\w$]*)/.exec(
      line,
    )
  if (declared) return [declared[1]]
  const list = /^\s*export\s+(?:type\s+)?\{([^}]*)\}/.exec(line)
  if (!list) return []
  return list[1]
    .split(',')
    .map(
      (part) =>
        part
          .trim()
          .split(/\s+as\s+/)
          .at(-1)
          ?.trim() ?? '',
    )
    .filter((name) => /^[A-Za-z_$][\w$]*$/.test(name) && name !== 'default')
}

/**
 * Where an importer finds a definition: a Go export belongs to its package directory, any other
 * export to its module. @param {ChangedFile} file @param {'-' | '+'} sign
 */
function definitions(file, sign) {
  const lang = language(file.path)
  if (!lang || typeof file.patch !== 'string') return []
  const scope = lang === 'go' ? posix.dirname(file.path) : stripModulePath(file.path)
  return file.patch
    .split('\n')
    .filter((line) => line.startsWith(sign) && !/^(?:---|\+\+\+) /.test(line))
    .flatMap((line) =>
      exportedNames(lang, line.slice(1)).map((name) => ({ id: `${lang}:${name}`, name, scope })),
    )
}

/** The extension-less `dir/basename` an importer would write for a file. @param {string} file */
const pathTail = (file) => stripModulePath(file).split('/').slice(-2).join('/')

const RELATIVE_SPEC = /(['"`])(\.{1,2}(?:\/[^'"`\n]*)?)\1/g

/**
 * Does a relative specifier on a line of `file` resolve to the deleted module (or, for a deleted
 * directory, into it)? @param {string} file @param {string} line @param {Relative} relative
 */
function importsModule(file, line, { module, directory }) {
  for (const [, , spec] of line.matchAll(RELATIVE_SPEC)) {
    const resolved = stripModulePath(posix.normalize(posix.join(posix.dirname(file), spec)))
    if (resolved === module || (directory && resolved.startsWith(`${module}/`))) return true
  }
  return false
}

/**
 * @param {{area: {paths: string[]}, baseSha: string, headSha: string}} envelope
 * @param {{cwd?: string, run?: Run, now?: () => number}} [options]
 */
export function describeDeletions(envelope, { cwd, run = spawnSync, now = Date.now } = {}) {
  const { headSha } = envelope
  const files = readChangedFiles({ baseSha: envelope.baseSha, headSha, cwd, run })
  const paths = new Set(envelope.area.paths)
  const own = files.filter((file) => paths.has(file.path))
  /** @type {Map<string, Set<string>>} */
  const addedScopes = new Map()
  for (const { id, scope } of files.flatMap((file) => definitions(file, '+')))
    addedScopes.set(id, (addedScopes.get(id) ?? new Set()).add(scope))
  /** @type {Map<string, Target>} */
  const exports = new Map()
  for (const file of own)
    for (const { id, name, scope } of definitions(file, '-')) {
      const added = addedScopes.get(id)
      const key = `${scope}\0${id}`
      // Re-defined where importers already look: an edit, not a deletion.
      if (added?.has(scope) || exports.has(key)) continue
      /** @type {Target} */
      const target = { key: name, label: `\`${name}\``, exclude: file.path }
      // Defined again elsewhere: only a file that still names the old module is a stale importer.
      if (added) target.movedFrom = posix.basename(scope)
      exports.set(key, target)
    }
  const deleted = own.filter((file) => file.status === 'removed').map((file) => file.path)
  if (deleted.length === 0 && exports.size === 0) return { items: [], breaking: false }

  /** @param {string} dir */
  const inHead = (dir) => runTool(run, 'git', ['cat-file', '-e', `${headSha}:${dir}`], { cwd }).ok
  /** @type {Map<string, Target>} */
  const fileTargets = new Map()
  /** @type {Map<string, boolean>} */
  const dirPresent = new Map()
  for (const file of [...deleted].sort()) {
    let gone = ''
    for (let dir = posix.dirname(file); dir !== '.'; dir = posix.dirname(dir)) {
      if (!dirPresent.has(dir)) dirPresent.set(dir, inHead(dir))
      if (dirPresent.get(dir)) break
      gone = dir
    }
    const key = gone ? gone.split('/').slice(-2).join('/') : pathTail(file)
    const label = gone ? `${gone}/` : file
    if (!fileTargets.has(key)) fileTargets.set(key, { key, label })
    // A relative importer near the module names only its basename, so search for that and keep
    // the lines whose specifier resolves to the deleted module.
    const module = gone || stripModulePath(file)
    const relativeKey = `\0${module}`
    if (!fileTargets.has(relativeKey))
      fileTargets.set(relativeKey, {
        key: posix.basename(module),
        label,
        relative: { module, directory: Boolean(gone) },
      })
  }

  const targets = [...fileTargets.values(), ...exports.values()]
  /** @param {string} entry */
  const unprefix = (entry) =>
    entry.startsWith(`${headSha}:`) ? entry.slice(headSha.length + 1) : entry
  /** @param {string} stdout */
  const listed = (stdout) => stdout.split('\0').filter(Boolean).map(unprefix)
  /** `-z` match lines read `<sha>:<path>\0<line>`. @param {string} stdout @param {Relative} relative */
  const relativeHits = (stdout, relative) =>
    stdout.split('\n').flatMap((record) => {
      const at = record.indexOf('\0')
      const file = unprefix(record.slice(0, at))
      return at > 0 && importsModule(file, record.slice(at + 1), relative) ? [file] : []
    })
  const started = now()
  let searches = 0
  /**
   * A `git grep` for one whole word, or null once the cap or budget is spent.
   * @param {string} word @param {string[]} mode @param {string[]} pathspec
   */
  const grep = (word, mode, pathspec) => {
    if (searches >= MAX_SEARCHES || now() - started > SEARCH_BUDGET_MS) return null
    searches += 1
    const found = runTool(
      run,
      'git',
      ['grep', ...mode, '-z', '-I', '-w', '-F', '-e', word, headSha, '--', ...pathspec],
      { cwd },
    )
    return found.ok || found.status === 1 ? found.stdout : null
  }
  /** @param {Target} target @returns {string[] | null} */
  const search = (target) => {
    if (target.relative) {
      // A relative search lists match lines under the module's parent directory.
      const scope = posix.dirname(target.relative.module)
      const stdout = grep(target.key, [], scope === '.' ? [] : [`:(top)${scope}`])
      return stdout === null ? null : relativeHits(stdout, target.relative)
    }
    const stdout = grep(target.key, ['-l'], [])
    if (stdout === null) return null
    const hits = listed(stdout).filter((file) => file !== target.exclude)
    if (!target.movedFrom || hits.length === 0) return hits
    const naming = grep(
      target.movedFrom,
      ['-l'],
      hits.map((file) => `:(top,literal)${file}`),
    )
    return naming === null ? null : listed(naming)
  }
  /** @type {{target: Target, files: string[]}[]} */
  const referenced = []
  let unchecked = 0
  for (const target of targets) {
    const hits = search(target)
    if (hits === null) {
      unchecked += 1
      continue
    }
    const files = hits.filter((file) => file !== target.exclude)
    const prior = referenced.find((entry) => entry.target.label === target.label)
    if (prior) prior.files = [...new Set([...prior.files, ...files])].sort()
    else if (files.length > 0) referenced.push({ target, files: [...new Set(files)].sort() })
  }

  const items = []
  if (deleted.length > 0) items.push(`${plural(deleted.length, 'file')} deleted`)
  const removed = [...exports.values()].filter((target) => !target.movedFrom).length
  if (removed > 0) items.push(`${plural(removed, 'export')} deleted`)
  for (const { target, files: hits } of referenced.slice(0, MAX_REFERENCE_LINES)) {
    const listed = hits.slice(0, MAX_LISTED_FILES).join(', ')
    const more = hits.length > MAX_LISTED_FILES ? ', …' : ''
    items.push(
      `${target.label} still referenced in ${plural(hits.length, 'file')}: ${listed}${more}`,
    )
  }
  if (referenced.length > MAX_REFERENCE_LINES)
    items.push(`${referenced.length - MAX_REFERENCE_LINES} more deletions still referenced`)
  if (unchecked > 0) items.push(`${plural(unchecked, 'deletion')} not checked for references`)
  return { items, breaking: referenced.length > 0 || unchecked > 0 }
}

export const main = () => describerMain(describeDeletions)

if (isMainModule(import.meta.url)) await main()
