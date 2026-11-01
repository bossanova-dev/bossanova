// Pure-move analysis: is a changed file's diff nothing but import specifiers that a rename in the
// same diff explains? Shared by the area detector (which drops such files' asks) and the moves
// describer (which reports them), so the two never disagree. Pure: Node builtins only.

import path from 'node:path'

/**
 * @typedef {{path: string, previousPath?: string, status?: string, changes?: number, patch?: string}} ChangedFile
 * @typedef {{from: string, to: string, directory?: true}} RenamePair
 * @typedef {{template: string, spec: string, sideEffect?: true}} ImportLine
 * @typedef {{removed: ImportLine[], added: ImportLine[]}} ImportHunk
 * @typedef {ImportHunk & {hunks: ImportHunk[]}} ImportDiff
 */

const posix = path.posix
const JS = /\.(?:js|mjs|cjs|jsx|ts|tsx|mts|cts)$/
const SPEC = '\u0000'

/** Strip a file's last extension and a trailing `/index` (or Python `/__init__`). @param {string} file */
export function stripModulePath(file) {
  const base = posix.basename(file)
  const dot = base.lastIndexOf('.')
  const bare = dot > 0 ? file.slice(0, file.length - (base.length - dot)) : file
  return bare.replace(/\/(?:index|__init__)$/, '')
}

/** @param {string} value */
const segments = (value) => value.split('/').filter(Boolean)
/** @param {string} a @param {string} b */
const compare = (a, b) => (a < b ? -1 : a > b ? 1 : 0)

/**
 * File-level and directory-level `{from, to}` pairs for every rename, longest `from` first.
 * A directory pair maps the old parent directory to the new one when the file name is unchanged.
 * @param {readonly (string | ChangedFile)[]} files @returns {RenamePair[]}
 */
export function renamePairs(files) {
  /** @type {Map<string, RenamePair>} */
  const pairs = new Map()
  const add = (/** @type {string} */ from, /** @type {string} */ to, directory = false) => {
    if (from && to && from !== '.' && to !== '.' && from !== to)
      pairs.set(`${from}\0${to}`, directory ? { from, to, directory: true } : { from, to })
  }
  for (const file of files) {
    if (typeof file === 'string' || file.status !== 'renamed' || !file.previousPath) continue
    add(stripModulePath(file.previousPath), stripModulePath(file.path))
    if (posix.basename(file.previousPath) === posix.basename(file.path))
      add(posix.dirname(file.previousPath), posix.dirname(file.path), true)
  }
  return [...pairs.values()].sort(
    (a, b) =>
      segments(b.from).length - segments(a.from).length ||
      compare(a.from, b.from) ||
      compare(a.to, b.to),
  )
}

/** @param {string} line @param {RegExp} pattern @returns {ImportLine | null} */
function quoted(line, pattern) {
  const match = pattern.exec(line)
  if (!match?.groups) return null
  const { spec } = match.groups
  const start = match.index + match[0].lastIndexOf(match.groups.q + spec + match.groups.q) + 1
  return { template: line.slice(0, start) + SPEC + line.slice(start + spec.length), spec }
}

