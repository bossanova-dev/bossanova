// Built-in proof-describe script: lists an area's deleted files and deleted exports and marks the
// area breaking when the head tree still mentions one. Moved exports count only imports of the
// old module. Conservative false positives add an ask; a failed search counts as a reference.

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
 * `movedFrom` is the old extension-less module path (Go: package directory).
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

/** @param {string} value */
const escaped = (value) => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/** @param {string} list @param {string} name */
const namesExport = (list, name) =>
  list.split(',').some(
    (part) =>
      part
        .trim()
        .replace(/^type\s+/, '')
        .split(/\s+as\s+|\s*[:=]\s*/)[0] === name,
  )

/**
 * @param {'js' | 'go' | 'py' | null} lang @param {string} file @param {string[]} lines
 * @param {{key: string, movedFrom: string}} moved
 */
export function staleImporter(lang, file, lines, { key, movedFrom }) {
  const basename = posix.basename(movedFrom)
  /** @param {string} qualifier */
  const uses = (qualifier) => {
    const pattern = new RegExp(
      `(?<![\\w$])${escaped(qualifier)}\\s*\\.\\s*${escaped(key)}(?![\\w$])`,
    )
    if (lang !== 'js' && lang !== null) return lines.some((line) => pattern.test(line))
    const binding = `(?<![\\w$])${escaped(qualifier)}`
    const member = new RegExp(
      `${binding}\\s*(?:\\?\\.\\s*|\\.\\s*)${escaped(key)}(?![\\w$])|` +
        `${binding}\\s*(?:\\?\\.\\s*)?\\[\\s*(['"\x60])${escaped(key)}\\1\\s*\\]`,
    )
    const destructured = new RegExp(
      `\\{([^}]*)\\}\\s*=\\s*${binding}(?![\\w$]|\\s*(?:\\?\\.|\\.|\\[))`,
    )
    return lines.some((line) => {
      if (member.test(line)) return true
      const match = destructured.exec(line)
      return Boolean(match && namesExport(match[1], key))
    })
  }
  /** @param {string} spec @param {string} line */
  const jsModule = (spec, line) =>
    spec.startsWith('.')
      ? importsModule(file, line, { module: movedFrom, directory: false })
      : posix.basename(stripModulePath(spec)) === basename
  if (lang === 'go' && posix.dirname(file) === movedFrom) return true
  return lines.some((line) => {
    // Embedded JS (MDX, Vue and other containers) uses the same import forms; prose alone does not.
    if (lang === 'js' || lang === null) {
      const from = /^\s*(?:import|export)\s+(.*?)\s+from\s*['"]([^'"]+)['"]/.exec(line)
      const require = /^\s*(?:const|let|var)\s+(.*?)\s*=\s*require\s*\(\s*['"]([^'"]+)['"]/.exec(
        line,
      )
      const close = /^\s*[^{}]*}\s*(?:from\s*|=\s*require\s*\(\s*)['"]([^'"]+)['"]/.exec(line)
      if (close && jsModule(close[1], line)) return true
      const dynamic = /\bimport\s*\(\s*['"]([^'"]+)['"]/.exec(line)
      const member = /\brequire\s*\(\s*['"]([^'"]+)['"]\s*\)\s*\.\s*([\w$]+)/.exec(line)
      if (dynamic && !/^\s*(?:\/\/|\*)/.test(line) && jsModule(dynamic[1], line)) return true
      if (member && !/^\s*(?:\/\/|\*)/.test(line) && jsModule(member[1], line))
        return member[2] === key
      const match = from ?? require
      if (!match) {
        // Grep may omit a split import's opening line; incomplete old-module clauses stay stale.
        const continuation =
          /^\s*(?:[\w$*,:{}\s]*\bfrom\s*|(?:[\w$,:{}\s]*=\s*)?require\s*\(\s*)['"]([^'"]+)['"]/.exec(
            line,
          )
        return Boolean(continuation && jsModule(continuation[1], line))
      }
      if (!jsModule(match[2], line)) return false
      const clause = match[1].replace(/^type\s+/, '')
      const named = /\{([^}]*)\}/.exec(clause)
      if (named && namesExport(named[1], key)) return true
      if (/^\*/.test(clause) && /^\s*export\b/.test(line)) return true
      const namespace = /(?:^|,)\s*\*\s+as\s+([\w$]+)/.exec(clause)
      const binding = namespace?.[1] ?? /^([A-Za-z_$][\w$]*)/.exec(clause)?.[1]
      // Unknown containers may transform default bindings; keep an old-module import ambiguous.
      if (lang === null && from && binding && !namespace) return true
      return Boolean(binding && uses(binding))
    }
    if (lang === 'go') {
      const match = /^\s*(?:import\s+)?(?:(\w+|\.)\s+)?"([^"]+)"/.exec(line)
      if (!match || posix.basename(stripModulePath(match[2])) !== basename) return false
      const qualifier = match[1] ?? basename
      // Unaliased package names differing from their directory need compiler checks, not guessing.
      return qualifier === '.' || (qualifier !== '_' && uses(qualifier))
    }
    if (lang === 'py') {
      const from = /^\s*from\s+([.\w]+)\s+import\s+(.+)/.exec(line)
      if (from) {
        const [, spec, clause] = from
        const list = clause.split(/[;#]/)[0].trim()
        const dots = /^\.+/.exec(spec)?.[0].length ?? 0
        const module = dots
          ? posix.normalize(
              posix.join(
                posix.dirname(file),
                ...Array(dots - 1).fill('..'),
                spec.slice(dots).replaceAll('.', '/'),
              ),
            )
          : spec.replaceAll('.', '/')
        const matches = dots ? module === movedFrom : posix.basename(module) === basename
        if (matches && (/^[(*]/.test(list) || list.endsWith('\\') || namesExport(list, key)))
          return true
        // `from pkg import old as alias` binds a module rather than an export from pkg.
        return list.split(',').some((part) => {
          const binding = /^\s*(\w+)(?:\s+as\s+(\w+))?\s*$/.exec(part)
          if (!binding) return false
          const importedModule = posix.join(module, binding[1])
          const oldModule = dots
            ? importedModule === movedFrom
            : posix.basename(importedModule) === basename
          return oldModule && uses(binding[2] ?? binding[1])
        })
      }
      const imported = /^\s*import\s+(.+)/.exec(line)
      return Boolean(
        imported?.[1].split(',').some((part) => {
          const match = /^\s*([\w.]+)(?:\s+as\s+(\w+))?/.exec(part)
          return match && match[1].split('.').at(-1) === basename && uses(match[2] ?? match[1])
        }),
      )
    }
    return false
  })
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
      // Defined again elsewhere: retain the old scope to resolve stale import specifiers.
      if (added) target.movedFrom = scope
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
  /** `-z` match lines read `<sha>:<path>\0<line>`. @param {string} stdout */
  const matchLines = (stdout) =>
    stdout.split('\n').flatMap((record) => {
      const at = record.indexOf('\0')
      const file = unprefix(record.slice(0, at))
      return at > 0 ? [{ file, line: record.slice(at + 1) }] : []
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
      return stdout === null
        ? null
        : matchLines(stdout)
            .filter(({ file, line }) =>
              importsModule(file, line, /** @type {Relative} */ (target.relative)),
            )
            .map(({ file }) => file)
    }
    if (target.movedFrom) {
      const stdout = grep(target.key, ['--all-match', '-e', posix.basename(target.movedFrom)], [])
      if (stdout === null) return null
      /** @type {Map<string, string[]>} */
      const byFile = new Map()
      for (const { file, line } of matchLines(stdout)) {
        if (file === target.exclude) continue
        if (!byFile.has(file)) byFile.set(file, [])
        byFile.get(file)?.push(line)
      }
      const moved = { key: target.key, movedFrom: target.movedFrom }
      return [...byFile]
        .filter(([file, lines]) => staleImporter(language(file), file, lines, moved))
        .map(([file]) => file)
    }
    const stdout = grep(target.key, ['-l'], [])
    return stdout === null ? null : listed(stdout).filter((file) => file !== target.exclude)
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
