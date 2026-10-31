// Read a PR's changed files with rename detection, in the pull-files shape the area detector and
// the proof describers read: `{path, previousPath?, status, changes?, patch?}`. All git access goes
// through an injected spawnSync-shaped `run`.

import { spawnSync } from 'node:child_process'

/**
 * @typedef {import('./verify-moves.mjs').ChangedFile} ChangedFile
 * @typedef {(command: string, args: string[], options: object) =>
 *   {status: number | null, stdout?: string | Buffer, stderr?: string | Buffer, error?: Error}} Run
 * @typedef {{ok: boolean, status: number | null, stdout: string, stderr: string, code?: string}} RunResult
 */

/** Per-call bound, well under the describer's own 60-second budget. */
export const TOOL_TIMEOUT_MS = 20_000
export const TOOL_MAX_BUFFER = 64 * 1024 * 1024

const STATUS = /** @type {const} */ ({ A: 'added', M: 'modified', D: 'removed', T: 'modified' })

/**
 * Run a tool through `run`, never throwing.
 * @param {Run} run @param {string} command @param {string[]} args
 * @param {{cwd?: string, input?: string, timeout?: number, maxBuffer?: number}} [options]
 * @returns {RunResult}
 */
export function runTool(run, command, args, options = {}) {
  try {
    const out = run(command, args, {
      cwd: options.cwd,
      input: options.input,
      encoding: 'utf8',
      timeout: options.timeout ?? TOOL_TIMEOUT_MS,
      maxBuffer: options.maxBuffer ?? TOOL_MAX_BUFFER,
      stdio: [options.input === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
    })
    const code = out.error && 'code' in out.error ? String(out.error.code) : undefined
    return {
      ok: out.status === 0 && !out.error,
      status: out.status ?? null,
      stdout: String(out.stdout ?? ''),
      stderr: String(out.stderr ?? ''),
      ...(code ? { code } : {}),
    }
  } catch (error) {
    return { ok: false, status: null, stdout: '', stderr: String(error), code: 'EXCEPTION' }
  }
}

/** Split `git diff` patch output into one chunk per `diff --git` header. @param {string} text */
function patchChunks(text) {
  const chunks = []
  for (const line of text.split('\n')) {
    if (line.startsWith('diff --git ')) chunks.push([])
    chunks.at(-1)?.push(line)
  }
  return chunks
}

/** The hunk lines of one chunk (from the first `@@`), or `undefined` for a binary file.
 * @param {string[]} chunk */
function hunks(chunk) {
  const first = chunk.findIndex((line) => line.startsWith('@@'))
  if (first < 0)
    return chunk.some((line) => /^Binary files |^GIT binary patch/.test(line)) ? undefined : ''
  return chunk.slice(first).join('\n').replace(/\n$/, '')
}

/**
 * The PR's changed files between the merge base of `baseSha` and `headSha`. Throws when git fails.
 * @param {{baseSha: string, headSha: string, cwd?: string, run?: Run}} options
 * @returns {ChangedFile[]}
 */
export function readChangedFiles({ baseSha, headSha, cwd, run = spawnSync }) {
  const range = `${baseSha}...${headSha}`
  const common = [
    '-c',
    'core.quotePath=false',
    'diff',
    '-M',
    '--no-color',
    '--no-ext-diff',
    '--no-textconv',
  ]
  const names = runTool(run, 'git', [...common, '--name-status', '-z', range, '--'], { cwd })
  if (!names.ok) throw new Error(`git diff --name-status failed: ${names.code ?? names.status}`)
  const patches = runTool(run, 'git', [...common, '-U0', range, '--'], { cwd })
  if (!patches.ok) throw new Error(`git diff -U0 failed: ${patches.code ?? patches.status}`)
  const fields = names.stdout.split('\0')
  if (fields.at(-1) === '') fields.pop()
  /** @type {(ChangedFile & {score?: number})[]} */
  const files = []
  for (let i = 0; i < fields.length;) {
    const code = fields[i++]
    const letter = code[0]
    if (letter === 'R' || letter === 'C') {
      const previousPath = fields[i++]
      const path = fields[i++]
      files.push({ path, previousPath, status: 'renamed', score: Number(code.slice(1)) })
    } else {
      files.push({ path: fields[i++], status: STATUS[/** @type {'A'} */ (letter)] ?? 'modified' })
    }
  }
  const chunks = patchChunks(patches.stdout)
  // A type change splits into two patches; with no one-to-one pairing every patch is unknown.
  const paired = chunks.length === files.length
  return files.map(({ score, ...file }, index) => {
    const patch = paired ? hunks(chunks[index]) : undefined
    if (typeof patch === 'string') {
      const changed = patch.split('\n').filter((line) => /^[+-]/.test(line)).length
      return { ...file, changes: changed, patch }
    }
    return file.status === 'renamed' && score === 100 ? { ...file, changes: 0 } : file
  })
}

/**
 * Keep the changed files a path list names (by path or previous path), and add every listed path
 * no file covers as a plain string, so the result never drops a path the list carries.
 * @param {readonly ChangedFile[]} files @param {readonly string[]} paths
 * @returns {(ChangedFile | string)[]}
 */
export function scopeChangedFiles(files, paths) {
  const wanted = new Set(paths)
  const kept = files.filter(
    (file) =>
      wanted.has(file.path) || (file.previousPath !== undefined && wanted.has(file.previousPath)),
  )
  const covered = new Set(kept.flatMap((file) => [file.path, file.previousPath ?? file.path]))
  return [...kept, ...[...wanted].filter((path) => !covered.has(path))]
}