const JS_LINES = [
  /^\s*(?:import|export)\b[^'"]*?\bfrom\s*(?<q>['"])(?<spec>[^'"\n]+)\k<q>\s*;?\s*$/,
  /^\s*\}\s*from\s*(?<q>['"])(?<spec>[^'"\n]+)\k<q>\s*;?\s*$/,
  /^\s*import\s*(?<q>['"])(?<spec>[^'"\n]+)\k<q>\s*;?\s*$/,
  /^\s*(?:(?:const|let|var)\s+[\w${}\s,:]+=\s*)?(?:await\s+)?(?:require|import)\(\s*(?<q>['"])(?<spec>[^'"\n]+)\k<q>\s*\)\s*;?\s*$/,
]
// A bare string line is only an import spec in Go; statement keywords are excluded as aliases.
const GO_LINES = [
  /^\s*(?:import\s+)?(?:(?!(?:return|case|go|defer|goto|else|var|const|type|package|func)\b)[A-Za-z_]\w*\s+|\.\s+)?(?<q>")(?<spec>[^"\n]+)\k<q>\s*$/,
]
const PY_LINES = [
  /^(\s*import\s+)([\w.]+)((?:\s+as\s+\w+)?\s*)$/,
  /^(\s*from\s+)([\w.]+)(\s+import\s+\S.*)$/,
]

/** Python dotted paths read as slash paths; a relative (leading-dot) one only matches itself.
 * @param {string} line @returns {ImportLine | null} */
function pythonLine(line) {
  const match = PY_LINES.map((re) => re.exec(line)).find(Boolean)
  if (!match) return null
  const spec = match[2].startsWith('.') ? `\u0001${match[2]}` : match[2].replace(/\./g, '/')
  return { template: match[1] + SPEC + match[3], spec }
}

// Imports that bind nothing run for their effects, so their relative order is behaviour.
const JS_SIDE_EFFECT = /^\s*(?:import\s*['"]|(?:await\s+)?(?:require|import)\()/
const GO_SIDE_EFFECT = /^\s*(?:import\s+)?_\s+"/

/** @param {RegExp[]} lines @param {RegExp} sideEffect
 * @returns {(line: string) => ImportLine | null} */
const quotedGrammar = (lines, sideEffect) => (line) => {
  const parsed = lines.map((re) => quoted(line, re)).find(Boolean)
  if (!parsed) return null
  return sideEffect.test(line) ? { ...parsed, sideEffect: true } : parsed
}

/** @param {string} file @returns {((line: string) => ImportLine | null) | null} */
function grammar(file) {
  if (JS.test(file)) return quotedGrammar(JS_LINES, JS_SIDE_EFFECT)
  if (file.endsWith('.go')) return quotedGrammar(GO_LINES, GO_SIDE_EFFECT)
  if (file.endsWith('.py')) return pythonLine
  return null
}

/**
 * The removed and added import lines of a unified-diff patch, overall and per hunk (hunks with no
 * non-blank changed line are omitted), or `null` when the language is unsupported or any changed
 * non-blank line is not an import line.
 * @param {string} file @param {string | undefined} patch @returns {ImportDiff | null}
 */
export function importLines(file, patch) {
  if (typeof patch !== 'string') return null
  const parse = grammar(file)
  /** @type {ImportDiff} */
  const result = { removed: [], added: [], hunks: [] }
  /** @type {ImportHunk | null} */
  let hunk = null
  let inHunks = false
  for (const line of patch.split('\n')) {
    if (line.startsWith('@@')) {
      inHunks = true
      hunk = null
      continue
    }
    if (!/^[+-]/.test(line) || (!inHunks && /^(?:---|\+\+\+) /.test(line))) continue
    if (line.slice(1).trim() === '') continue
    const parsed = parse?.(line.slice(1))
    if (!parsed) return null
    if (!hunk) result.hunks.push((hunk = { removed: [], added: [] }))
    const side = line[0] === '-' ? 'removed' : 'added'
    result[side].push(parsed)
    hunk[side].push(parsed)
  }
  return result
}

/** Map a repo module path through the file-level rename of exactly that path.
 * @param {string} target @param {RenamePair[]} pairs */
function mapThrough(target, pairs) {
  return pairs.find((pair) => !pair.directory && pair.from === target)?.to ?? target
}

/** @param {string} spec */
const extensionOf = (spec) => {
  const base = posix.basename(spec)
  const dot = base.lastIndexOf('.')
  return dot > 0 ? base.slice(dot) : ''
}

/**
 * Every rewrite of `spec` whose trailing segments are a suffix of `from`, replaced by the matching
 * suffix of `to`; the part of `from` the specifier does not show must be unchanged in `to`.
 * @param {string[]} spec @param {string[]} from @param {string[]} to @returns {string[][]}
 */
function moduleTranslations(spec, from, to) {
  const out = []
  for (let take = Math.min(2, from.length); take <= from.length; take += 1) {
    const hidden = from.length - take
    if (to.length < hidden || from.slice(0, hidden).join('/') !== to.slice(0, hidden).join('/'))
      continue
    const at = spec.length - take
    if (at >= 0 && spec.slice(at).join('/') === from.slice(hidden).join('/'))
      out.push([...spec.slice(0, at), ...to.slice(hidden)])
  }
  return out
}

/** @param {string} target */
const isIndex = (target) => /\/(?:index|__init__)(?:\.[^/]*)?$/.test(target)

/**
 * How the removed specifier becomes the added one: `'renamed'` when a rename in `pairs`
 * explains a new target, `'same'` when both name the same target, `false` otherwise.
 * @param {string} removed @param {string} added @param {ChangedFile} file @param {RenamePair[]} pairs
 * @returns {'renamed' | 'same' | false}
 */
function specMatches(removed, added, file, pairs) {
  // A relative target is a file: only a rename of that file explains a new target.
  if (removed.startsWith('.') && added.startsWith('.')) {
    if (extensionOf(removed) !== extensionOf(added)) return false
    const oldPath = posix.normalize(
      posix.join(posix.dirname(file.previousPath ?? file.path), removed),
    )
    const newPath = posix.normalize(posix.join(posix.dirname(file.path), added))
    const oldTarget = stripModulePath(oldPath)
    const mapped = mapThrough(oldTarget, pairs)
    if (mapped !== stripModulePath(newPath)) return false
    if (mapped !== oldTarget) return 'renamed'
    // Same stripped target, but `./a.js` and `./a/index.js` are different files.
    if (isIndex(oldPath) !== isIndex(newPath)) return false
    // A moved importer's own rename explains a rewritten specifier for the same target.
    const importerMoved =
      file.previousPath !== undefined &&
      posix.dirname(file.previousPath) !== posix.dirname(file.path)
    return importerMoved && removed !== added ? 'renamed' : 'same'
  }
  if (removed === added) return 'same'
  // A module specifier may name a directory (a Go package, a Python package), so directory
  // renames explain it too.
  if (removed.startsWith('.') || added.startsWith('.')) return false
  const spec = segments(removed)
  return pairs.some(({ from, to }) =>
    moduleTranslations(spec, segments(from), segments(to)).some(
      (candidate) => candidate.join('/') === segments(added).join('/'),
    ),
  )
    ? 'renamed'
    : false
}

/** @param {ImportLine[]} list */
const kinds = (list) => list.map((line) => (line.sideEffect ? 'e' : 'b')).join('')

/**
 * Whether one hunk only rewrites imports in place: as many import lines removed as added, the same
 * interleaving of binding and side-effect imports, side-effect imports matched in order and each
 * rename-translated, binding imports matched as a multiset (so re-sorting within the hunk is
 * allowed). `'renamed'` when a rename translates one of its specifiers, `'same'` when none does.
 * @param {ImportHunk} hunk @param {ChangedFile} file @param {RenamePair[]} pairs
 * @returns {'renamed' | 'same' | false}
 */
function pureHunk({ removed, added }, file, pairs) {
  if (removed.length !== added.length || kinds(removed) !== kinds(added)) return false
  const matches = (/** @type {ImportLine} */ from, /** @type {ImportLine} */ to) =>
    to.template === from.template ? specMatches(from.spec, to.spec, file, pairs) : false
  const addedEffects = added.filter((line) => line.sideEffect)
  const removedEffects = removed.filter((line) => line.sideEffect)
  if (removedEffects.some((line, index) => matches(line, addedEffects[index]) !== 'renamed'))
    return false
  const byLine = (/** @type {ImportLine} */ a, /** @type {ImportLine} */ b) =>
    compare(a.template, b.template) || compare(a.spec, b.spec)
  const unused = added.filter((line) => !line.sideEffect).sort(byLine)
  let translated = removedEffects.length > 0
  for (const line of removed.filter((entry) => !entry.sideEffect).sort(byLine)) {
    const index = unused.findIndex((candidate) => matches(line, candidate))
    if (index < 0) return false
    translated ||= matches(line, unused[index]) === 'renamed'
    unused.splice(index, 1)
  }
  return translated ? 'renamed' : 'same'
}

/**
 * True when every changed line is an import whose specifier change a rename explains, rewritten
 * within its own hunk. With `-U0` an import that moves past an unchanged line shows as a `-` in
 * one hunk and a `+` in another, so an unbalanced hunk is a reorder, not a rewrite. A renamed file
 * with no changed lines is pure; a file that is not itself renamed is pure only when a rename
 * translates at least one of its specifiers.
 * @param {ChangedFile} file @param {RenamePair[]} pairs
 */
export function isPureMoveFile(file, pairs) {
  if (file.status !== 'renamed' && file.status !== 'modified') return false
  const lines = importLines(file.path, file.patch)
  if (!lines) return false
  let translated = false
  for (const hunk of lines.hunks) {
    const result = pureHunk(hunk, file, pairs)
    if (!result) return false
    translated ||= result === 'renamed'
  }
  return file.status === 'renamed' || translated
}

/**
 * Summarise a set of changed files: `moved` renames, `importOnly` files whose only changed lines
 * are rename-explained imports, `other` files that are not pure moves.
 * @param {readonly ChangedFile[]} files @param {RenamePair[]} [pairs]
 */
export function pureMoveSummary(files, pairs = renamePairs(files)) {
  let moved = 0
  let importOnly = 0
  let other = 0
  for (const file of files) {
    const pure = (file.status === 'renamed' && file.changes === 0) || isPureMoveFile(file, pairs)
    const lines = pure ? importLines(file.path, file.patch) : null
    if (file.status === 'renamed') moved += 1
    if (!pure) other += 1
    else if (lines && lines.removed.length + lines.added.length > 0) importOnly += 1
  }
  return { pure: files.length > 0 && other === 0, moved, importOnly, other }
}
