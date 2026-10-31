// Built-in proof-describe script for the api-contract area: runs `<buf> breaking` for each buf
// module holding a changed .proto, comparing the head module against the base one. A missing buf,
// a missing buf config or a buf usage error is reported as "not checked", never as a failure: the
// api-contract area already asks, so a missing tool cannot lower anything.

import { spawnSync } from 'node:child_process'
import { accessSync, constants, mkdirSync, mkdtempSync, rmSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

import { isMainModule } from '../main-module.mjs'
import { runTool } from './verify-diff.mjs'
import { describerMain } from './verify-describe.mjs'

/** @typedef {import('./verify-diff.mjs').Run} Run */

const BUF_NAME = 'buf'
const CONFIGS = ['buf.yaml', 'buf.work.yaml']
const MAX_VIOLATIONS = 5
/** buf's exit status when it found breaking changes. */
const BUF_FOUND = 100

/** @param {string} file */
function executable(file) {
  try {
    if (!statSync(file).isFile()) return false
    accessSync(file, constants.X_OK)
    return true
  } catch {
    return false
  }
}

/**
 * The buf binary: `BUF` when it names an executable file, else the first `buf` on `PATH`.
 * @param {NodeJS.ProcessEnv} env @returns {string | null}
 */
export function findBuf(env = process.env) {
  if (env.BUF && executable(env.BUF)) return env.BUF
  for (const dir of (env.PATH ?? '').split(path.delimiter).filter(Boolean)) {
    const candidate = path.join(dir, BUF_NAME)
    if (executable(candidate)) return candidate
  }
  return null
}

/** One line per buf JSON violation. @param {string} stdout */
function violations(stdout) {
  return stdout
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .flatMap((line) => {
      try {
        const entry = JSON.parse(line)
        const where = entry.path
          ? `${entry.path}${entry.start_line ? `:${entry.start_line}` : ''}: `
          : ''
        return [`${where}${entry.message ?? entry.type ?? 'breaking change'}`]
      } catch {
        return []
      }
    })
}

/**
 * @param {{area: {paths: string[]}, baseSha: string, headSha: string}} envelope
 * @param {{cwd?: string, run?: Run, env?: NodeJS.ProcessEnv, tmp?: string}} [options]
 */
export function describeProtos(
  envelope,
  { cwd, run = spawnSync, env = process.env, tmp = tmpdir() } = {},
) {
  const { baseSha, headSha } = envelope
  const notChecked = (/** @type {string} */ reason) => ({
    items: [`Proto compatibility not checked: ${reason}`],
    breaking: false,
  })
  const protos = envelope.area.paths.filter((file) => file.endsWith('.proto'))
  if (protos.length === 0) return { items: [], breaking: false }
  const buf = findBuf(env)
  if (!buf) return notChecked('no buf binary (set BUF or put buf on PATH)')
  /** @param {string} file */
  const inHead = (file) => runTool(run, 'git', ['cat-file', '-e', `${headSha}:${file}`], { cwd }).ok
  const modules = new Set()
  for (const proto of protos) {
    for (let dir = path.posix.dirname(proto); ; dir = path.posix.dirname(dir)) {
      const prefix = dir === '.' ? '' : `${dir}/`
      if (CONFIGS.some((config) => inHead(`${prefix}${config}`))) {
        modules.add(dir)
        break
      }
      if (dir === '.') break
    }
  }
  if (modules.size === 0) return notChecked('no buf.yaml or buf.work.yaml')
  const mergeBase = runTool(run, 'git', ['merge-base', baseSha, headSha], { cwd })
  const before = mergeBase.ok ? mergeBase.stdout.trim() : baseSha
  const work = mkdtempSync(path.join(tmp, 'proof-protos-'))
  try {
    /** @param {string} sha @param {string} side @param {string} dir */
    const extract = (sha, side, dir) => {
      const root = path.join(work, side)
      mkdirSync(root, { recursive: true })
      const tar = path.join(work, `${side}.tar`)
      const paths = dir === '.' ? [] : [dir]
      const archived = runTool(
        run,
        'git',
        ['archive', '--format=tar', '-o', tar, sha, '--', ...paths],
        { cwd },
      )
      if (!archived.ok) return null
      return runTool(run, 'tar', ['-xf', tar, '-C', root]).ok ? path.join(root, dir) : null
    }
    const found = []
    /** Modules that could not be compared; their reasons follow any violations found elsewhere. */
    const unchecked = []
    for (const [index, dir] of [...modules].sort().entries()) {
      const name = dir === '.' ? 'the root module' : dir
      const against = extract(before, `base-${index}`, dir)
      if (!against) {
        unchecked.push(`${name} is new`)
        continue
      }
      const input = extract(headSha, `head-${index}`, dir)
      if (!input) {
        unchecked.push(`cannot read ${dir} at head`)
        continue
      }
      const result = runTool(
        run,
        buf,
        ['breaking', input, '--against', against, '--error-format', 'json'],
        {
          cwd,
        },
      )
      if (result.ok) continue
      const listed = violations(result.stdout)
      if (result.status !== BUF_FOUND || listed.length === 0) {
        unchecked.push(`buf failed for ${name} (${result.code ?? `exit ${result.status}`})`)
        continue
      }
      found.push(...listed.map((line) => line.split(`${input}${path.sep}`).join('')))
    }
    const reasons = unchecked.map((reason) => notChecked(reason).items[0])
    if (found.length === 0)
      return reasons.length > 0
        ? { items: reasons, breaking: false }
        : { items: ['No breaking proto changes (buf)'], breaking: false }
    const n = found.length
    return {
      items: [
        `${n} breaking proto change${n === 1 ? '' : 's'}`,
        ...found.slice(0, MAX_VIOLATIONS),
        ...reasons,
      ],
      breaking: true,
    }
  } finally {
    rmSync(work, { recursive: true, force: true, maxRetries: 3 })
  }
}

export const main = () => describerMain(describeProtos)

if (isMainModule(import.meta.url)) await main()
