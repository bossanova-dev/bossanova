/**
 * Git-quoted snippets: the page shows code only as `git diff base..head` prints it. A spec names
 * a head-side line range; the rows come from git, never from the spec.
 */
import { spawnSync } from 'node:child_process'

export const SNIPPET_CONTEXT = 100_000
// Paths are literal, never globs.
const GIT_FLAGS = Object.freeze(['--literal-pathspecs'])

/**
 * @typedef {{kind: 'add' | 'del' | 'ctx', old: number | null, new: number | null, text: string}} DiffRow
 * @typedef {{path: string, rows: DiffRow[], adds: number, dels: number}} FileDiff
 * @typedef {{path: string, start: number, end: number, pinLine?: number}} SnippetRequest
 * @typedef {{path: string, start: number, end: number, rows: DiffRow[], adds: number, dels: number, pinLine: number | null}} Snippet
 * @typedef {{snippet: Snippet | null, problems: string[]}} SnippetResult
 * @typedef {(request: SnippetRequest) => SnippetResult} SnippetResolver
 */

/** `b/path` from a `+++ b/path` line, or null for `/dev/null`. @param {string} line */
const headPath = (line) => {
  const raw = line.slice(4).replace(/^"(.*)"$/, '$1')
  return raw === '/dev/null' ? null : raw.replace(/^b\//, '')
}

/**
 * Parse unified diff text into per-file rows. Text with no file header parses as one unnamed file
 * when it has hunks, and as no files when it has none.
 * @param {string} text @returns {FileDiff[]}
 */
export function parseUnifiedDiff(text) {
  /** @type {FileDiff[]} */
  const files = []
  /** @type {FileDiff | null} */
  let file = null
  let oldLine = 0
  let newLine = 0
  let inHunk = false
  for (const line of text.replace(/\n$/, '').split('\n')) {
    if (line.startsWith('diff --git ')) {
      file = null
      inHunk = false
      continue
    }
    if (!inHunk && /^--- /.test(line)) continue
    if (!inHunk && /^\+\+\+ /.test(line)) {
      file = { path: headPath(line) ?? '', rows: [], adds: 0, dels: 0 }
      files.push(file)
      continue
    }
    const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line)
    if (hunk) {
      if (!file) {
        file = { path: '', rows: [], adds: 0, dels: 0 }
        files.push(file)
      }
      oldLine = Number(hunk[1])
      newLine = Number(hunk[2])
      inHunk = true
      continue
    }
    if (!inHunk || !file || line.startsWith('\\')) continue
    const body = line.slice(1)
    if (line.startsWith('+')) {
      file.rows.push({ kind: 'add', old: null, new: newLine++, text: body })
      file.adds++
    } else if (line.startsWith('-')) {
      file.rows.push({ kind: 'del', old: oldLine++, new: null, text: body })
      file.dels++
    } else if (line.startsWith(' ') || line === '') {
      // diff.suppressBlankEmpty prints a blank context line without its space.
      file.rows.push({ kind: 'ctx', old: oldLine++, new: newLine++, text: body })
    } else {
      inHunk = false
    }
  }
  return files
}

/**
 * The rows of `diff` whose head-side position is inside `start..end`, with the problems that drop
 * the snippet (range outside the diff, no changed line) or only its pin (pin outside the range).
 * A deleted row sits at the head position of the line that follows it.
 * @param {FileDiff | undefined} diff @param {SnippetRequest} request @returns {SnippetResult}
 */
export function cutSnippet(diff, { path, start, end, pinLine }) {
  const at = `${path}:${start}-${end}`
  if (!diff || diff.rows.length === 0)
    return { snippet: null, problems: [`${at}: ${path} is not in the diff, snippet dropped`] }
  /** @type {DiffRow[]} */
  const rows = []
  const seen = new Set()
  let previous = 0
  for (const row of diff.rows) {
    const position = row.new ?? previous + 1
    if (row.new !== null) previous = row.new
    if (position < start || position > end) continue
    rows.push(row)
    if (row.new !== null) seen.add(row.new)
  }
  for (let line = start; line <= end; line++)
    if (!seen.has(line))
      return {
        snippet: null,
        problems: [`${at}: line ${line} is outside the diff, snippet dropped`],
      }
  if (!rows.some((row) => row.kind !== 'ctx'))
    return { snippet: null, problems: [`${at}: shows no changed line, snippet dropped`] }
  const problems = []
  let pin = pinLine ?? null
  if (pin !== null && (pin < start || pin > end)) {
    problems.push(`${at}: pin ${pin} is outside the snippet, pin dropped`)
    pin = null
  }
  return {
    snippet: { path, start, end, rows, adds: diff.adds, dels: diff.dels, pinLine: pin },
    problems,
  }
}

/**
 * A resolver that quotes each snippet from `git diff -U<large> base..head -- path` in `cwd`.
 * `run(args)` returns git's stdout, or null when git fails; it defaults to spawning git.
 * @param {{base: string, head: string, cwd?: string, run?: (args: string[]) => string | null}} options
 * @returns {SnippetResolver}
 */
export function gitSnippetResolver({ base, head, cwd, run }) {
  const git =
    run ??
    ((args) => {
      try {
        const out = spawnSync('git', [...GIT_FLAGS, ...args], {
          cwd,
          encoding: 'utf8',
          maxBuffer: 64 * 1024 * 1024,
        })
        return out.status === 0 ? out.stdout : null
      } catch {
        return null
      }
    })
  /** @type {Map<string, FileDiff | undefined>} */
  const cache = new Map()
  return (request) => {
    if (!cache.has(request.path)) {
      const text = git([
        'diff',
        `-U${SNIPPET_CONTEXT}`,
        '--no-color',
        '--no-ext-diff',
        `${base}..${head}`,
        '--',
        request.path,
      ])
      const file = text === null ? undefined : parseUnifiedDiff(text)[0]
      cache.set(request.path, file)
    }
    return cutSnippet(cache.get(request.path), request)
  }
}

/**
 * The files `base..head` changes with their added and deleted line counts (null for binary), from
 * `git diff --numstat`, or null when git fails.
 * @param {{base: string, head: string, cwd?: string}} options
 * @returns {{path: string, adds: number | null, dels: number | null}[] | null}
 */
export function gitChangedFiles({ base, head, cwd }) {
  const out = spawnSync(
    'git',
    [...GIT_FLAGS, 'diff', '--numstat', '-z', '--no-renames', `${base}..${head}`],
    { cwd, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  )
  if (out.status !== 0) return null
  const count = (/** @type {string} */ n) => (n === '-' ? null : Number(n))
  return out.stdout
    .split('\0')
    .filter(Boolean)
    .map((entry) => {
      const [adds, dels, ...path] = entry.split('\t')
      return { path: path.join('\t'), adds: count(adds), dels: count(dels) }
    })
}
