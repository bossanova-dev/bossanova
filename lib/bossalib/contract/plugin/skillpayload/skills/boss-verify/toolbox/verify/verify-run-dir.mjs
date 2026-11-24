import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { PIN_TTL_MS, pinRun } from '../skill-pin.mjs'

const RUN_DIR_PARENT_PREFIX = 'boss-verify-run-'
const MISSING_RUN_DIR_ERROR = 'BLOCKED: boss-verify run dir missing - re-run the first block'
const UNTRUSTED_PARENT_ERROR = 'BLOCKED: boss-verify run dir parent is untrusted'
const UNTRUSTED_RUN_DIR_ERROR = 'BLOCKED: boss-verify run dir is untrusted'

/** @param {string} target @returns {fs.Stats | null} */
function lstatOrNull(target) {
  try {
    return fs.lstatSync(target)
  } catch (error) {
    if (/** @type {NodeJS.ErrnoException} */ (error)?.code === 'ENOENT') return null
    throw error
  }
}

/** @param {fs.Stats} st @returns {boolean} */
function isPrivateOwnedDir(st) {
  if (st.isSymbolicLink() || !st.isDirectory()) return false
  if (typeof process.getuid === 'function') {
    if (st.uid !== process.getuid()) return false
    if ((st.mode & 0o077) !== 0) return false
  }
  return true
}

/** @param {string} parent */
function ensureTrustedParent(parent) {
  try {
    fs.mkdirSync(parent, { mode: 0o700 })
  } catch (error) {
    if (/** @type {NodeJS.ErrnoException} */ (error)?.code !== 'EEXIST') {
      throw error
    }
  }
  const st = lstatOrNull(parent)
  if (!st || !isPrivateOwnedDir(st)) {
    throw new Error(UNTRUSTED_PARENT_ERROR)
  }
}

/** @param {string} dir */
function removeTree(dir) {
  fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
}

/** @param {string} parent @param {string} keep @param {number} now */
function sweepStaleRunDirs(parent, keep, now) {
  const cutoff = now - PIN_TTL_MS
  let names = []
  try {
    names = fs.readdirSync(parent)
  } catch {
    return
  }
  for (const name of names) {
    const entry = path.join(parent, name)
    try {
      if (entry !== keep && fs.lstatSync(entry).mtimeMs < cutoff) removeTree(entry)
    } catch {
      // A peer run removed it first, or it is not ours to remove; reaping never fails creation.
    }
  }
}

/**
 * @param {string} [tmpRoot]
 * @returns {string}
 */
export function runDirParent(tmpRoot = os.tmpdir()) {
  const uid = typeof process.getuid === 'function' ? process.getuid() : 'user'
  return path.join(fs.realpathSync(tmpRoot), `${RUN_DIR_PARENT_PREFIX}${uid}`)
}

/**
 * @param {{gitDir: string, env?: NodeJS.ProcessEnv, tmpRoot?: string}} options
 * @returns {string}
 */
export function runDirPath({ gitDir, env = process.env, tmpRoot = os.tmpdir() }) {
  if (typeof gitDir !== 'string' || !gitDir.trim()) {
    throw new Error('BLOCKED: gitDir is required')
  }
  const run = pinRun(env)
  const key = createHash('sha256').update(path.resolve(gitDir)).digest('hex').slice(0, 12)
  return path.join(runDirParent(tmpRoot), `${key}-${run}`)
}

/**
 * @param {{gitDir: string, env?: NodeJS.ProcessEnv, tmpRoot?: string, now?: number}} options
 * @returns {string}
 */
export function createRunDir({
  gitDir,
  env = process.env,
  tmpRoot = os.tmpdir(),
  now = Date.now(),
}) {
  const dir = runDirPath({ gitDir, env, tmpRoot })
  const parent = path.dirname(dir)
  ensureTrustedParent(parent)
  removeTree(dir)
  fs.mkdirSync(dir, { mode: 0o700 })
  fs.chmodSync(dir, 0o700)
  sweepStaleRunDirs(parent, dir, now)
  return dir
}

/**
 * @param {{gitDir: string, env?: NodeJS.ProcessEnv, tmpRoot?: string}} options
 * @returns {string}
 */
export function resolveRunDir({ gitDir, env = process.env, tmpRoot = os.tmpdir() }) {
  const dir = runDirPath({ gitDir, env, tmpRoot })
  const parent = path.dirname(dir)
  const parentStat = lstatOrNull(parent)
  if (!parentStat) throw new Error(MISSING_RUN_DIR_ERROR)
  if (!isPrivateOwnedDir(parentStat)) throw new Error(UNTRUSTED_PARENT_ERROR)
  const dirStat = lstatOrNull(dir)
  if (!dirStat || dirStat.isSymbolicLink() || !dirStat.isDirectory()) {
    throw new Error(MISSING_RUN_DIR_ERROR)
  }
  if (!isPrivateOwnedDir(dirStat)) throw new Error(UNTRUSTED_RUN_DIR_ERROR)
  return dir
}

/**
 * @param {{gitDir: string, env?: NodeJS.ProcessEnv, tmpRoot?: string}} options
 * @returns {string}
 */
export function removeRunDir({ gitDir, env = process.env, tmpRoot = os.tmpdir() }) {
  const dir = runDirPath({ gitDir, env, tmpRoot })
  const parent = path.dirname(dir)
  const parentStat = lstatOrNull(parent)
  if (!parentStat) return dir
  if (!isPrivateOwnedDir(parentStat)) throw new Error(UNTRUSTED_PARENT_ERROR)
  removeTree(dir)
  return dir
}

/** @param {string[]} [argv] @param {NodeJS.ProcessEnv} [env] */
function main(argv = process.argv.slice(2), env = process.env) {
  const { values, positionals } = parseArgs({
    args: argv,
    strict: true,
    allowPositionals: true,
    options: {
      'git-dir': { type: 'string' },
      'tmp-root': { type: 'string' },
    },
  })
  const [cmd] = positionals
  if (positionals.length !== 1 || !['create', 'path', 'remove'].includes(cmd)) {
    throw new Error(
      'usage: verify-run-dir.mjs create|path|remove [--git-dir <dir>] [--tmp-root <dir>]',
    )
  }
  const gitDir =
    values['git-dir'] ||
    execFileSync('git', ['rev-parse', '--absolute-git-dir'], { encoding: 'utf8' }).trim()
  const tmpRoot = values['tmp-root'] || os.tmpdir()
  const options = { gitDir, env, tmpRoot }
  if (cmd === 'create') console.log(createRunDir(options))
  else if (cmd === 'path') console.log(resolveRunDir(options))
  else if (cmd === 'remove') removeRunDir(options)
}

if (isMainModule(import.meta.url)) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
