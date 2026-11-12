/**
 * Git-quoted snippets: the page shows code only as `git diff base..head` prints it. A spec names
 * a head-side line range; the rows come from git, never from the spec.
 */
import { spawnSync } from 'node:child_process'

export const SNIPPET_CONTEXT = 100_000
/** A diff excerpt line longer than this is cut, its last character an ellipsis. */
export const MAX_LINE_CHARS = 200
// Paths are literal, never globs.
const GIT_FLAGS = Object.freeze(['--literal-pathspecs'])

/**
 * @typedef {{kind: 'add' | 'del' | 'ctx', old: number | null, new: number | null, text: string, cut?: boolean}} DiffRow
 * @typedef {{path: string, rows: DiffRow[], adds: number, dels: number}} FileDiff
 * @typedef {{path: string, start: number, end: number, pinLine?: number}} SnippetRequest
 * @typedef {{path: string, start: number, end: number, rows: DiffRow[], adds: number, dels: number, pinLine: number | null}} Snippet
 * @typedef {{snippet: Snippet | null, problems: string[]}} SnippetResult
 * @typedef {(request: SnippetRequest) => SnippetResult} SnippetResolver
 */

const ESCAPES = Object.freeze(
  /** @type {Record<string, number>} */ ({ a: 7, b: 8, t: 9, n: 10, v: 11, f: 12, r: 13 }),
)

/** A path as git writes it in a diff header: C-quoted when it has special bytes, and followed by a
 * tab when it has a space. @param {string} raw */
function unquotePath(raw) {
  const text = raw.replace(/\t$/, '')
  const quoted = /^"(.*)"$/.exec(text)
  if (!quoted) return text
  /** @type {number[]} */
  const bytes = []
  const body = quoted[1]
  for (let i = 0; i < body.length; i++) {
    if (body[i] !== '\\') {
      bytes.push(...Buffer.from(body[i]))
      continue
    }
    const octal = /^[0-7]{3}/.exec(body.slice(i + 1))
    if (octal) {
      bytes.push(parseInt(octal[0], 8))
      i += 3
    } else {
      const next = body[++i] ?? ''
      bytes.push(...(ESCAPES[next] === undefined ? Buffer.from(next) : [ESCAPES[next]]))
    }
  }
  return Buffer.from(bytes).toString('utf8')
}

/** The path a `--- a/path` or `+++ b/path` header names, or null for `/dev/null`.
 * @param {string} line */
const headerPath = (line) => {
  const raw = unquotePath(line.slice(4))
  return raw === '/dev/null' ? null : raw.replace(/^[ab]\//, '')
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
  /** @type {string | null} */
  let oldPath = null
  for (const line of text.replace(/\n$/, '').split('\n')) {
    if (line.startsWith('diff --git ')) {
      file = null
      inHunk = false
      oldPath = null
      continue
    }
    if (!inHunk && /^--- /.test(line)) {
      oldPath = headerPath(line)
      continue
    }
    if (!inHunk && /^\+\+\+ /.test(line)) {
      // A deleted file's head side is /dev/null, so its name comes from the base side.
      file = { path: headerPath(line) ?? oldPath ?? '', rows: [], adds: 0, dels: 0 }
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
 * Each path's whole-file diff, `git diff -U<large> base...head -- path` in `cwd`, read once.
 * `run(args)` returns git's stdout, or null when git fails; it defaults to spawning git.
 * @param {{base: string, head: string, cwd?: string, run?: (args: string[]) => string | null}} options
 * @returns {(path: string) => FileDiff | undefined}
 */
export function gitFileDiffs({ base, head, cwd, run }) {
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
  return (path) => {
    if (!cache.has(path)) {
      const text = git([
        'diff',
        `-U${SNIPPET_CONTEXT}`,
        '--no-color',
        '--no-ext-diff',
        `${base}...${head}`,
        '--',
        path,
      ])
      cache.set(path, text === null ? undefined : parseUnifiedDiff(text)[0])
    }
    return cache.get(path)
  }
}

/** A resolver that quotes each snippet from `diffs`, as `gitFileDiffs` reads them.
 * @param {(path: string) => FileDiff | undefined} diffs @returns {SnippetResolver} */
export const snippetResolver = (diffs) => (request) => cutSnippet(diffs(request.path), request)

/**
 * Whether `line` exists in `path` at head, from its whole-file diff; a deleted file's lines are
 * its base lines. False only when the diff shows the file whole: one run of lines from line 1,
 * ending before the `SNIPPET_CONTEXT` window does. Null when the line is not proved either way:
 * the diff cannot be read, has no rows (a mode-only or binary change), or may not reach the line.
 * @param {(path: string) => FileDiff | undefined} diffs
 * @returns {(path: string, line: number) => boolean | null}
 */
export const lineChecker = (diffs) => (path, line) => {
  const diff = diffs(path)
  if (!diff) return null
  const side = diff.rows.some((row) => row.new !== null) ? 'new' : 'old'
  const lines = diff.rows.flatMap((row) => (row[side] === null ? [] : [row[side]]))
  if (lines.includes(line)) return true
  let trailing = 0
  while (trailing < diff.rows.length && diff.rows[diff.rows.length - 1 - trailing].kind === 'ctx')
    trailing++
  const whole = lines.length > 0 && lines.every((n, i) => n === i + 1) && trailing < SNIPPET_CONTEXT
  return whole ? false : null
}

/** @param {string[]} args @param {{cwd?: string, run?: (args: string[]) => string | null}} options */
function gitOutput(args, { cwd, run }) {
  if (run) return run(args)
  const out = spawnSync('git', [...GIT_FLAGS, ...args], {
    cwd,
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
  })
  return out.status === 0 ? out.stdout : null
}

/**
 * The commit `base...head` diffs from, `git merge-base base head`, or null when git fails.
 * `run(args)` is as for `gitFileDiffs`.
 * @param {{base: string, head: string, cwd?: string, run?: (args: string[]) => string | null}} options
 */
export function gitMergeBase({ base, head, cwd, run }) {
  return gitOutput(['merge-base', base, head], { cwd, run })?.trim() || null
}

/**
 * The files `base...head` changes with their added and deleted line counts (null for binary), from
 * `git diff --numstat`, or null when git fails. `run(args)` is as for `gitFileDiffs`.
 * @param {{base: string, head: string, cwd?: string, run?: (args: string[]) => string | null}} options
 * @returns {{path: string, adds: number | null, dels: number | null}[] | null}
 */
export function gitChangedFiles({ base, head, cwd, run }) {
  const text = gitOutput(['diff', '--numstat', '-z', '--no-renames', `${base}...${head}`], {
    cwd,
    run,
  })
  if (text === null) return null
  const count = (/** @type {string} */ n) => (n === '-' ? null : Number(n))
  return text
    .split('\0')
    .filter(Boolean)
    .map((entry) => {
      const [adds, dels, ...path] = entry.split('\t')
      return { path: path.join('\t'), adds: count(adds), dels: count(dels) }
    })
}
